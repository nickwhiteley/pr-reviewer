// Package diff computes PR diffs using local git commands.
package diff

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// MaxChunkSize is the ceiling on diff bytes per chunk sent to a provider.
//
// Sized against the review model's context window rather than a round
// number: Kimi K2.6 (the default Ollama model) accepts 262,144 tokens, and
// diff/code text runs roughly 3.5 bytes per token, so the window is on the
// order of 900KB. A 150KB diff chunk plus the prompt's other sections (up to
// 96KB of file context, 8KB repo tree, 16KB discussion, instructions) lands
// near 70K tokens — about a quarter of the window, leaving headroom for the
// response and for models with smaller windows.
//
// This was 50KB, a figure that predates the current generation of context
// windows. Because every chunk is a separate provider round-trip and each
// round-trip pays its own decode cost, chunk count is very nearly a direct
// multiplier on wall-clock review time — an undersized chunk was the main
// reason large PRs blew through --review-timeout. Chunking smaller would
// have made that worse, not better.
const MaxChunkSize = 150 * 1024

// MinChunkSize floors adaptive sizing so a pathological input can never
// drive the per-chunk budget below something that holds a realistic hunk.
const MinChunkSize = 16 * 1024

// ChunkSizeFor returns the per-chunk byte budget for a diff of totalBytes,
// given a ceiling (pass 0 for MaxChunkSize).
//
// It picks the SMALLEST number of chunks that keeps every chunk under the
// ceiling, then spreads the diff evenly over that many chunks. The balancing
// step matters because chunks are now reviewed concurrently: wall-clock time
// is set by the largest chunk, so two 80KB chunks finish sooner than a 150KB
// chunk plus a 10KB one, even though both are two round-trips.
func ChunkSizeFor(totalBytes, ceiling int) int {
	if ceiling <= 0 {
		ceiling = MaxChunkSize
	}
	if totalBytes <= ceiling {
		return ceiling
	}
	chunks := (totalBytes + ceiling - 1) / ceiling
	return max((totalBytes+chunks-1)/chunks, MinChunkSize)
}

// Excluded patterns that are skipped from the diff. Three forms are
// supported (see excluded): "dir/" matches that directory at any depth,
// "*.ext" glob-matches the file basename, anything else matches the
// basename or the full path exactly.
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

// excluded reports whether a repo-relative file path matches an exclusion.
func excluded(filePath string) bool {
	base := path.Base(filePath)
	for _, ex := range exclusions {
		switch {
		case strings.HasSuffix(ex, "/"):
			// Directory pattern: match at the root or any depth, but only on
			// component boundaries so "dist/" doesn't catch "redist/".
			if strings.HasPrefix(filePath, ex) || strings.Contains(filePath, "/"+ex) {
				return true
			}
		case strings.ContainsAny(ex, "*?["):
			if ok, _ := path.Match(ex, base); ok {
				return true
			}
		default:
			if base == ex || filePath == ex {
				return true
			}
		}
	}
	return false
}

// diffFilePath extracts the new-side ("b/") path from a "diff --git" header
// line, or "" if the line doesn't parse. Handles the common case; paths with
// spaces fall back to the last "b/" occurrence.
func diffFilePath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+len(" b/"):]
	}
	return ""
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
			skipFile = excluded(diffFilePath(line))
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

// Chunk splits a whole diff into review chunks of at most ceiling bytes,
// covering all of it.
//
// It packs twice — once at the ceiling, once at the balanced size from
// ChunkSizeFor — and keeps whichever produced fewer chunks. Balancing alone
// can overshoot: files are packed greedily and in order, so a smaller target
// can strand a file that would have fitted at the ceiling and end up costing
// MORE round-trips than not balancing at all. Fewer chunks always wins, since
// each one is a provider call; at equal counts the balanced packing is
// preferred because its worst-case chunk is smaller, and the largest chunk is
// what sets wall-clock time once chunks run concurrently.
func Chunk(raw string, ceiling int) []string {
	if len(raw) <= ceiling {
		return []string{raw}
	}

	files := SplitFiles(raw)
	best := ChunkFiles(files, ChunkSizeFor(len(raw), ceiling))
	if atCeiling := ChunkFiles(files, ceiling); len(atCeiling) < len(best) {
		best = atCeiling
	}

	out := make([]string, len(best))
	for i, c := range best {
		out[i] = strings.Join(c, "\n")
	}
	return out
}

// ChunkFiles groups per-file diffs into chunks where each chunk is at most
// maxSize bytes. A file larger than maxSize is split at hunk boundaries (see
// splitFileByHunks) rather than truncated, so no changed line is ever dropped
// from the review.
func ChunkFiles(files []string, maxSize int) [][]string {
	var chunks [][]string
	var current []string
	var currentSize int

	for _, f := range files {
		for _, part := range splitFileByHunks(f, maxSize) {
			if currentSize+len(part) > maxSize && len(current) > 0 {
				chunks = append(chunks, current)
				current = nil
				currentSize = 0
			}
			current = append(current, part)
			currentSize += len(part)
		}
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// splitFileByHunks splits one file's diff into pieces of at most maxSize
// bytes, breaking between hunks and repeating the file header on every piece
// so each one is a valid, self-describing diff.
//
// This replaces a mid-line truncation that appended a "[file truncated]"
// marker: that dropped changed lines from the review entirely, and — because
// the cut landed at an arbitrary byte offset — often left a half-written line
// for the agent to misread. A single hunk larger than maxSize is emitted
// whole rather than cut, since an oversized prompt fails loudly at the
// provider whereas a truncated one silently hides code from the reviewer.
func splitFileByHunks(file string, maxSize int) []string {
	if len(file) <= maxSize {
		return []string{file}
	}

	// Byte offsets where each hunk starts. Working in offsets and slicing the
	// original keeps every piece byte-identical to its source; rebuilding
	// from split lines would perturb trailing newlines.
	var starts []int
	for off := 0; off < len(file); {
		line := file[off:]
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if strings.HasPrefix(line, "@@ ") {
			starts = append(starts, off)
		}
		off += len(line) + 1
	}
	if len(starts) == 0 {
		return []string{file} // binary file or pure rename — nothing to split on
	}

	// Everything before the first hunk is the header (diff --git, index,
	// ---/+++ lines); it is repeated on every piece so each one is valid.
	header := file[:starts[0]]

	var (
		parts      []string
		cur        strings.Builder
		curHasHunk bool
	)
	cur.WriteString(header)

	for i, start := range starts {
		end := len(file)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		hunk := file[start:end]

		if curHasHunk && cur.Len()+len(hunk) > maxSize {
			parts = append(parts, cur.String())
			cur.Reset()
			cur.WriteString(header)
			curHasHunk = false
		}
		cur.WriteString(hunk)
		curHasHunk = true
	}
	if curHasHunk {
		parts = append(parts, cur.String())
	}

	return parts
}

// NewSideLines returns, per file, the set of new-side line numbers visible
// in the diff (added and context lines). GitHub rejects inline review
// comments on lines outside the diff, so findings are validated against
// this before posting.
func NewSideLines(raw string) map[string]map[int]bool {
	result := make(map[string]map[int]bool)
	var file string
	var newLine int
	inHunk := false

	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			file = diffFilePath(line)
			inHunk = false
		case strings.HasPrefix(line, "@@ "):
			// Hunk header: @@ -a,b +c,d @@
			inHunk = false
			rest := line[3:]
			if i := strings.Index(rest, " +"); i >= 0 {
				numPart := rest[i+2:]
				if j := strings.IndexAny(numPart, ", @"); j >= 0 {
					numPart = numPart[:j]
				}
				if n, err := strconv.Atoi(numPart); err == nil {
					newLine = n
					inHunk = true
				}
			}
		case inHunk && file != "":
			if len(line) == 0 {
				// Blank context line within a hunk.
				markLine(result, file, newLine)
				newLine++
				continue
			}
			switch line[0] {
			case '+', ' ':
				markLine(result, file, newLine)
				newLine++
			case '-':
				// Old-side only: does not advance the new-side counter.
			case '\\':
				// "\ No newline at end of file" — a marker, not content. It can
				// appear mid-hunk (after the old side's last line) with more
				// +/- lines following, so it must not end the hunk.
			default:
				inHunk = false
			}
		}
	}
	return result
}

func markLine(result map[string]map[int]bool, file string, line int) {
	if result[file] == nil {
		result[file] = make(map[int]bool)
	}
	result[file][line] = true
}

// FilePaths returns the repo-relative paths of the files in a diff (or diff
// chunk), in order of appearance.
func FilePaths(raw string) []string {
	var paths []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if p := diffFilePath(line); p != "" {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// suppressionPattern matches an author's explicit acknowledgement that
// something a reviewer would flag is intentional, e.g.
//
//	// pr-review:allow sql-injection — table name comes from a fixed enum
//
// Only directives WITH a reason are honored; the capture requires trailing text.
var suppressionPattern = regexp.MustCompile(`pr-review:allow\s+(\S+)\s+(.+)`)

// Suppressions scans the ADDED lines of a diff for pr-review:allow
// directives and returns human-readable descriptions ("file: topic — reason").
// Directives on unchanged lines are intentionally ignored: an allow must be
// (re)stated in the change that introduces the flagged code.
func Suppressions(raw string) []string {
	var out []string
	var file string
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			file = diffFilePath(line)
			continue
		}
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		if m := suppressionPattern.FindStringSubmatch(line); m != nil {
			out = append(out, fmt.Sprintf("`%s`: %s — %s", file, m[1], strings.TrimSpace(m[2])))
		}
	}
	return out
}

// RepoTree returns a `git ls-files` listing capped at maxBytes, so agents can
// check "X doesn't exist" claims against reality instead of guessing.
func RepoTree(ctx context.Context, maxBytes int) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "ls-files").Output()
	if err != nil {
		return "", fmt.Errorf("git ls-files: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if len(s) <= maxBytes {
		return s, nil
	}
	cut := strings.LastIndexByte(s[:maxBytes], '\n')
	if cut <= 0 {
		cut = maxBytes
	}
	omitted := strings.Count(s[cut:], "\n") + 1
	return s[:cut] + fmt.Sprintf("\n... (%d more files omitted)", omitted), nil
}
