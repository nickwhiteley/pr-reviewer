package config

import (
	"testing"
)

func TestParseBytes_valid(t *testing.T) {
	input := `
## Who owns the repo
Owned by Nick
## Context and intent
Internal market system
## Production status
Deployed
## Security level
High
## PR Hygiene
- [x] H001 README.md is present and up to date
- [ ] H002 codeowners exists
## Code reviews
| Plugin | Subagent | Additional |
| voltagent-qa-sec | architect-reviewer | focus on api |
`
	cfg, err := ParseBytes([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Owner != "Owned by Nick" {
		t.Errorf("owner = %q, want %q", cfg.Owner, "Owned by Nick")
	}
	if cfg.Context != "Internal market system" {
		t.Errorf("context = %q", cfg.Context)
	}
	if cfg.SecurityLevel != "High" {
		t.Errorf("security level = %q", cfg.SecurityLevel)
	}
	if len(cfg.Hygiene) != 2 {
		t.Fatalf("expected 2 hygiene checks, got %d", len(cfg.Hygiene))
	}
	if !cfg.Hygiene[0].Ticked {
		t.Error("expected H001 ticked")
	}
	if cfg.Hygiene[1].Ticked {
		t.Error("expected H002 not ticked")
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(cfg.Agents))
	}
	if cfg.Agents[0].Subagent != "architect-reviewer" {
		t.Errorf("agent = %q", cfg.Agents[0].Subagent)
	}
}

func TestParseBytes_missingRequired(t *testing.T) {
	_, err := ParseBytes([]byte(""))
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

// TestParse_exampleFile guards against the parser drifting out of sync with
// the example config that users are told to copy into their repositories.
func TestParse_exampleFile(t *testing.T) {
	cfg, err := Parse("../../example/PR-REVIEW.md")
	if err != nil {
		t.Fatalf("example/PR-REVIEW.md failed to parse: %v", err)
	}
	if cfg.Owner == "" {
		t.Error("example owner not parsed")
	}
	if cfg.Context == "" {
		t.Error("example context not parsed")
	}
	if len(cfg.Hygiene) != 4 {
		t.Errorf("expected 4 hygiene checks in example, got %d", len(cfg.Hygiene))
	}
	if len(cfg.Agents) != 4 {
		t.Errorf("expected 4 agents in example, got %d", len(cfg.Agents))
	}
}

func TestParseBytes_codeownersSectionDoesNotClobberOwner(t *testing.T) {
	cfg, err := ParseBytes([]byte(`## Owner
Nick

## Codeowners
platform-team

## Context
App

## Production Status
Dev

## Security Level
Low
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Owner != "Nick" {
		t.Errorf("owner = %q, clobbered by Codeowners section", cfg.Owner)
	}
}
