package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/nickwhiteley/pr-reviewer/internal/github"
)

const (
	// Budgets for prompt context sections. Generous enough to be useful,
	// bounded so a huge PR can't balloon every prompt.
	maxDiscussionBytes  = 16 * 1024
	maxCommentBytes     = 2 * 1024
	maxFileBytes        = 24 * 1024
	maxFileContextBytes = 96 * 1024
	maxRepoTreeBytes    = 8 * 1024
)

// buildDiscussion renders human PR conversation (issue comments and inline
// review replies) for agent prompts, so explanations of intentional behavior
// reach the reviewer. Our own bot comments are skipped — agents get those
// separately as their previous report.
func buildDiscussion(comments []github.Comment, reviewComments []github.ReviewComment) string {
	var b strings.Builder

	add := func(author, location, body string) {
		if strings.Contains(body, "wd-auto-review") {
			return // our own output, or a quote of it
		}
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		if len(body) > maxCommentBytes {
			body = body[:maxCommentBytes] + "… [comment truncated]"
		}
		entry := fmt.Sprintf("@%s%s:\n%s\n\n", author, location, body)
		if b.Len()+len(entry) > maxDiscussionBytes {
			return
		}
		b.WriteString(entry)
	}

	for _, c := range comments {
		add(c.User.Login, "", c.Body)
	}
	for _, rc := range reviewComments {
		loc := ""
		if rc.Path != "" {
			loc = fmt.Sprintf(" (on %s:%d)", rc.Path, rc.Line)
		}
		add(rc.User.Login, loc, rc.Body)
	}

	return strings.TrimSpace(b.String())
}

// buildFileContext reads the current (head) content of changed files from
// the checked-out working tree, so agents see whole files instead of hunks.
// Missing files (deleted in the PR), binaries, and budget overruns are
// skipped or truncated with an explicit note — never silently.
func buildFileContext(paths []string) string {
	var b strings.Builder
	var omitted []string

	for _, p := range paths {
		if b.Len() >= maxFileContextBytes {
			omitted = append(omitted, p)
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue // deleted in this PR, or unreadable — the diff still shows it
		}
		if isBinary(data) {
			continue
		}
		truncNote := ""
		if len(data) > maxFileBytes {
			data = data[:maxFileBytes]
			truncNote = "\n… [file truncated for context]"
		}
		// Four-backtick fence so file contents containing ``` don't break out.
		fmt.Fprintf(&b, "### `%s`\n\n````\n%s%s\n````\n\n", p, strings.TrimRight(string(data), "\n"), truncNote)
	}

	if len(omitted) > 0 {
		fmt.Fprintf(&b, "_Content omitted for %d more file(s) (context budget): %s_\n",
			len(omitted), strings.Join(omitted, ", "))
	}
	return strings.TrimSpace(b.String())
}

// isBinary reports whether data looks like a binary file (NUL byte in the
// first 8KB, the same heuristic git uses).
func isBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return bytes.IndexByte(probe, 0) >= 0
}
