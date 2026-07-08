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
