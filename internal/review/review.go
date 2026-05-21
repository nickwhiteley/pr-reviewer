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
func BuildPrompt(cfg *config.Config, agent config.AgentConfig, diff string) string {
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
	b.WriteString("Then provide your detailed findings in markdown. Use headings, bullet points, and code blocks where appropriate.\n")

	return b.String()
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
