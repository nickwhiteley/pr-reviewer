package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nickwhiteley/pr-reviewer/internal/config"
	"github.com/nickwhiteley/pr-reviewer/internal/diff"
	"github.com/nickwhiteley/pr-reviewer/internal/github"
	"github.com/nickwhiteley/pr-reviewer/internal/hygiene"
	"github.com/nickwhiteley/pr-reviewer/internal/provider"
	"github.com/nickwhiteley/pr-reviewer/internal/review"
)

func main() {
	var (
		prNum        = flag.Int("pr", 0, "Pull request number")
		repo         = flag.String("repo", "", "Repository in owner/name format")
		base         = flag.String("base", "", "Base commit SHA")
		head         = flag.String("head", "", "Head commit SHA")
		model        = flag.String("model", "kimi-k2.6:cloud", "Ollama model to use")
		debug        = flag.Bool("debug", false, "Enable debug logging")
		configPath   = flag.String("config", "PR-REVIEW.md", "Path to PR-REVIEW.md")
		skipAgents   = flag.Bool("skip-agents", false, "Skip AI agent reviews (hygiene only)")
		coverageFile = flag.String("coverage-file", "", "Path to go tool cover -func output to include in agent prompts")
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
	runner := hygiene.NewRunner()
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

	// Prepare diff chunks
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

	// Run code review agents in parallel.
	// Each agent is independent — a failure in one does not cancel the others.
	// We create a fresh OllamaProvider per agent so that transport-level
	// timeouts and connection state are fully isolated.

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

			ollama := provider.NewOllamaProvider()
			slog.Info("running agent", "subagent", agent.Subagent, "chunks", len(diffChunks))
			combinedSummary, combinedText, err := runAgentChunks(ctx, ollama, *model, cfg, agent, diffChunks, coverageSummary)
			if err != nil {
				slog.Error("agent failed", "subagent", agent.Subagent, "error", err)

				// Post a failure comment so the PR shows which agent did not finish.
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

			report := review.FormatAgentReport(agent, combinedSummary, combinedText)
			marker := fmt.Sprintf("<!-- wd-auto-review:agent=%s -->", agent.Subagent)
			if err := gh.PostOrUpdate(ctx, *prNum, marker, report); err != nil {
				slog.Error("failed to post agent comment", "subagent", agent.Subagent, "error", err)
				// Do not fail the pipeline for posting errors; the review result is still valid.
			}
		}(agent)
	}

	wg.Wait()

	if failedAgents > 0 {
		slog.Info("review complete with agent failures", "failed", failedAgents)
	}
	if hasCriticalOrHigh {
		slog.Info("review complete with critical/high findings")
		os.Exit(1)
	}

	slog.Info("review complete")
}

// runAgentChunks processes all diff chunks for a single agent and combines results.
// Chunks are processed sequentially within an agent to avoid overwhelming the provider.
func runAgentChunks(ctx context.Context, ollama *provider.OllamaProvider, model string, cfg *config.Config, agent config.AgentConfig, diffChunks []string, coverageSummary string) (review.SeveritySummary, string, error) {
	var combined review.SeveritySummary
	var combinedText strings.Builder

	for i, chunk := range diffChunks {
		if ctx.Err() != nil {
			return combined, combinedText.String(), ctx.Err()
		}

		slog.Info("agent chunk", "subagent", agent.Subagent, "chunk", i+1, "total", len(diffChunks))
		prompt := review.BuildPrompt(cfg, agent, chunk, coverageSummary)

		resp, err := ollama.Generate(ctx, model, prompt)
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
