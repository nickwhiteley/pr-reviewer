// Package review builds agent prompts and parses severity from responses.
package review

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickwhiteley/pr-reviewer/internal/config"
)

// SeveritySummary is the structured severity block from an agent response.
type SeveritySummary struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

// Total returns the sum of all severity counts.
func (s SeveritySummary) Total() int {
	return s.Critical + s.High + s.Medium + s.Low
}

// BuildPrompt constructs the full prompt for an agent.
// coverageSummary is optional; when non-empty it is injected into the prompt
// so agents can flag low-coverage areas in the diff.
// chunkNote is optional; when non-empty it tells the agent it's seeing part
// of a larger diff (see ChunkNote).
// previousReport is optional; when non-empty it's this same agent's report
// from an earlier run on this PR, so the agent can avoid blindly re-raising
// findings that were already addressed or explained (see PreviousReviewNote).
func BuildPrompt(cfg *config.Config, agent config.AgentConfig, diff, coverageSummary string, chunkNote string, previousReport string) string {
	var b strings.Builder

	b.WriteString("You are a code review agent. Review the following pull request diff and report findings.\n\n")

	b.WriteString("## Project Context\n\n")
	b.WriteString(fmt.Sprintf("**Owner**: %s\n", cfg.Owner))
	b.WriteString(fmt.Sprintf("**Context**: %s\n", cfg.Context))
	b.WriteString(fmt.Sprintf("**Production Status**: %s\n", cfg.ProductionStatus))
	if cfg.ConnectedSystems != "" {
		b.WriteString(fmt.Sprintf("**Connected Systems**: %s\n", cfg.ConnectedSystems))
	}
	if cfg.IntendedAudience != "" {
		b.WriteString(fmt.Sprintf("**Intended Audience**: %s\n", cfg.IntendedAudience))
	}
	if cfg.AuditLevel != "" {
		b.WriteString(fmt.Sprintf("**Audit Level**: %s\n", cfg.AuditLevel))
	}
	b.WriteString(fmt.Sprintf("**Security Level**: %s\n\n", cfg.SecurityLevel))

	if chunkNote != "" {
		b.WriteString("## Diff Coverage Notice\n\n")
		b.WriteString(chunkNote)
		b.WriteString("\n\n")
	}

	if previousReport != "" {
		b.WriteString("## Your Previous Review Of This PR\n\n")
		b.WriteString(
			"You (this same agent role) already reviewed an earlier version of this PR — its report is quoted below. " +
				"The PR has since changed; the diff you're about to review reflects the CURRENT state only. " +
				"Do not restate a previous finding unless the current diff still shows evidence for it. " +
				"If the code now looks correct, or if a human explanation in a comment reply resolves it, do not re-count it toward your severity summary. " +
				"Treat this as your own prior work to reconsider, not as ground truth to defend.\n\n",
		)
		b.WriteString("```\n")
		b.WriteString(previousReport)
		b.WriteString("\n```\n\n")
	}

	if coverageSummary != "" {
		b.WriteString("## Test Coverage\n\n```\n")
		b.WriteString(coverageSummary)
		b.WriteString("\n```\n\n")
	}

	b.WriteString(fmt.Sprintf("## Agent Role: %s\n\n", agent.Subagent))
	if agent.Additional != "" {
		b.WriteString(fmt.Sprintf("**Additional Focus**: %s\n\n", agent.Additional))
	}

	b.WriteString("## Diff\n\n```diff\n")
	b.WriteString(diff)
	b.WriteString("\n```\n\n")

	b.WriteString("## Instructions\n\n")
	b.WriteString("Analyze the diff for issues related to your role. Output a JSON severity summary block BEFORE your markdown report, like:\n")
	b.WriteString("```json\n")
	b.WriteString(`{"critical": 0, "high": 0, "medium": 0, "low": 0}` + "\n")
	b.WriteString("```\n\n")
	b.WriteString("Then provide your detailed findings in markdown. Use headings, bullet points, and code blocks where appropriate.\n\n")

	b.WriteString("## Severity Discipline\n\n")
	b.WriteString(
		"Only assign CRITICAL or HIGH severity when the diff shown gives POSITIVE evidence of the issue — an actual " +
			"line of code that is wrong, missing, or unsafe. Do NOT assign CRITICAL or HIGH based on the mere absence " +
			"of a file, function, or check from what you can see (phrasing like \"if X is not validated elsewhere\", " +
			"\"assuming Y is not implemented\", or \"the diff does not show...\" is a signal you're about to do this — " +
			"stop and downgrade instead). Code you cannot see is not evidence that it doesn't exist, especially when " +
			"this diff was chunked (see the Diff Coverage Notice above, if present) or when the changed lines are a " +
			"small part of a larger existing function. If you suspect an issue but cannot confirm it from what's " +
			"shown, report it as LOW severity, say explicitly what you could not see, and suggest what to check — " +
			"do not round it up to HIGH/CRITICAL to be safe.\n",
	)

	return b.String()
}

// ChunkNote returns the "Diff Coverage Notice" text for a diff split into
// multiple chunks by file, or "" when there's only one chunk (nothing to
// note). index and total are 1-based.
func ChunkNote(index, total int) string {
	if total <= 1 {
		return ""
	}
	return fmt.Sprintf(
		"This diff was too large for one review pass and was split into %d chunks by file — you are reviewing "+
			"chunk %d of %d now. The other chunks contain other files from the SAME pull request, reviewed "+
			"separately by the same role; their findings are combined with yours afterward. A file, function, or "+
			"check that isn't shown in this chunk may simply be in a different chunk, not missing from the PR.",
		total, index, total,
	)
}

// ParseSeverity extracts the severity JSON block from the response text.
func ParseSeverity(text string) (SeveritySummary, string, error) {
	// Look for JSON block in backticks or inline
	// Pattern matches: ```json {"critical": 0, ...} ``` or just {"critical": 0, ...}
	pattern := "(?s)(?:```json\\s*)?(\\{[^}]*\"critical\"\\s*:\\s*\\d+[^}]*\\})(?:\\s*```)?"
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(text)
	if len(matches) < 2 {
		return SeveritySummary{}, text, fmt.Errorf("no severity JSON block found")
	}

	var summary SeveritySummary
	if err := json.Unmarshal([]byte(matches[1]), &summary); err != nil {
		return SeveritySummary{}, text, fmt.Errorf("unmarshal severity: %w", err)
	}

	return summary, text, nil
}

// FormatAgentReport returns a markdown comment for an agent report.
func FormatAgentReport(agent config.AgentConfig, summary SeveritySummary, report string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## Code Review: %s\n\n", agent.Subagent))

	b.WriteString("**Severity Summary**: ")
	if summary.Total() == 0 {
		b.WriteString("✅ No issues found\n")
	} else {
		b.WriteString(fmt.Sprintf("🔴 Critical: %d | 🟠 High: %d | 🟡 Medium: %d | 🟢 Low: %d\n",
			summary.Critical, summary.High, summary.Medium, summary.Low))
	}
	b.WriteString("\n")

	if report != "" {
		b.WriteString(report)
		b.WriteString("\n\n")
	}

	b.WriteString(fmt.Sprintf("<!-- wd-auto-review:agent=%s -->\n", agent.Subagent))
	return b.String()
}

// FormatAgentFailureReport returns a markdown comment when an agent fails to run.
func FormatAgentFailureReport(agent config.AgentConfig, err error) string {
	return fmt.Sprintf(
		"## Code Review: %s\n\n**Status**: ⚠️ Agent failed to complete\n\n"+
			"This review agent encountered an error and could not finish its analysis. "+
			"Other review agents may still have completed successfully.\n\n"+
			"**Error**: `%s`\n\n"+
			"<!-- wd-auto-review:agent=%s -->\n",
		agent.Subagent, err, agent.Subagent,
	)
}
