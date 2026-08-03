package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickwhiteley/pr-reviewer/internal/github"
)

func TestBuildDiscussion(t *testing.T) {
	var human, bot github.Comment
	human.User.Login = "alice"
	human.Body = "The retry is intentional — upstream flakes, see #42."
	bot.User.Login = "github-actions"
	bot.Body = "## Code Review: x\n<!-- wd-auto-review:agent=x -->"

	var reply github.ReviewComment
	reply.User.Login = "bob"
	reply.Body = "Agreed, leaving as is."
	reply.Path = "retry.go"
	reply.Line = 12

	out := buildDiscussion([]github.Comment{human, bot}, []github.ReviewComment{reply})

	if !strings.Contains(out, "@alice") || !strings.Contains(out, "intentional") {
		t.Errorf("missing human comment: %q", out)
	}
	if strings.Contains(out, "wd-auto-review") {
		t.Error("bot's own comments must be excluded from discussion")
	}
	if !strings.Contains(out, "@bob (on retry.go:12)") {
		t.Errorf("missing review reply with location: %q", out)
	}
}

func TestBuildFileContext(t *testing.T) {
	dir := t.TempDir()
	textFile := filepath.Join(dir, "code.go")
	os.WriteFile(textFile, []byte("package main\n// fence breaker: ```\n"), 0o644)
	binFile := filepath.Join(dir, "blob.bin")
	os.WriteFile(binFile, []byte{0x00, 0x01, 0x02}, 0o644)

	out := buildFileContext([]string{textFile, binFile, filepath.Join(dir, "deleted.go")}, maxFileContextBytes)

	if !strings.Contains(out, "fence breaker") {
		t.Errorf("missing text file content: %q", out)
	}
	if !strings.Contains(out, "````") {
		t.Error("content must be wrapped in a four-backtick fence")
	}
	if strings.Contains(out, "blob.bin") {
		t.Error("binary files must be skipped")
	}
	if strings.Contains(out, "deleted.go") {
		t.Error("missing files must be skipped silently")
	}
}
