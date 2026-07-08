package diff

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompute(t *testing.T) {
	// Create a temporary git repo with two commits.
	dir := t.TempDir()
	ctx := context.Background()

	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run("git", "init")
	run("git", "config", "user.email", "test@test.com")
	run("git", "config", "user.name", "Test")

	f := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "hello.txt")
	run("git", "commit", "-m", "initial")

	base, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	baseSHA := string(base[:len(base)-1])

	if err := os.WriteFile(f, []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "hello.txt")
	run("git", "commit", "-m", "second")

	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	headSHA := string(head[:len(head)-1])

	// Change to temp dir so Compute runs git diff in the correct repo.
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)

	d, err := Compute(ctx, baseSHA, headSHA)
	if err != nil {
		t.Fatalf("Compute error: %v", err)
	}
	if d == "" {
		t.Error("expected non-empty diff")
	}
	if !contains(d, "+hello world") {
		t.Errorf("diff missing expected change: %s", d)
	}
}

func TestFilterDiff_exclusions(t *testing.T) {
	raw := `diff --git a/vendor/lib.go b/vendor/lib.go
+package vendor
diff --git a/src/main.go b/src/main.go
+package main
`
	filtered := filterDiff(raw)
	if contains(filtered, "vendor/lib.go") {
		t.Error("vendor path should be excluded")
	}
	if !contains(filtered, "src/main.go") {
		t.Error("src path should be included")
	}
}

// TestCompute_noWholeDiffTruncation guards against regressing to the old
// behavior: Compute must return a large diff in full and leave chunking to
// diff.ChunkFiles (via the caller), not truncate it itself. A whole-diff
// cutoff here silently drops files from every downstream chunk, not just
// the last one — see the comment on Compute for the full story.
func TestCompute_noWholeDiffTruncation(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run("git", "init")
	run("git", "config", "user.email", "test@test.com")
	run("git", "config", "user.name", "Test")

	// 600KB of changed content — comfortably larger than the old 100KB
	// whole-diff cutoff, to prove it's gone.
	f := filepath.Join(dir, "large.txt")
	data := make([]byte, 600000)
	for i := range data {
		data[i] = 'a'
	}
	if err := os.WriteFile(f, data, 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "large.txt")
	run("git", "commit", "-m", "initial")

	base, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	baseSHA := string(base[:len(base)-1])

	for i := range data {
		data[i] = 'b'
	}
	if err := os.WriteFile(f, data, 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "large.txt")
	run("git", "commit", "-m", "second")

	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	headSHA := string(head[:len(head)-1])

	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)

	d, err := Compute(ctx, baseSHA, headSHA)
	if err != nil {
		t.Fatalf("Compute error: %v", err)
	}
	if len(d) < 1_000_000 {
		t.Errorf("expected the full ~1.2MB diff (both old and new 600KB sides), got %d bytes — looks truncated", len(d))
	}
	if strings.Contains(d, "[diff truncated") {
		t.Error("Compute should never truncate; that's ChunkFiles' job now")
	}
	if !strings.Contains(d, "-"+strings.Repeat("a", 100)) || !strings.Contains(d, "+"+strings.Repeat("b", 100)) {
		t.Error("diff missing content from both sides — looks truncated mid-file")
	}
}

func TestSplitFiles(t *testing.T) {
	raw := `diff --git a/file1.go b/file1.go
index 123..456 100644
--- a/file1.go
+++ b/file1.go
@@ -1 +1 @@
-old
+new1
diff --git a/file2.go b/file2.go
index 789..abc 100644
--- a/file2.go
+++ b/file2.go
@@ -1 +1 @@
-old
+new2
`
	files := SplitFiles(raw)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if !strings.Contains(files[0], "file1.go") {
		t.Error("first chunk missing file1.go")
	}
	if !strings.Contains(files[1], "file2.go") {
		t.Error("second chunk missing file2.go")
	}
}

func TestChunkFiles(t *testing.T) {
	files := []string{
		"diff --git a/small.go b/small.go\n+small\n",
		strings.Repeat("x", 40000),
		"diff --git a/medium.go b/medium.go\n+medium\n",
	}

	chunks := ChunkFiles(files, 30000)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	// Verify each chunk is within limit
	for i, c := range chunks {
		var size int
		for _, f := range c {
			size += len(f)
		}
		if size > 30000 {
			t.Errorf("chunk %d exceeds maxSize: %d bytes", i, size)
		}
	}

	// Verify all files are present
	var totalFiles int
	for _, c := range chunks {
		totalFiles += len(c)
	}
	if totalFiles != len(files) {
		t.Errorf("expected %d total files across chunks, got %d", len(files), totalFiles)
	}
}

func TestChunkFiles_singleFileTooLarge(t *testing.T) {
	files := []string{
		strings.Repeat("x", 60000),
	}

	chunks := ChunkFiles(files, 50000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if len(chunks[0]) != 1 {
		t.Fatalf("expected 1 file in chunk, got %d", len(chunks[0]))
	}
	if !strings.Contains(chunks[0][0], "[file truncated]") {
		t.Error("expected truncation marker for oversized file")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstr(s, substr))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestExcluded(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"vendor/lib/a.go", true},
		{"pkg/vendor/lib/a.go", true},
		{"myvendor/a.go", false},
		{"dist/bundle.js", true},
		{"redist/bundle.js", false},
		{"Cargo.lock", true},
		{"pkg/Gemfile.lock", true},
		{"locksmith.go", false},
		{"api/service.pb.go", true},
		{"types.gen.go", true},
		{"generator.go", false},
		{"go.sum", true},
		{"sub/module/go.sum", true},
		{"PR-REVIEW.md", true},
		{"main.go", false},
	}
	for _, tc := range cases {
		if got := excluded(tc.path); got != tc.want {
			t.Errorf("excluded(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestDiffFilePath(t *testing.T) {
	if got := diffFilePath("diff --git a/internal/diff/diff.go b/internal/diff/diff.go"); got != "internal/diff/diff.go" {
		t.Errorf("diffFilePath = %q", got)
	}
	if got := diffFilePath("not a diff header"); got != "" {
		t.Errorf("diffFilePath on garbage = %q", got)
	}
}

func TestNewSideLines(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -10,4 +10,5 @@ func main() {
 context line ten
-removed line
+added line eleven
+added line twelve
 context thirteen
@@ -30,2 +31,2 @@ func other() {
 context thirty-one
 context thirty-two
diff --git a/other.go b/other.go
--- a/other.go
+++ b/other.go
@@ -1,2 +1,2 @@
-old
+new first line
 second
`
	lines := NewSideLines(raw)

	for _, n := range []int{10, 11, 12, 13, 31, 32} {
		if !lines["main.go"][n] {
			t.Errorf("main.go:%d should be a valid new-side line", n)
		}
	}
	if lines["main.go"][14] || lines["main.go"][30] {
		t.Error("lines outside hunks must not be valid")
	}
	if !lines["other.go"][1] || !lines["other.go"][2] {
		t.Errorf("other.go lines wrong: %v", lines["other.go"])
	}
}

func TestSuppressions(t *testing.T) {
	raw := `diff --git a/db.go b/db.go
--- a/db.go
+++ b/db.go
@@ -1,3 +1,5 @@
 package db
+// pr-review:allow sql-injection table name comes from a fixed enum
+func query() {}
+// pr-review:allow bare-directive-no-reason
 // pr-review:allow unchanged-line this is context, not an added line
`
	got := Suppressions(raw)
	if len(got) != 1 {
		t.Fatalf("expected 1 suppression (with reason, on an added line), got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "db.go") || !strings.Contains(got[0], "sql-injection") || !strings.Contains(got[0], "fixed enum") {
		t.Errorf("suppression = %q", got[0])
	}
}

func TestFilePaths(t *testing.T) {
	raw := "diff --git a/x.go b/x.go\n+foo\ndiff --git a/dir/y.go b/dir/y.go\n+bar\n"
	got := FilePaths(raw)
	if len(got) != 2 || got[0] != "x.go" || got[1] != "dir/y.go" {
		t.Errorf("FilePaths = %v", got)
	}
}

func TestNewSideLines_noNewlineMarkerMidHunk(t *testing.T) {
	raw := `diff --git a/f.txt b/f.txt
--- a/f.txt
+++ b/f.txt
@@ -1,2 +1,3 @@
 context one
-old last
\ No newline at end of file
+new two
+new three
`
	lines := NewSideLines(raw)
	for _, n := range []int{1, 2, 3} {
		if !lines["f.txt"][n] {
			t.Errorf("f.txt:%d should be valid despite mid-hunk no-newline marker", n)
		}
	}
}
