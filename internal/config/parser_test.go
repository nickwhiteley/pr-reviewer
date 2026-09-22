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

func TestParseAgents_pathsColumn(t *testing.T) {
	cfg := mustParse(t, `## Who owns the repo
Nick

## Context and intent
A thing.

## Production Status
Live.

## Security Level
High

## Code Reviews
| Plugin | Subagent      | Paths                          | Additional      |
| ------ | ------------- | ------------------------------ | --------------- |
| qa     | postgres-pro  | `+"`api/internal/store/`, `*.sql`"+` | watch the CAS   |
| qa     | code-reviewer |                                | everything else |

## Excluded Paths
- `+"`spec.md`"+`
- `+"`specs/`"+`
`)

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	pg := cfg.Agents[0]
	if pg.Subagent != "postgres-pro" {
		t.Fatalf("agent 0 = %q", pg.Subagent)
	}
	if len(pg.Paths) != 2 || pg.Paths[0] != "api/internal/store/" || pg.Paths[1] != "*.sql" {
		t.Errorf("Paths = %#v, want the two patterns unquoted", pg.Paths)
	}
	if pg.Additional != "watch the CAS" {
		t.Errorf("Additional = %q — the Paths column must not shift it", pg.Additional)
	}
	if len(cfg.Agents[1].Paths) != 0 {
		t.Errorf("an empty Paths cell must mean the whole diff, got %#v", cfg.Agents[1].Paths)
	}
	if len(cfg.ExcludedPaths) != 2 || cfg.ExcludedPaths[0] != "spec.md" {
		t.Errorf("ExcludedPaths = %#v", cfg.ExcludedPaths)
	}
}

// Columns are addressed by name, so a table that puts Additional before
// Paths — or omits Paths entirely, as every config did before it existed —
// parses the same.
func TestParseAgents_columnOrderAndLegacyTables(t *testing.T) {
	legacy := mustParse(t, header+`
## Code Reviews
| Plugin | Subagent | Additional |
| ------ | -------- | ---------- |
| qa     | rev      | focus here |
`)
	if len(legacy.Agents) != 1 || legacy.Agents[0].Additional != "focus here" || len(legacy.Agents[0].Paths) != 0 {
		t.Errorf("legacy three-column table parsed as %#v", legacy.Agents)
	}

	swapped := mustParse(t, header+`
## Code Reviews
| Plugin | Subagent | Additional | Paths  |
| ------ | -------- | ---------- | ------ |
| qa     | rev      | focus here | api/   |
`)
	if len(swapped.Agents) != 1 {
		t.Fatalf("got %d agents", len(swapped.Agents))
	}
	if swapped.Agents[0].Additional != "focus here" {
		t.Errorf("Additional = %q", swapped.Agents[0].Additional)
	}
	if len(swapped.Agents[0].Paths) != 1 || swapped.Agents[0].Paths[0] != "api/" {
		t.Errorf("Paths = %#v", swapped.Agents[0].Paths)
	}
}

const header = `## Who owns the repo
Nick

## Context and intent
A thing.

## Production Status
Live.

## Security Level
High
`

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := ParseBytes([]byte(src))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return cfg
}

func TestAgentScope_negation(t *testing.T) {
	a := AgentConfig{Paths: []string{"api/", "*.sql", "!*_test.go", "!e2e/"}}
	include, exclude := a.Scope()

	if len(include) != 2 || include[0] != "api/" || include[1] != "*.sql" {
		t.Errorf("include = %#v", include)
	}
	if len(exclude) != 2 || exclude[0] != "*_test.go" || exclude[1] != "e2e/" {
		t.Errorf("exclude = %#v — the ! must be stripped", exclude)
	}

	// Negations alone mean "everything except", so include stays empty and
	// the caller's empty-include rule keeps the whole diff.
	only := AgentConfig{Paths: []string{"!*_test.go"}}
	if inc, exc := only.Scope(); len(inc) != 0 || len(exc) != 1 {
		t.Errorf("negation-only scope = include %#v, exclude %#v", inc, exc)
	}
}
