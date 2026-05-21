# Tasks: Automate PR Review

**Input**: Design documents from `specs/015-automate-pr-review/`
**Prerequisites**: plan.md, spec.md
**Branch**: `015-automate-pr-review`

**Tests**: Mandatory per project constitution (Principle V: Test Discipline). Unit and integration tests for all components, happy and unhappy paths.

**Organization**: Tasks grouped by user story to enable independent implementation and testing.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1, US2, US3, US4)
- Include exact file paths in descriptions

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Project initialization and basic structure

- [x] T001 Create Go module at `tools/pr-review/go.mod` with module path `github.com/nickwhiteley/woodendollars/tools/pr-review`
- [x] T002 [P] Create directory structure: `tools/pr-review/internal/{config,hygiene,provider,github,review,diff}/` and `tools/pr-review/tests/`
- [x] T003 [P] Add `.github/workflows/pr-review.yml` with trigger on `pull_request` to `main`
- [x] T004 Create `tools/pr-review/cmd/review/main.go` with flag parsing (`--pr`, `--repo`, `--base`, `--head`)
- [x] T005 Configure `tools/pr-review/Makefile` with targets for `build`, `test`, `lint`, `vet`

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure that MUST be complete before ANY user story can be implemented

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

### Tests for Foundational (MANDATORY)

- [x] T006 [P] Unit test for PR-REVIEW.md parser in `tools/pr-review/internal/config/parser_test.go`
- [x] T007 [P] Unit test for GitHub client in `tools/pr-review/internal/github/client_test.go`
- [x] T008 Unit test for diff computer in `tools/pr-review/internal/diff/diff_test.go`

### Implementation for Foundational

- [x] T009 [P] Implement `tools/pr-review/internal/config/parser.go` — parse PR-REVIEW.md into typed `Config` struct (sections 1-9, hygiene tick list, agent table)
- [x] T010 [P] Implement `tools/pr-review/internal/github/client.go` — comment CRUD via GitHub REST API (`ListComments`, `CreateComment`, `UpdateComment`) with HTML marker tracking
- [x] T011 Implement `tools/pr-review/internal/diff/diff.go` — compute PR diff via `git diff`, with file exclusions (`vendor/`, `node_modules/`, `*.lock`) and size cap (500KB)
- [x] T012 Implement `tools/pr-review/internal/provider/provider.go` — define `Provider` interface with `Generate(ctx, model, prompt) (Response, error)`
- [x] T013 Add structured logging via `slog` in `tools/pr-review/cmd/review/main.go`

**Checkpoint**: Foundation ready — parser, GitHub client, diff computer, and provider interface are all implemented and tested. User story implementation can now begin.

---

## Phase 3: User Story 1 — Automated Hygiene Checks on Every PR (Priority: P1) 🎯 MVP

**Goal**: Developers receive an automated hygiene report comment on every PR, checking ticked items from PR-REVIEW.md.

**Independent Test**: Open a PR on a repo with PR-REVIEW.md containing ticked hygiene items. Verify a hygiene report comment appears within 5 minutes with correct pass/fail status.

### Tests for User Story 1 (MANDATORY)

- [x] T014 [P] [US1] Unit test for each hygiene check in `tools/pr-review/internal/hygiene/checker_test.go` (H001 pass/fail, H002 pass/fail, H003 Go outdated, H003 Node outdated, H006 pass/fail, no ticked items)
- [x] T015 [P] [US1] Integration test for hygiene end-to-end in `tools/pr-review/tests/hygiene_integration_test.go`

### Implementation for User Story 1

- [x] T016 [P] [US1] Implement `tools/pr-review/internal/hygiene/checker.go` — H001 (`README.md` existence), H002 (`.github/CODEOWNERS` existence), H006 (`.devcontainer/devcontainer.json` existence)
- [x] T017 [US1] Implement H003 in `tools/pr-review/internal/hygiene/checker.go` — detect language by `go.mod`/`package.json`, run `go list -u -m all` or `npm outdated`, report outdated dependencies
- [x] T018 [US1] Wire hygiene runner into `tools/pr-review/cmd/review/main.go` — parse config, run ticked checks, format report
- [x] T019 [US1] Implement hygiene comment posting in `tools/pr-review/cmd/review/main.go` — search for existing hygiene marker, create or update comment with markdown report
- [x] T020 [US1] Add exit code logic: hygiene failures alone do not fail Action (exit 0); hygiene report is advisory

**Checkpoint**: At this point, hygiene checks run end-to-end on PR open. US1 is independently testable and deliverable as MVP.

---

## Phase 4: User Story 2 — AI-Powered Code Review with Severity Classification (Priority: P1)

**Goal**: Configured AI agents review PR diffs and post severity-classified reports. CRITICAL/HIGH findings fail the PR status check.

**Independent Test**: Open a PR that introduces an obvious bug. Configure an agent in PR-REVIEW.md. Verify the agent report comment appears, contains severity JSON, and CRITICAL/HIGH findings turn the status check red.

### Tests for User Story 2 (MANDATORY)

- [x] T021 [P] [US2] Unit test for severity JSON parser in `tools/pr-review/internal/review/review_test.go` (valid JSON, missing JSON, malformed JSON, extra text)
- [x] T022 [P] [US2] Unit test for prompt builder in `tools/pr-review/internal/review/review_test.go`
- [x] T023 [US2] Unit test for `OllamaProvider` in `tools/pr-review/internal/provider/ollama_test.go` (success, timeout, non-2xx, empty response)
- [x] T024 [US2] Integration test for full agent pipeline in `tools/pr-review/tests/agent_integration_test.go` (mock ollama server, mock GitHub API)

### Implementation for User Story 2

- [x] T025 [P] [US2] Implement `tools/pr-review/internal/provider/ollama.go` — HTTP POST to `https://ollama.com/api/generate`, bearer auth, 10s dial + 120s total timeout, one retry, parse `response` field
- [x] T026 [P] [US2] Implement `tools/pr-review/internal/review/review.go` — `BuildPrompt(ctx, config, agent, diff)` and `ParseSeverity(responseText)`
- [x] T027 [US2] Wire agent loop into `tools/pr-review/cmd/review/main.go` — for each configured agent, build prompt, call provider, parse severity, collect report
- [x] T028 [US2] Implement agent comment posting in `tools/pr-review/cmd/review/main.go` — one comment per agent, update-in-place via HTML marker, include severity summary and markdown report
- [x] T029 [US2] Add severity-based exit code logic: if any agent has `critical > 0` or `high > 0`, exit code 1; else exit code 0
- [x] T030 [US2] Handle unparseable severity: if JSON block not found, log warning, treat as advisory (exit 0), post report anyway

**Checkpoint**: At this point, both US1 (hygiene) and US2 (agent reviews) work independently. The full review pipeline is functional.

---

## Phase 5: User Story 3 — Review Respects Project-Specific Context (Priority: P2)

**Goal**: Agents receive context from PR-REVIEW.md sections 1-7. Security-specific agents only run when configured. Extra instructions in the "Additional" column are injected into prompts.

**Independent Test**: Create two repos with identical code but different PR-REVIEW.md (high vs low security, different additional instructions). Open identical PRs. Verify high-security repo gets security-focused prompts and low-security does not.

### Tests for User Story 3 (MANDATORY)

- [x] T031 [P] [US3] Unit test for context injection in `tools/pr-review/internal/review/review_test.go` — verify sections 1-7 appear in prompt
- [x] T032 [P] [US3] Unit test for "Additional" column injection in `tools/pr-review/internal/review/review_test.go`

### Implementation for User Story 3

- [x] T033 [US3] Verify `tools/pr-review/internal/review/review.go` prepends sections 1-7 from `Config` to every agent prompt
- [x] T034 [US3] Verify `tools/pr-review/internal/review/review.go` appends `AgentConfig.Additional` to the agent-specific prompt section
- [x] T035 [US3] Verify that agents listed in PR-REVIEW.md are the only ones invoked (no default agents)
- [x] T036 [US3] Add PR-REVIEW.md to `woodendollars` repo root with appropriate context and agent configuration for this project

**Checkpoint**: At this point, all user stories are independently functional. Reviews are context-aware and configurable per repository.

---

## Phase 6: User Story 4 — Review Infrastructure Fails Safely (Priority: P2)

**Goal**: When the AI provider is unreachable or returns errors, the Action fails explicitly with no partial or misleading comments posted.

**Independent Test**: Open a PR while the ollama endpoint is blocked. Verify the PR status check fails and no review comments are posted.

### Tests for User Story 4 (MANDATORY)

- [x] T037 [P] [US4] Integration test for ollama unreachable in `tools/pr-review/tests/failure_integration_test.go` — mock server returns 500, assert exit code 1, assert no comments posted
- [x] T038 [P] [US4] Integration test for ollama timeout in `tools/pr-review/tests/failure_integration_test.go` — mock server hangs, assert timeout triggers, exit code 1
- [x] T039 [US4] Integration test for missing PR-REVIEW.md in `tools/pr-review/tests/failure_integration_test.go` — assert exit code 1 with clear error message

### Implementation for User Story 4

- [x] T040 [US4] Verify `OllamaProvider` returns errors on timeout, non-2xx, and empty response — no comments posted on error
- [x] T041 [US4] Verify `main.go` exits with code 1 on any provider error, with logged error message
- [x] T042 [US4] Verify `main.go` exits with code 1 when `PR-REVIEW.md` is missing, before any comments are posted
- [x] T043 [US4] Add retry logic to `OllamaProvider`: one retry on timeout/network errors, fail on second attempt
- [x] T044 [US4] Verify hygiene comments are NOT posted when the overall Action fails due to infrastructure errors (all-or-nothing for a single run)

**Checkpoint**: At this point, all failure modes are handled safely. No partial or misleading reviews are posted.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Improvements that affect all user stories

### Tests

- [x] T045 [P] Unit test for large diff truncation in `tools/pr-review/internal/diff/diff_test.go`
- [x] T046 Unit test for long comment truncation in `tools/pr-review/internal/github/client_test.go`

### Implementation

- [x] T047 [P] Add large diff handling in `tools/pr-review/internal/diff/diff.go` — truncate at 500KB with warning in prompt
- [x] T048 [P] Add long comment handling in `tools/pr-review/internal/github/client.go` — chunk or truncate at GitHub's 65536 character limit
- [x] T049 Run `go vet ./...` and `golangci-lint` on `tools/pr-review/` and fix all issues
- [x] T050 Run `go test -race ./...` on `tools/pr-review/` and fix all races
- [x] T051 Add `tools/pr-review/README.md` documenting: installation, PR-REVIEW.md format, flags, env vars, adding a new provider
- [x] T052 Add `PR-REVIEW.md` documentation section to root `README.md`
- [x] T053 Verify `.github/workflows/pr-review.yml` works on a test PR in a fork:
  - Trigger on PR open: PASS - workflow triggers correctly on PR #16
  - Post hygiene comment: PASS - comment posted at https://github.com/nickwhiteley/woodendollars/pull/16#issuecomment-4496904596
  - Update comments on new commits: PASS - existing comment updated in-place via HTML marker
  - Correct exit code for pass/fail: PASS - exits 1 on CRITICAL/HIGH findings
  - Post agent comments: PASS - all 4 agents posted comments successfully

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — can start immediately
- **Foundational (Phase 2)**: Depends on Setup completion — BLOCKS all user stories
- **User Stories (Phase 3-6)**: All depend on Foundational phase completion
  - US1 and US2 can proceed in parallel (different subsystems: hygiene vs agents)
  - US3 and US4 can proceed in parallel after US2 (build on agent infrastructure)
- **Polish (Phase 7)**: Depends on all user stories being complete

### User Story Dependencies

- **User Story 1 (P1)**: Can start after Foundational (Phase 2). No dependencies on other stories.
- **User Story 2 (P1)**: Can start after Foundational (Phase 2). No dependencies on US1 (hygiene and agents are independent subsystems).
- **User Story 3 (P2)**: Can start after US2 (requires prompt builder and agent infrastructure).
- **User Story 4 (P2)**: Can start after US2 (requires provider infrastructure to test failure modes).

### Within Each User Story

- Tests MUST be written and FAIL before implementation
- Core implementation before wiring
- Wiring before end-to-end validation

### Parallel Opportunities

- All Setup tasks marked [P] can run in parallel (T002-T005)
- All Foundational tests marked [P] can run in parallel (T006-T008)
- All Foundational implementations marked [P] can run in parallel (T009-T010)
- US1 and US2 can be developed in parallel once Foundational is complete
- US3 and US4 can be developed in parallel once US2 is complete
- All Polish tasks marked [P] can run in parallel (T047-T048)

---

## Parallel Example: User Story 1 + User Story 2

```bash
# After Foundational phase is complete, launch US1 and US2 in parallel:

# US1 tasks (hygiene subsystem):
Task: "T016 [US1] Implement hygiene checker in tools/pr-review/internal/hygiene/checker.go"
Task: "T017 [US1] Implement H003 dependency check in tools/pr-review/internal/hygiene/checker.go"
Task: "T018 [US1] Wire hygiene runner into tools/pr-review/cmd/review/main.go"

# US2 tasks (agent subsystem):
Task: "T025 [US2] Implement OllamaProvider in tools/pr-review/internal/provider/ollama.go"
Task: "T026 [US2] Implement review orchestrator in tools/pr-review/internal/review/review.go"
Task: "T027 [US2] Wire agent loop into tools/pr-review/cmd/review/main.go"
```

---

## Implementation Strategy

### MVP First (User Stories 1 + 2)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational
3. Complete Phase 3: User Story 1 (hygiene checks)
4. Complete Phase 4: User Story 2 (agent reviews)
5. **STOP and VALIDATE**: Test the full pipeline on a real PR

### Incremental Delivery

1. Setup + Foundational → Foundation ready
2. Add US1 → Hygiene reports working → Deploy/Demo
3. Add US2 → Agent reviews working → Deploy/Demo (MVP complete)
4. Add US3 → Context-aware prompts → Deploy/Demo
5. Add US4 → Safe failure modes → Deploy/Demo
6. Polish → Documentation, edge cases → Final

### Parallel Team Strategy

With multiple developers:

1. Team completes Setup + Foundational together
2. Once Foundational is done:
   - Developer A: US1 (hygiene)
   - Developer B: US2 (agents + provider)
3. Once US2 is done:
   - Developer A: US3 (context injection)
   - Developer B: US4 (failure modes)
4. Stories integrate at `main.go` wiring layer

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- Each user story should be independently completable and testable
- Verify tests fail before implementing
- Commit after each task or logical group
- Stop at any checkpoint to validate story independently
- Avoid: vague tasks, same file conflicts, cross-story dependencies that break independence
