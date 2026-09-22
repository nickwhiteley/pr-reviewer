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

// DefaultExclusions are skipped from every diff: generated code, vendored
// trees, lockfiles, and the two files that configure this tool (quoting them
// back at an agent is both noise and an injection surface). A repository adds
// its own through the "Excluded Paths" section of PR-REVIEW.md.
var DefaultExclusions = []string{
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

// Match reports whether a repo-relative file path matches any of the given
// patterns. Four forms are supported:
//
//   - "dir/" or "dir/**" matches that directory at any depth
//   - "a/b/*.go" (a pattern containing "/") is path.Match'd against the whole path
//   - "*.ext" glob-matches the file basename
//   - anything else matches the basename or the full path exactly
//
// An empty pattern list matches nothing; callers that mean "everything"
// check for emptiness themselves, because the two need opposite defaults:
// no exclusions excludes nothing, no path filter includes everything.
func Match(filePath string, patterns []string) bool {
	base := path.Base(filePath)
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		pat = strings.TrimSuffix(pat, "**")
		switch {
		case strings.HasSuffix(pat, "/"):
			// Directory pattern: match at the root or any depth, but only on
			// component boundaries so "dist/" doesn't catch "redist/".
			if strings.HasPrefix(filePath, pat) || strings.Contains(filePath, "/"+pat) {
				return true
			}
		case strings.Contains(pat, "/") && strings.ContainsAny(pat, "*?["):
			if ok, _ := path.Match(pat, filePath); ok {
				return true
			}
		case strings.ContainsAny(pat, "*?["):
			if ok, _ := path.Match(pat, base); ok {
				return true
			}
		default:
			if base == pat || filePath == pat {
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
// extra names repository-specific exclusions from PR-REVIEW.md, applied on
// top of DefaultExclusions.
func Compute(ctx context.Context, base, head string, extra []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", base, head)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git diff: %w: stderr=%s", err, ee.Stderr)
		}
		return "", fmt.Errorf("git diff: %w", err)
	}

	return Filter(string(out), nil, append(append([]string{}, DefaultExclusions...), extra...)), nil
}

// Filter keeps the files of a diff that match include (all of them when
// include is empty) and do not match exclude. It works on whole files, so
// the result is always a valid diff.
//
// This is what routes a chunk of the review to the agents it concerns:
// filtering BEFORE chunking, rather than after, is what makes the saving
// real. An agent scoped to `api/internal/store/` gets its own small diff
// packed into its own chunks, instead of being handed every chunk of the
// whole PR and asked to ignore most of each one.
func Filter(raw string, include, exclude []string) string {
	var out bytes.Buffer
	var skipFile bool

	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			p := diffFilePath(line)
			skipFile = Match(p, exclude) || (len(include) > 0 && !Match(p, include))
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

// Hunk is one hunk of one file's diff: the new-side line range it covers and
// the raw text of the hunk itself, header included.
type Hunk struct {
	File  string
	Start int // first new-side line number
	End   int // last new-side line number (inclusive)
	Text  string
}

// hunkHeaderPattern captures the new-side start and length from "@@ -a,b +c,d @@".
// The length is optional: "@@ -1 +1 @@" means one line.
var hunkHeaderPattern = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// Hunks returns every hunk in a diff, in order of appearance.
//
// Two things need this and neither wants the whole diff: the file-context
// section, which shows the source around each hunk rather than whole files,
// and the verification pass, which needs only the hunk a finding points at.
func Hunks(raw string) []Hunk {
	var hunks []Hunk
	var file string
	var cur *Hunk
	var b strings.Builder

	flush := func() {
		if cur != nil {
			cur.Text = b.String()
			hunks = append(hunks, *cur)
			cur = nil
		}
		b.Reset()
	}

	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			file = diffFilePath(line)
		case strings.HasPrefix(line, "@@ "):
			flush()
			m := hunkHeaderPattern.FindStringSubmatch(line)
			if m == nil || file == "" {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			length := 1
			if m[2] != "" {
				length, _ = strconv.Atoi(m[2])
			}
			end := start + length - 1
			if end < start {
				// A pure deletion has length 0; it still anchors at start.
				end = start
			}
			cur = &Hunk{File: file, Start: start, End: end}
			b.WriteString(line)
			b.WriteByte('\n')
		default:
			if cur != nil {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
	}
	flush()
	return hunks
}

// HunkRanges returns, per file, the new-side line ranges the diff touches.
func HunkRanges(raw string) map[string][][2]int {
	out := make(map[string][][2]int)
	for _, h := range Hunks(raw) {
		out[h.File] = append(out[h.File], [2]int{h.Start, h.End})
	}
	return out
}

// EvidenceFor returns a minimal diff containing only the hunks that cover the
// given (file, line) locations, with each file's header repeated so the result
// is a valid, self-describing diff.
//
// The verification pass used to be handed the whole chunk it came from —
// 120KB of diff to re-examine three findings against. Verification asks one
// question, "does a line here actually support this?", and the only lines
// that can answer it are the ones the finding points at. Locations that fall
// in no hunk are skipped: a finding that cannot be anchored has no evidence
// to check, and the verifier is told to dismiss it on exactly those grounds.
func EvidenceFor(raw string, locations map[string][]int) string {
	headers := fileHeaders(raw)

	var b strings.Builder
	emitted := make(map[string]bool)
	for _, h := range Hunks(raw) {
		lines, ok := locations[h.File]
		if !ok {
			continue
		}
		hit := false
		for _, l := range lines {
			if l >= h.Start && l <= h.End {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if !emitted[h.File] {
			emitted[h.File] = true
			b.WriteString(headers[h.File])
		}
		b.WriteString(h.Text)
	}
	return b.String()
}

// fileHeaders returns each file's diff header — everything from "diff --git"
// up to its first hunk.
func fileHeaders(raw string) map[string]string {
	out := make(map[string]string)
	var file string
	var b strings.Builder
	flush := func() {
		if file != "" {
			out[file] = b.String()
		}
		b.Reset()
	}
	inHeader := false
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			file = diffFilePath(line)
			inHeader = true
			b.WriteString(line)
			b.WriteByte('\n')
		case strings.HasPrefix(line, "@@ "):
			inHeader = false
		case inHeader:
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	flush()
	return out
}
