package review

import (
	"strings"
	"testing"

	"github.com/nickwhiteley/pr-reviewer/internal/config"
)

func TestBuildPrompt(t *testing.T) {
	cfg := &config.Config{
		Owner:            "Nick",
		Context:          "Test app",
		ProductionStatus: "Dev",
		SecurityLevel:    "Low",
	}
	agent := config.AgentConfig{
		Subagent:   "code-reviewer",
		Additional: "focus on errors",
	}
	prompt := BuildPrompt(cfg, agent, "diff content")
	if !strings.Contains(prompt, "Nick") {
		t.Error("prompt missing owner")
	}
	if !strings.Contains(prompt, "code-reviewer") {
		t.Error("prompt missing agent name")
	}
	if !strings.Contains(prompt, "focus on errors") {
		t.Error("prompt missing additional instructions")
	}
	if !strings.Contains(prompt, "diff content") {
		t.Error("prompt missing diff")
	}
}

func TestParseSeverity_valid(t *testing.T) {
	text := `Here is the review.
{"critical": 1, "high": 2, "medium": 3, "low": 4}
Details follow...`
	summary, _, err := ParseSeverity(text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Critical != 1 || summary.High != 2 || summary.Medium != 3 || summary.Low != 4 {
		t.Errorf("unexpected severity: %+v", summary)
	}
}

func TestParseSeverity_missing(t *testing.T) {
	text := "No JSON here"
	_, _, err := ParseSeverity(text)
	if err == nil {
		t.Fatal("expected error for missing JSON")
	}
}

func TestParseSeverity_malformed(t *testing.T) {
	text := `{"critical": abc}`
	_, _, err := ParseSeverity(text)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestParseSeverity_extraText(t *testing.T) {
	text := `Some text
` + "```json\n" + `{"critical": 0, "high": 1}` + "\n```\nMore text"
	summary, _, err := ParseSeverity(text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.High != 1 {
		t.Errorf("expected high=1, got %d", summary.High)
	}
}
