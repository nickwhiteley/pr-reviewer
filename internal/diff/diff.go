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
	"dist/",
	"*.gen.go",
	"*.pb.go",
	"PR-REVIEW.md",
	"CLAUDE.md",
}

const maxDiffSize = 100 * 1024 // 100KB

// Compute returns the diff between base and head.
func Compute(ctx context.Context, base, head string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", base, head)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git diff: %w: stderr=%s", err, ee.Stderr)
		}
		return "", fmt.Errorf("git diff: %w", err)
	}

	filtered := filterDiff(string(out))
	if len(filtered) > maxDiffSize {
		return filtered[:maxDiffSize] + "\n\n[diff truncated at 500KB]", nil
	}
	return filtered, nil
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
