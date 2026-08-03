package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickwhiteley/pr-reviewer/internal/config"
	"github.com/nickwhiteley/pr-reviewer/internal/diff"
	"github.com/nickwhiteley/pr-reviewer/internal/github"
	"github.com/nickwhiteley/pr-reviewer/internal/hygiene"
	"github.com/nickwhiteley/pr-reviewer/internal/provider"
	"github.com/nickwhiteley/pr-reviewer/internal/review"
)

func main() {
	// Subcommand dispatch before flag parsing: "pr-reviewer check-config [path]"
	if len(os.Args) > 1 && os.Args[1] == "check-config" {
		os.Exit(runCheckConfig(os.Args[2:]))
	}

	var (
		prNum         = flag.Int("pr", 0, "Pull request number")
		repo          = flag.String("repo", "", "Repository in owner/name format")
		base          = flag.String("base", "", "Base commit SHA")
		head          = flag.String("head", "", "Head commit SHA")
		model         = flag.String("model", "", "AI model to use (default: provider-specific)")
		debug         = flag.Bool("debug", false, "Enable debug logging")
		configPath    = flag.String("config", "PR-REVIEW.md", "Path to PR-REVIEW.md")
		skipAgents    = flag.Bool("skip-agents", false, "Skip AI agent reviews (hygiene only)")
		coverageFile  = flag.String("coverage-file", "", "Path to go tool cover -func output to include in agent prompts")
		providerName  = flag.String("provider", "ollama", "AI provider: ollama or anthropic")
		reviewTimeout = flag.Int("review-timeout", 600, "Timeout in seconds for the AI review phase (0 = no timeout)")
		failMode      = flag.String("fail-mode", "closed", "closed: agent failures/timeouts/unparseable results fail the check; open: they are advisory (exit 0)")
		verify        = flag.Bool("verify", true, "Run a verification pass on each agent's findings to drop unsupported ones")
		inline        = flag.Bool("inline-comments", true, "Post findings as inline PR review comments where they map to diff lines")
		fileContext   = flag.Bool("file-context", true, "Include full changed-file contents (budgeted) in agent prompts")
		dryRun        = flag.Bool("dry-run", false, "Print reports to stdout instead of posting to GitHub (no --pr/--repo needed)")
		concurrency   = flag.Int("concurrency", 6, "Maximum provider calls in flight at once across all agents and chunks")
		maxChunkBytes = flag.Int("max-chunk-bytes", 0, "Override the per-chunk diff byte ceiling (0 = derive from the model's context window)")
	)
	flag.Parse()

	if *concurrency < 1 {
		fmt.Fprintf(os.Stderr, "invalid --concurrency %d: must be at least 1\n", *concurrency)
		os.Exit(1)
	}

	if *failMode != "closed" && *failMode != "open" {
		fmt.Fprintf(os.Stderr, "invalid --fail-mode %q: must be closed or open\n", *failMode)
		os.Exit(1)
	}
	failClosed := *failMode == "closed"

	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)

	if *dryRun {
		if *base == "" || *head == "" {
			slog.Error("missing required flags for --dry-run", "base", *base, "head", *head)
			os.Exit(1)
		}
	} else if *prNum == 0 || *repo == "" || *base == "" || *head == "" {
		slog.Error("missing required flags", "pr", *prNum, "repo", *repo, "base", *base, "head", *head)
		os.Exit(1)
	}

	// Resolve default model per provider.
	if *model == "" {
		switch *providerName {
		case "anthropic":
			*model = "claude-sonnet-4-6"
		default:
			*model = "kimi-k2.6:cloud"
		}
	}

	var p provider.Provider
	switch *providerName {
	case "anthropic":
		if !*skipAgents && os.Getenv("ANTHROPIC_API_KEY") == "" {
			slog.Error("ANTHROPIC_API_KEY is not set")
			os.Exit(1)
		}
		p = provider.NewAnthropicProvider()
	case "ollama":
		if !*skipAgents && os.Getenv("OLLAMA_API_KEY") == "" {
			slog.Error("OLLAMA_API_KEY is not set")
			os.Exit(1)
		}
		p = provider.NewOllamaProvider()
	default:
		slog.Error("unknown provider", "provider", *providerName)
		os.Exit(1)
	}

	var coverageSummary string
	if *coverageFile != "" {
		data, err := os.ReadFile(*coverageFile)
		if err != nil {
			slog.Warn("could not read coverage file", "path", *coverageFile, "error", err)
		} else {
			coverageSummary = strings.TrimSpace(string(data))
		}
	}

	ctx := context.Background()

	// Parse PR-REVIEW.md
	cfg, err := config.Parse(*configPath)
	if err != nil {
		slog.Error("failed to parse PR-REVIEW.md", "error", err)
		os.Exit(1)
	}

	// Compute diff
	prDiff, err := diff.Compute(ctx, *base, *head)
	if err != nil {
		slog.Error("failed to compute diff", "error", err)
		os.Exit(1)
	}

	// Setup the comment destination: GitHub, or stdout in dry-run mode.
	var gh *github.Client
	var sink commentSink = stdoutSink{}
	if !*dryRun {
		gh, err = github.NewClient(*repo)
		if err != nil {
			slog.Error("github client setup failed", "error", err)
			os.Exit(1)
		}
		sink = gh
	}

	// Run hygiene checks
	runner := hygiene.NewRunner(coverageSummary)
	var hygieneChecks []hygiene.Check
	for _, h := range cfg.Hygiene {
		hygieneChecks = append(hygieneChecks, hygiene.Check{
			ID:     h.ID,
			Name:   h.Name,
			Ticked: h.Ticked,
		})
	}
	hygieneResults := runner.Run(ctx, hygieneChecks)
	hygieneReport := hygiene.FormatReport(hygieneResults)

	// Post hygiene comment
	if err := sink.PostOrUpdate(ctx, *prNum, "<!-- wd-auto-review:type=hygiene -->", hygieneReport); err != nil {
		slog.Error("failed to post hygiene comment", "error", err)
		os.Exit(1)
	}

	if *skipAgents {
		slog.Info("review complete (agents skipped)")
		return
	}

	// Size the prompt sections against what the chosen model can actually
	// accept, then chunk the diff to fit. diff.Compute returns the full diff,
	// untruncated, and chunking now covers all of it: there is no cap on
	// chunk count and no per-file byte cutoff, so nothing is dropped from the
	// review. Wall-clock cost is controlled by running chunks concurrently
	// (see the worker pool below) rather than by discarding content.
	budget := newPromptBudget(p.PromptBudgetBytes(*model))
	if *maxChunkBytes > 0 {
		budget.chunk = *maxChunkBytes
	}

	diffChunks := diff.Chunk(prDiff, budget.chunk)
	slog.Info("diff chunked", "chunks", len(diffChunks), "diff_bytes", len(prDiff),
		"chunk_ceiling", budget.chunk)
	for i, c := range diffChunks {
		slog.Debug("chunk size", "chunk", i, "bytes", len(c))
	}

	// The full content of each chunk's changed files, built ONCE per chunk
	// and shared by every agent. This used to be rebuilt inside each agent's
	// loop, re-reading the same files off disk once per agent per chunk.
	chunkFileContext := make([]string, len(diffChunks))
	if *fileContext {
		for i, c := range diffChunks {
			chunkFileContext[i] = buildFileContext(diff.FilePaths(c), budget.fileContext)
		}
	}

	// Fetch existing PR comments once, up front, so each agent can see its
	// own prior report (if this is a re-run on a pushed-to PR) and avoid
	// blindly re-raising a finding that was already fixed or explained.
	// Best-effort: if this fails, every agent just runs without that
	// context, same as before this feature existed.
	var existingComments []github.Comment
	if gh != nil {
		if cs, err := gh.ListComments(ctx, *prNum); err != nil {
			slog.Warn("could not list existing comments for previous-review context", "error", err)
		} else {
			existingComments = cs
		}
	}
	previousReportFor := func(agent config.AgentConfig) string {
		marker := fmt.Sprintf("<!-- wd-auto-review:agent=%s -->", agent.Subagent)
		for _, cm := range existingComments {
			if strings.Contains(cm.Body, marker) {
				return clip(cm.Body, maxPreviousReportBytes, "previous report")
			}
		}
		return ""
	}

	// Inline review comments already on the PR: reused both for discussion
	// context and for inline-finding idempotency.
	var reviewComments []github.ReviewComment
	if gh != nil {
		if rcs, err := gh.ListReviewComments(ctx, *prNum); err != nil {
			slog.Warn("could not list review comments", "error", err)
		} else {
			reviewComments = rcs
		}
	}

	// Inline comment infrastructure: valid diff positions for anchoring, and
	// markers of findings already posted on earlier runs (idempotency).
	var poster *inlinePoster
	if *inline && gh != nil {
		existingMarkers := make(map[string]bool)
		for _, rc := range reviewComments {
			if m := findingMarkerPattern.FindString(rc.Body); m != "" {
				existingMarkers[m] = true
			}
		}
		poster = &inlinePoster{
			gh:       gh,
			pr:       *prNum,
			headSHA:  *head,
			valid:    diff.NewSideLines(prDiff),
			existing: existingMarkers,
			budget:   maxInlineComments,
		}
	}

	// Shared prompt input for every agent and chunk; per-chunk fields
	// (Agent, Diff, ChunkNote, PreviousReport, FileContext) are filled in
	// runChunk. Everything here is best-effort context: a fetch failure
	// degrades the review, it doesn't abort it.
	basePrompt := review.PromptInput{
		Cfg:             cfg,
		CoverageSummary: clip(coverageSummary, maxCoverageBytes, "coverage summary"),
		Discussion:      buildDiscussion(existingComments, reviewComments),
		Suppressions:    diff.Suppressions(prDiff),
	}
	if gh != nil {
		if pr, err := gh.GetPR(ctx, *prNum); err != nil {
			slog.Warn("could not fetch PR metadata", "error", err)
		} else {
			basePrompt.PRTitle = pr.Title
			basePrompt.PRBody = pr.Body
		}
	}
	if tree, err := diff.RepoTree(ctx, maxRepoTreeBytes); err != nil {
		slog.Warn("could not build repo tree", "error", err)
	} else {
		basePrompt.RepoTree = tree
	}

	// Build a context with a timeout for the AI review phase. On expiry we
	// post a PR comment and then fail or pass the check per --fail-mode.
	var reviewCtx context.Context
	var reviewCancel context.CancelFunc
	if *reviewTimeout > 0 {
		reviewCtx, reviewCancel = context.WithTimeout(ctx, time.Duration(*reviewTimeout)*time.Second)
	} else {
		reviewCtx, reviewCancel = context.WithCancel(ctx)
	}
	defer reviewCancel()

	// Schedule every (agent, chunk) pair through one bounded worker pool.
	//
	// Agents used to run concurrently while each walked its own chunks
	// serially, so wall-clock time was chunkCount × (review + verify) latency
	// no matter how many agents there were — adding agents bought nothing and
	// the runner spent most of the review idle. Scheduling the whole
	// agent×chunk grid keeps --concurrency calls in flight instead, which is
	// what stops large PRs from running past --review-timeout.
	// Ordered chunk-major (every agent's chunk 0, then every agent's chunk 1,
	// …) rather than agent-major. If the budget does run out, coverage then
	// degrades uniformly — all agents have seen the same early chunks —
	// instead of leaving the last agent with nothing reviewed at all.
	type work struct{ agent, chunk int }
	var queue []work
	for c := range diffChunks {
		for a := range cfg.Agents {
			queue = append(queue, work{a, c})
		}
	}

	outcomes := make([]agentOutcome, len(cfg.Agents))
	previousReports := make([]string, len(cfg.Agents))
	for i, agent := range cfg.Agents {
		outcomes[i].done = make(map[int]bool)
		previousReports[i] = previousReportFor(agent)
	}

	var mu sync.Mutex
	jobs := make(chan work)
	workers := min(*concurrency, len(queue))

	slog.Info("scheduling review", "agents", len(cfg.Agents), "chunks", len(diffChunks),
		"calls", len(queue), "workers", workers, "provider", *providerName, "model", *model)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for w := range jobs {
				if reviewCtx.Err() != nil {
					return
				}
				res := runChunk(reviewCtx, p, *model, basePrompt, cfg.Agents[w.agent], chunkInput{
					diff:        diffChunks[w.chunk],
					fileContext: chunkFileContext[w.chunk],
					index:       w.chunk,
					total:       len(diffChunks),
					previous:    previousReports[w.agent],
				}, *verify)

				mu.Lock()
				outcomes[w.agent].merge(w.chunk, res)
				mu.Unlock()
			}
		})
	}

dispatch:
	for _, w := range queue {
		select {
		case jobs <- w:
		case <-reviewCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	timedOut := errors.Is(reviewCtx.Err(), context.DeadlineExceeded)

	// Publish whatever each agent managed to collect, even on timeout. The
	// previous behaviour discarded every finding an agent had already
	// produced and posted only a timeout notice, throwing away most of a
	// review that had run for minutes.
	var (
		hasCriticalOrHigh bool
		failedAgents      int
		partialAgents     int
	)
	for i, agent := range cfg.Agents {
		out := &outcomes[i]
		out.finalize(*verify)

		// An agent that completed nothing has no findings worth posting; it
		// gets a failure comment instead. On timeout the single timeout
		// notice below covers it, so don't spam one comment per agent.
		if len(out.done) == 0 {
			failedAgents++
			if !timedOut {
				err := out.err
				if err == nil {
					err = errors.New("agent produced no result for any diff chunk")
				}
				slog.Error("agent failed", "subagent", agent.Subagent, "error", err)
				marker := fmt.Sprintf("<!-- wd-auto-review:agent=%s -->", agent.Subagent)
				if postErr := sink.PostOrUpdate(ctx, *prNum, marker, review.FormatAgentFailureReport(agent, err)); postErr != nil {
					slog.Error("failed to post agent failure comment", "subagent", agent.Subagent, "error", postErr)
				}
			}
			continue
		}

		if out.summary.Critical > 0 || out.summary.High > 0 {
			hasCriticalOrHigh = true
		}
		unreviewed := out.unreviewedFiles(diffChunks)
		if len(unreviewed) > 0 || out.parseFailures > 0 {
			partialAgents++
		}

		var body strings.Builder
		body.WriteString(review.RenderFindings(out.findings, out.dismissed, out.notes))
		for _, raw := range out.fallbackTexts {
			fmt.Fprintf(&body, "\n\n---\n\n### Unstructured agent output\n\n%s\n", raw)
		}
		if out.parseFailures > 0 {
			fmt.Fprintf(&body,
				"\n\n---\n\n⚠️ **Results could not be parsed for %d of %d chunk(s)** — any findings there are "+
					"shown as raw output above but are NOT reflected in the severity summary. Treat this review as incomplete.\n",
				out.parseFailures, len(diffChunks),
			)
		}
		if *verify && out.unverifiedFindings > 0 {
			fmt.Fprintf(&body,
				"\n\n---\n\nℹ️ %d medium/low finding(s) were published without a verification pass. "+
					"Verification runs on critical/high findings — the ones that gate this check — to keep the "+
					"review inside its time budget.\n", out.unverifiedFindings)
		}
		body.WriteString(formatCoverageNote(unreviewed, timedOut))

		report := review.FormatAgentReport(agent, out.summary, body.String())
		marker := fmt.Sprintf("<!-- wd-auto-review:agent=%s -->", agent.Subagent)
		if err := sink.PostOrUpdate(ctx, *prNum, marker, report); err != nil {
			slog.Error("failed to post agent comment", "subagent", agent.Subagent, "error", err)
			// Do not fail the pipeline for posting errors; the review result is still valid.
		}

		if poster != nil {
			poster.post(ctx, agent.Subagent, out.findings)
		}
	}

	// A timeout still fails the check in closed mode: an incompletely
	// reviewed PR must not show green (spec FR-013). The difference is that
	// the partial findings are now on the PR alongside the notice.
	if timedOut {
		timeoutReport := formatTimeoutReport(*reviewTimeout, *providerName, failClosed)
		if postErr := sink.PostOrUpdate(ctx, *prNum, "<!-- wd-auto-review:type=timeout -->", timeoutReport); postErr != nil {
			slog.Error("failed to post timeout comment", "error", postErr)
		}
		if failClosed {
			slog.Error("review timed out — failing check (fail-mode=closed)", "timeout_seconds", *reviewTimeout)
			os.Exit(1)
		}
		slog.Info("review timed out — exiting 0 (fail-mode=open)", "timeout_seconds", *reviewTimeout)
		return
	}

	if hasCriticalOrHigh {
		slog.Info("review complete with critical/high findings")
		os.Exit(1)
	}
	if failedAgents > 0 || partialAgents > 0 {
		if failClosed {
			slog.Error("review incomplete — failing check (fail-mode=closed)",
				"failed_agents", failedAgents, "partial_agents", partialAgents)
			os.Exit(1)
		}
		slog.Info("review incomplete — exiting 0 (fail-mode=open)",
			"failed_agents", failedAgents, "partial_agents", partialAgents)
		return
	}

	slog.Info("review complete")
}

// formatCoverageNote names the files an agent never got to, so a partial
// review says exactly what it did not look at rather than implying the whole
// diff was covered.
func formatCoverageNote(unreviewed []string, timedOut bool) string {
	if len(unreviewed) == 0 {
		return ""
	}
	cause := "these chunks did not complete"
	if timedOut {
		cause = "the review ran out of time before reaching them"
	}
	const maxListed = 40
	listed := unreviewed
	suffix := ""
	if len(listed) > maxListed {
		suffix = fmt.Sprintf("\n- …and %d more", len(listed)-maxListed)
		listed = listed[:maxListed]
	}
	return fmt.Sprintf(
		"\n\n---\n\n⚠️ **Incomplete coverage** — %d file(s) in this PR were NOT reviewed by this agent (%s):\n\n- %s%s\n",
		len(unreviewed), cause, strings.Join(listed, "\n- "), suffix)
}

// formatTimeoutReport returns a PR comment body explaining that the review timed out.
func formatTimeoutReport(timeoutSecs int, providerName string, failClosed bool) string {
	consequence := "This check is marked **failed** because the review could not complete — the PR has NOT been reviewed. " +
		"A green check here must mean the code was actually looked at."
	if !failClosed {
		consequence = "This is **not a CI failure** (fail-mode=open) — but the code changes have NOT been reviewed by this tool."
	}
	return fmt.Sprintf(`## ⏱ PR Review — Agent Timeout

The AI review agents did not complete within the configured timeout (%ds via the **%s** provider).

%s

**To retry:** re-run the *PR Review* workflow job from the GitHub Actions tab.
`, timeoutSecs, providerName, consequence)
}

// promptBudget divides a provider's usable prompt bytes across the sections
// of an agent prompt that can grow with PR size. Deriving these from the
// model's own window keeps a big diff inside the context instead of relying
// on constants tuned for one particular model.
type promptBudget struct {
	chunk       int // diff bytes per chunk
	fileContext int // bytes of full changed-file content
}

func newPromptBudget(total int) promptBudget {
	return promptBudget{
		chunk:       min(max(total*60/100, diff.MinChunkSize), diff.MaxChunkSize),
		fileContext: min(total*25/100, maxFileContextBytes),
	}
}

// agentOutcome is the combined result of one agent across all diff chunks.
// Chunks land in it concurrently and out of order, so everything here is
// accumulated rather than assumed sequential.
type agentOutcome struct {
	summary   review.SeveritySummary
	findings  []review.Finding
	dismissed []review.Dismissed
	notes     []string
	// fallbackTexts holds raw chunk responses that had no parseable findings
	// block; legacy severity blocks in them still count toward summary.
	fallbackTexts []string
	// parseFailures counts chunks whose response yielded neither findings
	// nor a legacy severity block — unverifiable output.
	parseFailures int
	// legacy accumulates severity counts from responses that used the old
	// severity-block format instead of structured findings.
	legacy review.SeveritySummary
	// done records which chunk indices this agent actually completed, so a
	// partial review can name what it never looked at.
	done map[int]bool
	// unverifiedFindings counts published findings that skipped the
	// verification pass because they were below the gating severity. Set by
	// finalize, after dedupe, so it matches what the reader actually sees.
	unverifiedFindings int
	// err is the first hard error seen; only reported when nothing completed.
	err error
}

// chunkResult is one agent's result for one diff chunk.
type chunkResult struct {
	findings     []review.Finding
	dismissed    []review.Dismissed
	notes        string
	fallbackText string
	parseFailure bool
	legacy       review.SeveritySummary
	err          error
}

// merge folds one chunk's result into the agent's accumulated outcome.
func (o *agentOutcome) merge(chunk int, r chunkResult) {
	if r.err != nil {
		if o.err == nil {
			o.err = r.err
		}
		return
	}
	o.done[chunk] = true
	o.findings = append(o.findings, r.findings...)
	o.dismissed = append(o.dismissed, r.dismissed...)
	o.fallbackTexts = append(o.fallbackTexts, nonEmpty(r.fallbackText)...)
	o.notes = append(o.notes, nonEmpty(r.notes)...)
	if r.parseFailure {
		o.parseFailures++
	}
	o.legacy.Critical += r.legacy.Critical
	o.legacy.High += r.legacy.High
	o.legacy.Medium += r.legacy.Medium
	o.legacy.Low += r.legacy.Low
}

// finalize dedupes findings and computes the severity summary. Ordering is
// restored here because chunks complete concurrently. verify reports whether
// the verification pass was enabled, which decides whether the surviving
// medium/low findings are worth flagging as unverified.
func (o *agentOutcome) finalize(verify bool) {
	o.findings = review.DedupeFindings(o.findings)
	if verify {
		_, advisory := partitionGating(o.findings)
		o.unverifiedFindings = len(advisory)
	}
	o.summary = review.SummaryFromFindings(o.findings)
	o.summary.Critical += o.legacy.Critical
	o.summary.High += o.legacy.High
	o.summary.Medium += o.legacy.Medium
	o.summary.Low += o.legacy.Low
}

// unreviewedFiles returns the files in chunks this agent never completed.
// A file split across several chunks may have been partly reviewed, so it is
// listed once and only when at least one of its chunks was missed.
func (o *agentOutcome) unreviewedFiles(diffChunks []string) []string {
	var files []string
	seen := make(map[string]bool)
	for i, c := range diffChunks {
		if o.done[i] {
			continue
		}
		for _, f := range diff.FilePaths(c) {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	return files
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// chunkInput is the per-chunk half of a review prompt.
type chunkInput struct {
	diff        string
	fileContext string
	index       int // 0-based
	total       int
	previous    string
}

// runChunk performs one agent's review of one diff chunk: the review call,
// plus a verification call when the chunk produced findings that would gate
// the check.
func runChunk(ctx context.Context, p provider.Provider, model string, base review.PromptInput, agent config.AgentConfig, c chunkInput, verify bool) chunkResult {
	slog.Info("agent chunk", "subagent", agent.Subagent, "chunk", c.index+1, "total", c.total)

	in := base
	in.Agent = agent
	in.Diff = c.diff
	in.ChunkNote = review.ChunkNote(c.index+1, c.total)
	in.PreviousReport = c.previous
	in.FileContext = c.fileContext

	resp, err := p.Generate(ctx, model, review.BuildPrompt(in))
	if err != nil {
		return chunkResult{err: fmt.Errorf("chunk %d: %w", c.index+1, err)}
	}

	findings, notes, err := review.ParseFindings(resp.Text)
	if err != nil {
		// No findings block. Accept a legacy severity-count block so a model
		// that ignored the format still gates correctly; anything else is
		// unverifiable and counted, not swallowed — it must not silently read
		// as "no issues found".
		res := chunkResult{fallbackText: resp.Text}
		if summary, _, serr := review.ParseSeverity(resp.Text); serr == nil {
			res.legacy = summary
		} else {
			res.parseFailure = true
			slog.Warn("could not parse agent response", "subagent", agent.Subagent, "chunk", c.index+1, "error", err)
		}
		return res
	}

	res := chunkResult{notes: notes}

	// Verify only the findings that actually gate this check. The pass costs
	// a full extra provider round-trip per chunk, and its value is filtering
	// false positives out of results that fail CI — medium and low findings
	// are advisory, so paying to re-examine them doubles review time for no
	// change in outcome. They are published marked as unverified.
	gating, advisory := partitionGating(findings)
	if verify && len(gating) > 0 {
		verified, dismissed, verr := verifyFindings(ctx, p, model, agent, c.diff, gating)
		if verr != nil {
			slog.Warn("verification pass failed; keeping unverified findings",
				"subagent", agent.Subagent, "chunk", c.index+1, "error", verr)
		} else {
			slog.Info("verification pass", "subagent", agent.Subagent, "chunk", c.index+1,
				"draft", len(gating), "kept", len(verified), "dismissed", len(dismissed))
			gating = verified
			res.dismissed = dismissed
		}
	}

	res.findings = append(gating, advisory...)
	return res
}

// partitionGating splits findings into those that fail the check
// (critical/high) and those that are advisory (medium/low).
func partitionGating(findings []review.Finding) (gating, advisory []review.Finding) {
	for _, f := range findings {
		if f.Severity == "critical" || f.Severity == "high" {
			gating = append(gating, f)
		} else {
			advisory = append(advisory, f)
		}
	}
	return gating, advisory
}

// verifyFindings runs the self-verification pass for one chunk's findings.
func verifyFindings(ctx context.Context, p provider.Provider, model string, agent config.AgentConfig, chunk string, findings []review.Finding) ([]review.Finding, []review.Dismissed, error) {
	prompt := review.BuildVerificationPrompt(agent, chunk, findings)
	resp, err := p.Generate(ctx, model, prompt)
	if err != nil {
		return nil, nil, err
	}
	return review.ParseVerification(resp.Text)
}

// findingMarkerPattern matches the idempotency marker embedded in inline
// finding comments (see review.FindingMarker).
var findingMarkerPattern = regexp.MustCompile(`<!-- wd-auto-review:finding=[^ ]+ -->`)

// maxInlineComments caps inline comments posted per run so a pathological
// review can't bury a PR; everything is always in the summary comment anyway.
const maxInlineComments = 50

// inlinePoster posts findings as inline PR review comments. Shared across
// agent goroutines; bookkeeping is mutex-guarded, HTTP calls are not held
// under the lock.
type inlinePoster struct {
	mu       sync.Mutex
	gh       *github.Client
	pr       int
	headSHA  string
	valid    map[string]map[int]bool // diff positions inline comments can anchor to
	existing map[string]bool         // finding markers already on the PR
	budget   int
}

func (ip *inlinePoster) post(ctx context.Context, agent string, findings []review.Finding) {
	for _, f := range findings {
		if !ip.valid[f.File][f.Line] {
			slog.Debug("finding does not map to a diff line; summary comment only",
				"agent", agent, "file", f.File, "line", f.Line)
			continue
		}
		marker := review.FindingMarker(agent, f)

		ip.mu.Lock()
		if ip.existing[marker] || ip.budget <= 0 {
			ip.mu.Unlock()
			continue
		}
		ip.existing[marker] = true
		ip.budget--
		ip.mu.Unlock()

		body := review.FormatInlineComment(agent, f)
		if err := ip.gh.CreateReviewComment(ctx, ip.pr, ip.headSHA, f.File, f.Line, body); err != nil {
			// Best-effort: the finding is still in the summary comment.
			slog.Warn("could not post inline comment", "agent", agent, "file", f.File, "line", f.Line, "error", err)
		}
	}
}
