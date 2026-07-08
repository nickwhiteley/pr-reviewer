// Package diff computes PR diffs using local git commands.
package diff

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// MaxChunkSize is the maximum bytes per diff chunk sent to a provider.
const MaxChunkSize = 50 * 1024 // 50KB

// Excluded patterns that are skipped from the diff.
var exclusions = []string{
	"vendor/",
	"node_modules/",
	"*.lock",
	"package-lock.json",
	"pnpm-lock.yaml",
	"yarn.lock",
	"dist/",
	"*.gen.go",
	"*.pb.go",
	"go.sum",
	"PR-REVIEW.md",
	"CLAUDE.md",
}

// Compute returns the diff between base and head.
//
// This does NOT truncate the result, even for large PRs. It used to
// hard-truncate the whole diff to 100KB before any per-file chunking ran,
// which silently dropped entire files from every downstream chunk — not
// just the last one, since chunking (SplitFiles/ChunkFiles) only ever saw
// whatever survived this cutoff. In practice that meant large-but-normal
// PRs (a few hundred KB across 20-30 files) had roughly half their files
// invisible to every review agent, in every chunk, with no correct
// indication of which files were affected — agents would report "diff
// truncated, no reviewable changes" for a chunk boundary that had nothing
// to do with the actual file list, and speculate about files they never
// saw at all. Chunking already exists specifically to handle diffs larger
// than one provider call can take; a second, earlier, whole-diff cutoff
// defeated it. The caller (cmd/review) is responsible for capping the
// number of *chunks* actually sent to a provider, which is the real cost
// control and — unlike a raw byte cutoff — can honestly report what
// wasn't reviewed instead of silently mangling file boundaries.
func Compute(ctx context.Context, base, head string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", base, head)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git diff: %w: stderr=%s", err, ee.Stderr)
		}
		return "", fmt.Errorf("git diff: %w", err)
	}

	return filterDiff(string(out)), nil
}

func filterDiff(raw string) string {
	var out bytes.Buffer
	var skipFile bool

	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			skipFile = false
			for _, ex := range exclusions {
				if strings.Contains(line, ex) {
					skipFile = true
					break
				}
			}
		}
		if skipFile {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// SplitFiles splits a raw diff into individual per-file diffs.
func SplitFiles(raw string) []string {
	var files []string
	var current strings.Builder

	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if current.Len() > 0 {
				files = append(files, current.String())
			}
			current.Reset()
		}
		current.WriteString(line)
		current.WriteByte('\n')
	}
	if current.Len() > 0 {
		files = append(files, current.String())
	}
	return files
}

// ChunkFiles groups per-file diffs into chunks where each chunk is at most maxSize bytes.
func ChunkFiles(files []string, maxSize int) [][]string {
	var chunks [][]string
	var current []string
	var currentSize int

	for _, f := range files {
		const truncMarker = "\n\n[file truncated]\n"
		if len(f) > maxSize {
			// A single file exceeds the limit; truncate it
			f = f[:maxSize-len(truncMarker)] + truncMarker
		}
		if currentSize+len(f) > maxSize && len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
			currentSize = 0
		}
		current = append(current, f)
		currentSize += len(f)
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}
