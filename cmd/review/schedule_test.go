package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/nickwhiteley/pr-reviewer/internal/diff"
	"github.com/nickwhiteley/pr-reviewer/internal/review"
)

func TestNewPromptBudget(t *testing.T) {
	// Kimi K2.6: (262144 - 8192) tokens × 3 bytes ≈ 762KB of prompt. The diff
	// chunk should land on the ceiling, not consume 60% of that.
	big := newPromptBudget(762 * 1024)
	if big.chunk != diff.MaxChunkSize {
		t.Errorf("large window: chunk = %d, want the %d ceiling", big.chunk, diff.MaxChunkSize)
	}
	if big.fileContext != maxFileContextBytes {
		t.Errorf("large window: fileContext = %d, want %d", big.fileContext, maxFileContextBytes)
	}

	// A small-window model must get proportionally smaller sections rather
	// than a prompt the server will quietly truncate.
	small := newPromptBudget(80 * 1024)
	if small.chunk >= diff.MaxChunkSize {
		t.Errorf("small window: chunk = %d, expected scaling below the ceiling", small.chunk)
	}
	if small.chunk+small.fileContext > 80*1024 {
		t.Errorf("small window: sections total %d, over the %d budget",
			small.chunk+small.fileContext, 80*1024)
	}

	// Never below the floor, however tiny the window.
	tiny := newPromptBudget(1024)
	if tiny.chunk < diff.MinChunkSize {
		t.Errorf("tiny window: chunk = %d, want at least the %d floor", tiny.chunk, diff.MinChunkSize)
	}
}

func TestPartitionGating(t *testing.T) {
	findings := []review.Finding{
		{Title: "a", Severity: "critical"},
		{Title: "b", Severity: "medium"},
		{Title: "c", Severity: "high"},
		{Title: "d", Severity: "low"},
	}
	gating, advisory := partitionGating(findings)

	if len(gating) != 2 || gating[0].Title != "a" || gating[1].Title != "c" {
		t.Errorf("gating = %+v, want the critical and high findings", gating)
	}
	if len(advisory) != 2 || advisory[0].Title != "b" || advisory[1].Title != "d" {
		t.Errorf("advisory = %+v, want the medium and low findings", advisory)
	}
}

func TestAgentOutcomeMerge(t *testing.T) {
	o := agentOutcome{done: map[int]bool{}}

	o.merge(0, chunkResult{
		findings: []review.Finding{{File: "a.go", Line: 1, Severity: "high", Title: "x"}},
		notes:    "note one",
	})
	// A duplicate from an overlapping chunk must collapse in finalize.
	o.merge(2, chunkResult{
		findings: []review.Finding{
			{File: "a.go", Line: 1, Severity: "high", Title: "x"},
			{File: "b.go", Line: 9, Severity: "low", Title: "y"},
		},
		legacy: review.SeveritySummary{Medium: 3},
	})
	// An errored chunk contributes nothing and is not marked done.
	o.merge(1, chunkResult{err: errors.New("provider exploded")})

	o.finalize(true)

	if len(o.findings) != 2 {
		t.Errorf("findings = %d, want 2 after dedupe", len(o.findings))
	}
	if o.summary.High != 1 || o.summary.Low != 1 {
		t.Errorf("summary = %+v, want High:1 Low:1 from structured findings", o.summary)
	}
	if o.summary.Medium != 3 {
		t.Errorf("summary.Medium = %d, want 3 carried from the legacy block", o.summary.Medium)
	}
	// Counted after dedupe: the one surviving low finding, not the raw
	// per-chunk total, so the note matches what the report shows.
	if o.unverifiedFindings != 1 {
		t.Errorf("unverifiedFindings = %d, want 1 (the deduped low finding)", o.unverifiedFindings)
	}
	if o.done[1] {
		t.Error("an errored chunk must not count as reviewed")
	}
	if o.err == nil {
		t.Error("the chunk error must be retained for reporting")
	}
}

func TestAgentOutcomeUnreviewedFiles(t *testing.T) {
	chunks := []string{
		"diff --git a/one.go b/one.go\n+x\n",
		"diff --git a/two.go b/two.go\n+y\n",
		// Same file split across two chunks by hunk: listed once.
		"diff --git a/two.go b/two.go\n+z\n",
	}
	o := agentOutcome{done: map[int]bool{0: true}}

	got := o.unreviewedFiles(chunks)
	if len(got) != 1 || got[0] != "two.go" {
		t.Errorf("unreviewedFiles = %v, want [two.go] deduped", got)
	}

	o.done[1], o.done[2] = true, true
	if got := o.unreviewedFiles(chunks); len(got) != 0 {
		t.Errorf("unreviewedFiles = %v, want empty when every chunk completed", got)
	}
}

func TestFormatCoverageNote(t *testing.T) {
	if got := formatCoverageNote(nil, false); got != "" {
		t.Errorf("full coverage must produce no note, got %q", got)
	}

	note := formatCoverageNote([]string{"a.go", "b.go"}, true)
	if !strings.Contains(note, "Incomplete coverage") || !strings.Contains(note, "a.go") {
		t.Errorf("note must name the unreviewed files: %q", note)
	}
	if !strings.Contains(note, "ran out of time") {
		t.Errorf("timeout must be given as the cause: %q", note)
	}

	// Long lists are capped so a pathological PR can't produce a wall of text.
	many := make([]string, 100)
	for i := range many {
		many[i] = "f.go"
	}
	long := formatCoverageNote(many, false)
	if !strings.Contains(long, "and 60 more") {
		t.Errorf("long file lists must be elided: %q", long)
	}
}
