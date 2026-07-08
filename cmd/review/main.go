package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
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
	)
	flag.Parse()

	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)

	if *prNum == 0 || *repo == "" || *base == "" || *head == "" {
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
		p = provider.NewAnthropicProvider()
	case "ollama":
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

	// Setup GitHub client
	gh := github.NewClient(*repo)

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
	if err := gh.PostOrUpdate(ctx, *prNum, "<!-- wd-auto-review:type=hygiene -->", hygieneReport); err != nil {
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
	if cs, err := gh.ListComments(ctx, *prNum); err != nil {
		slog.Warn("could not list existing comments for previous-review context", "error", err)
	} else {
		existingComments = cs
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

	// Build a context with a timeout for the AI review phase.
	// On expiry we post a PR comment and exit 0 rather than failing the pipeline.
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
	)

	for _, agent := range cfg.Agents {
		wg.Add(1)
		go func(agent config.AgentConfig) {
			defer wg.Done()

			slog.Info("running agent", "subagent", agent.Subagent, "chunks", len(diffChunks), "provider", *providerName, "model", *model)
			previousReport := previousReportFor(agent)
			combinedSummary, combinedText, err := runAgentChunks(reviewCtx, p, *model, cfg, agent, diffChunks, coverageSummary, previousReport)
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
				if postErr := gh.PostOrUpdate(ctx, *prNum, marker, failureReport); postErr != nil {
					slog.Error("failed to post agent failure comment", "subagent", agent.Subagent, "error", postErr)
				}

				mu.Lock()
				failedAgents++
				mu.Unlock()
				return
			}

			mu.Lock()
			if combinedSummary.Critical > 0 || combinedSummary.High > 0 {
				hasCriticalOrHigh = true
			}
			mu.Unlock()

			report := review.FormatAgentReport(agent, combinedSummary, combinedText+skippedChunksNote)
			marker := fmt.Sprintf("<!-- wd-auto-review:agent=%s -->", agent.Subagent)
			if err := gh.PostOrUpdate(ctx, *prNum, marker, report); err != nil {
				slog.Error("failed to post agent comment", "subagent", agent.Subagent, "error", err)
				// Do not fail the pipeline for posting errors; the review result is still valid.
			}
		}(agent)
	}

	wg.Wait()

	// If the review timed out, post a single comment and exit successfully.
	if errors.Is(reviewCtx.Err(), context.DeadlineExceeded) {
		timeoutReport := formatTimeoutReport(*reviewTimeout, *providerName)
		if postErr := gh.PostOrUpdate(ctx, *prNum, "<!-- wd-auto-review:type=timeout -->", timeoutReport); postErr != nil {
			slog.Error("failed to post timeout comment", "error", postErr)
		}
		slog.Info("review timed out — exiting 0", "timeout_seconds", *reviewTimeout)
		return
	}

	if failedAgents > 0 {
		slog.Info("review complete with agent failures", "failed", failedAgents)
	}
	if hasCriticalOrHigh {
		slog.Info("review complete with critical/high findings")
		os.Exit(1)
	}

	slog.Info("review complete")
}

// formatTimeoutReport returns a PR comment body explaining that the review timed out.
func formatTimeoutReport(timeoutSecs int, providerName string) string {
	return fmt.Sprintf(`## ⏱ PR Review — Agent Timeout

The AI review agents did not complete within the configured timeout (%ds via the **%s** provider).

This is **not a CI failure** — your other checks have passed. The code changes may still require manual review.

**To retry:** re-run the *PR Review* workflow job from the GitHub Actions tab.
`, timeoutSecs, providerName)
}

// runAgentChunks processes all diff chunks for a single agent and combines results.
// Chunks are processed sequentially within an agent to avoid overwhelming the provider.
// previousReport is this same agent's report from an earlier run on this PR (empty if
// none), passed into every chunk's prompt so the agent can avoid blindly re-raising
// findings that were already fixed or explained.
func runAgentChunks(ctx context.Context, p provider.Provider, model string, cfg *config.Config, agent config.AgentConfig, diffChunks []string, coverageSummary string, previousReport string) (review.SeveritySummary, string, error) {
	var combined review.SeveritySummary
	var combinedText strings.Builder

	for i, chunk := range diffChunks {
		if ctx.Err() != nil {
			return combined, combinedText.String(), ctx.Err()
		}

		slog.Info("agent chunk", "subagent", agent.Subagent, "chunk", i+1, "total", len(diffChunks))
		chunkNote := review.ChunkNote(i+1, len(diffChunks))
		prompt := review.BuildPrompt(cfg, agent, chunk, coverageSummary, chunkNote, previousReport)

		resp, err := p.Generate(ctx, model, prompt)
		if err != nil {
			return combined, combinedText.String(), fmt.Errorf("chunk %d: %w", i+1, err)
		}

		summary, _, err := review.ParseSeverity(resp.Text)
		if err != nil {
			slog.Warn("could not parse severity", "subagent", agent.Subagent, "chunk", i+1, "error", err)
		}

		combined.Critical += summary.Critical
		combined.High += summary.High
		combined.Medium += summary.Medium
		combined.Low += summary.Low

		if combinedText.Len() > 0 {
			combinedText.WriteString("\n\n---\n\n")
		}
		combinedText.WriteString(fmt.Sprintf("### Chunk %d/%d\n\n", i+1, len(diffChunks)))
		combinedText.WriteString(resp.Text)
	}

	return combined, combinedText.String(), nil
}
