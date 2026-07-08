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
	)
	flag.Parse()

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

	// Prepare diff chunks. diff.Compute returns the full diff, untruncated —
	// chunking (not a whole-diff byte cutoff) is what handles large PRs, so
	// that no file is ever silently invisible to every chunk. The only cap
	// left is on the NUMBER of chunks actually sent to a provider, below,
	// which is a cost/time control, not a correctness one, and reports
	// honestly what it skipped.
	var diffChunks []string
	if len(prDiff) <= diff.MaxChunkSize {
		diffChunks = []string{prDiff}
	} else {
		files := diff.SplitFiles(prDiff)
		chunks := diff.ChunkFiles(files, diff.MaxChunkSize)
		slog.Info("diff chunked", "files", len(files), "chunks", len(chunks))
		for i, c := range chunks {
			diffChunks = append(diffChunks, strings.Join(c, "\n"))
			slog.Debug("chunk size", "chunk", i, "bytes", len(diffChunks[i]))
		}
	}

	// Cap the number of chunks actually reviewed per agent — a genuinely
	// pathological diff (accidentally-committed vendor dump, huge generated
	// file that slipped past exclusions, etc.) shouldn't turn into dozens of
	// sequential provider calls per agent. Unlike the old whole-diff byte
	// truncation this removed, it's honest about what it skips instead of
	// silently cutting content off mid-file and letting an agent guess.
	const maxChunksPerAgent = 12
	var skippedChunksNote string
	if len(diffChunks) > maxChunksPerAgent {
		slog.Warn("diff has more chunks than the per-agent cap; skipping the rest",
			"total_chunks", len(diffChunks), "cap", maxChunksPerAgent)
		skippedChunksNote = fmt.Sprintf(
			"\n\n---\n\n**Note**: this PR's diff produced %d chunks; only the first %d were reviewed "+
				"(cost/time cap per agent). Files beyond that point were not reviewed by this tool. "+
				"Consider splitting this PR, or request a manual review of the remaining files.\n",
			len(diffChunks), maxChunksPerAgent,
		)
		diffChunks = diffChunks[:maxChunksPerAgent]
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
				return cm.Body
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
	// runAgentChunks. Everything here is best-effort context: a fetch
	// failure degrades the review, it doesn't abort it.
	basePrompt := review.PromptInput{
		Cfg:             cfg,
		CoverageSummary: coverageSummary,
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

	// Run code review agents in parallel.
	// Each agent is independent — a failure in one does not cancel the others.
	var (
		wg                sync.WaitGroup
		mu                sync.Mutex
		hasCriticalOrHigh bool
		failedAgents      int
		unverifiedAgents  int
	)

	for _, agent := range cfg.Agents {
		wg.Add(1)
		go func(agent config.AgentConfig) {
			defer wg.Done()

			slog.Info("running agent", "subagent", agent.Subagent, "chunks", len(diffChunks), "provider", *providerName, "model", *model)
			previousReport := previousReportFor(agent)
			outcome, err := runAgentChunks(reviewCtx, p, *model, basePrompt, agent, diffChunks, previousReport, *verify, *fileContext)
			if err != nil {
				// If the review context expired, suppress individual failure comments —
				// a single timeout comment will be posted after all goroutines finish.
				if reviewCtx.Err() != nil {
					mu.Lock()
					failedAgents++
					mu.Unlock()
					return
				}

				slog.Error("agent failed", "subagent", agent.Subagent, "error", err)

				failureReport := review.FormatAgentFailureReport(agent, err)
				marker := fmt.Sprintf("<!-- wd-auto-review:agent=%s -->", agent.Subagent)
				if postErr := sink.PostOrUpdate(ctx, *prNum, marker, failureReport); postErr != nil {
					slog.Error("failed to post agent failure comment", "subagent", agent.Subagent, "error", postErr)
				}

				mu.Lock()
				failedAgents++
				mu.Unlock()
				return
			}

			mu.Lock()
			if outcome.summary.Critical > 0 || outcome.summary.High > 0 {
				hasCriticalOrHigh = true
			}
			if outcome.parseFailures > 0 {
				unverifiedAgents++
			}
			mu.Unlock()

			body := review.RenderFindings(outcome.findings, outcome.dismissed, outcome.notes)
			for _, raw := range outcome.fallbackTexts {
				body += "\n\n---\n\n### Unstructured agent output\n\n" + raw + "\n"
			}
			if outcome.parseFailures > 0 {
				body += fmt.Sprintf(
					"\n\n---\n\n⚠️ **Results could not be parsed for %d of %d chunk(s)** — any findings there are "+
						"shown as raw output above but are NOT reflected in the severity summary. Treat this review as incomplete.\n",
					outcome.parseFailures, len(diffChunks),
				)
			}

			report := review.FormatAgentReport(agent, outcome.summary, body+skippedChunksNote)
			marker := fmt.Sprintf("<!-- wd-auto-review:agent=%s -->", agent.Subagent)
			if err := sink.PostOrUpdate(ctx, *prNum, marker, report); err != nil {
				slog.Error("failed to post agent comment", "subagent", agent.Subagent, "error", err)
				// Do not fail the pipeline for posting errors; the review result is still valid.
			}

			if poster != nil {
				poster.post(ctx, agent.Subagent, outcome.findings)
			}
		}(agent)
	}

	wg.Wait()

	// If the review timed out, post a single comment. In closed mode this
	// fails the check: an unreviewed PR must not show green (spec FR-013).
	if errors.Is(reviewCtx.Err(), context.DeadlineExceeded) {
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
	if failedAgents > 0 || unverifiedAgents > 0 {
		if failClosed {
			slog.Error("review incomplete — failing check (fail-mode=closed)",
				"failed_agents", failedAgents, "unverified_agents", unverifiedAgents)
			os.Exit(1)
		}
		slog.Info("review incomplete — exiting 0 (fail-mode=open)",
			"failed_agents", failedAgents, "unverified_agents", unverifiedAgents)
		return
	}

	slog.Info("review complete")
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

// agentOutcome is the combined result of one agent across all diff chunks.
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
}

// runAgentChunks processes all diff chunks for a single agent and combines results.
// Chunks are processed sequentially within an agent to avoid overwhelming the provider.
// previousReport is this same agent's report from an earlier run on this PR (empty if
// none), passed into every chunk's prompt so the agent can avoid blindly re-raising
// findings that were already fixed or explained. When verify is true, each chunk's
// findings go through a second self-verification pass that drops unsupported ones.
func runAgentChunks(ctx context.Context, p provider.Provider, model string, base review.PromptInput, agent config.AgentConfig, diffChunks []string, previousReport string, verify bool, fileContext bool) (agentOutcome, error) {
	var out agentOutcome
	var legacySummary review.SeveritySummary

	for i, chunk := range diffChunks {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}

		slog.Info("agent chunk", "subagent", agent.Subagent, "chunk", i+1, "total", len(diffChunks))
		in := base
		in.Agent = agent
		in.Diff = chunk
		in.ChunkNote = review.ChunkNote(i+1, len(diffChunks))
		in.PreviousReport = previousReport
		if fileContext {
			in.FileContext = buildFileContext(diff.FilePaths(chunk))
		}
		prompt := review.BuildPrompt(in)

		resp, err := p.Generate(ctx, model, prompt)
		if err != nil {
			return out, fmt.Errorf("chunk %d: %w", i+1, err)
		}

		findings, notes, err := review.ParseFindings(resp.Text)
		if err != nil {
			// No findings block. Accept a legacy severity-count block so a
			// model that ignored the format still gates correctly; anything
			// else is unverifiable and counted, not swallowed — it must not
			// silently read as "no issues found".
			if summary, _, serr := review.ParseSeverity(resp.Text); serr == nil {
				legacySummary.Critical += summary.Critical
				legacySummary.High += summary.High
				legacySummary.Medium += summary.Medium
				legacySummary.Low += summary.Low
			} else {
				out.parseFailures++
				slog.Warn("could not parse agent response", "subagent", agent.Subagent, "chunk", i+1, "error", err)
			}
			out.fallbackTexts = append(out.fallbackTexts, resp.Text)
			continue
		}

		if verify && len(findings) > 0 {
			verified, dismissed, verr := verifyFindings(ctx, p, model, agent, chunk, findings)
			if verr != nil {
				slog.Warn("verification pass failed; keeping unverified findings",
					"subagent", agent.Subagent, "chunk", i+1, "error", verr)
			} else {
				slog.Info("verification pass", "subagent", agent.Subagent, "chunk", i+1,
					"draft", len(findings), "kept", len(verified), "dismissed", len(dismissed))
				findings = verified
				out.dismissed = append(out.dismissed, dismissed...)
			}
		}

		out.findings = append(out.findings, findings...)
		if notes != "" {
			out.notes = append(out.notes, notes)
		}
	}

	out.findings = review.DedupeFindings(out.findings)
	out.summary = review.SummaryFromFindings(out.findings)
	out.summary.Critical += legacySummary.Critical
	out.summary.High += legacySummary.High
	out.summary.Medium += legacySummary.Medium
	out.summary.Low += legacySummary.Low

	return out, nil
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
