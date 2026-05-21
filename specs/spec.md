# Feature Specification: Automate PR Review

**Feature Branch**: `015-automate-pr-review`  
**Created**: 2026-05-20  
**Status**: Draft  

## Overview

Developers raising pull requests against the main branch must receive an automated code review within minutes. The review combines a configurable hygiene checklist with targeted AI-powered code reviews, posting results directly onto the pull request as comments. This reduces the burden on human reviewers for repetitive checks and ensures consistent quality gates across every repository in the organisation.

---

## User Scenarios & Testing

### User Story 1 — Automated hygiene checks on every PR (Priority: P1)

A developer opens a pull request. Before human reviewers spend time on it, the system automatically verifies standard repository hygiene items defined in the project's PR-REVIEW.md file — such as whether a README is present, whether CODEOWNERS is configured, and whether dependency versions are current. The results are posted as a single comment on the PR, with checked items marked pass and unchecked items marked fail.

**Why this priority**: These checks are mechanical and repetitive. Automating them frees human reviewers to focus on architecture and logic, and catches common omissions that block merges.

**Independent Test**: Open a PR on a repository that has PR-REVIEW.md with hygiene items ticked. The PR receives a hygiene report comment within 5 minutes. Each ticked item is either marked pass or fail with an explanation.

**Acceptance Scenarios**:

1. **Given** a repository has PR-REVIEW.md with hygiene check H001 ticked and README.md exists, **When** a PR is opened, **Then** the hygiene report marks H001 as passed.
2. **Given** a repository has PR-REVIEW.md with hygiene check H002 ticked and CODEOWNERS is missing, **When** a PR is opened, **Then** the hygiene report marks H002 as failed.
3. **Given** a repository has PR-REVIEW.md with hygiene check H003 ticked, **When** a PR is opened, **Then** the system verifies whether declared dependencies are up to date and reports the result.
4. **Given** a repository has PR-REVIEW.md with no hygiene items ticked, **When** a PR is opened, **Then** the hygiene report states that no checks were requested.

---

### User Story 2 — AI-powered code review with severity classification (Priority: P1)

A developer opens a pull request. The system reads the project's PR-REVIEW.md, identifies which code review agents are configured (e.g., architect reviewer, penetration tester), and invokes each agent against the PR diff. Each agent produces a structured report with findings classified by severity (critical, high, medium, low). Reports are posted as separate PR comments. If any finding is critical or high severity, the automated review fails and the PR status check turns red.

**Why this priority**: Catching architectural issues and security problems automatically before human review accelerates the feedback loop and prevents defects from reaching the codebase.

**Independent Test**: Open a PR that introduces an obvious SQL injection vulnerability. The PR receives a code review report identifying the vulnerability as critical severity, and the PR status check fails.

**Acceptance Scenarios**:

1. **Given** PR-REVIEW.md lists an architect-reviewer agent, **When** a PR is opened, **Then** the PR receives an architect review comment within 5 minutes.
2. **Given** a review agent finds a critical severity issue, **When** the report is posted, **Then** the PR status check fails.
3. **Given** a review agent finds only medium and low severity issues, **When** the report is posted, **Then** the PR status check remains green and the findings are advisory.
4. **Given** a PR is updated with new commits, **When** the review re-runs, **Then** the previous review comments are updated in-place rather than duplicated.

---

### User Story 3 — Review respects project-specific context and security posture (Priority: P2)

A repository handling financial transactions configures a high security level and includes a penetration-tester agent in its PR-REVIEW.md. A repository for internal tooling configures a moderate security level and omits the penetration tester. When PRs are opened in each repository, the review for the financial repository is more stringent and includes security-specific scrutiny, while the internal tooling review focuses on general code quality.

**Why this priority**: One-size-fits-all reviews produce noise in low-risk repos and miss critical issues in high-risk repos. Context-aware reviews maximise signal and minimise false positives.

**Independent Test**: Create two repositories with identical code but different PR-REVIEW.md security levels and agent configurations. Open identical PRs in both. The high-security repo receives a penetration test review; the low-security repo does not.

**Acceptance Scenarios**:

1. **Given** a repository's PR-REVIEW.md specifies a high security level, **When** a review is generated, **Then** the review prompt includes security-focused scrutiny and the configured security agents are invoked.
2. **Given** a repository's PR-REVIEW.md specifies a low security level, **When** a review is generated, **Then** security-specific agents are not invoked.
3. **Given** PR-REVIEW.md includes extra instructions in the "Additional" column for an agent, **When** that agent runs, **Then** its review focuses on the areas described in the additional instructions.

---

### User Story 4 — Review infrastructure fails safely (Priority: P2)

The AI provider hosting the review agents becomes temporarily unreachable. A developer opens a PR during the outage. The automated review Action detects the failure and marks the PR status check as failed, alerting the team that the review infrastructure is down. No partial or misleading review comments are posted.

**Why this priority**: Silent failures in automated checks create a false sense of security. Explicit failure ensures teams know when the safety net is missing.

**Independent Test**: Open a PR while the AI provider endpoint is blocked or returning errors. The PR receives a failed status check with a clear message indicating the review service is unavailable.

**Acceptance Scenarios**:

1. **Given** the AI provider is unreachable, **When** a PR is opened, **Then** the review Action fails and no review comments are posted.
2. **Given** the AI provider returns an error response, **When** a PR is opened, **Then** the review Action fails and the error is logged for debugging.
3. **Given** the review Action fails due to infrastructure, **When** the PR status is inspected, **Then** it shows a red check with a message indicating review unavailability.

---

### Edge Cases

- What happens when a PR is opened on a repository that does not have PR-REVIEW.md?
- What happens when the PR diff is extremely large (e.g., thousands of files changed)?
- What happens when multiple PRs are opened simultaneously — do reviews queue or run concurrently?
- What happens when a PR modifies PR-REVIEW.md itself?
- What happens when an agent produces a response that cannot be parsed into the expected severity format?
- What happens when a review comment exceeds the platform's maximum comment length?
- What happens when a previous review comment has been deleted by a human moderator?

---

## Requirements

### Functional Requirements

- **FR-001**: The system MUST trigger an automated review every time a pull request is opened against the main branch.
- **FR-002**: The system MUST read the project's PR-REVIEW.md configuration from the repository root before performing any review.
- **FR-003**: If PR-REVIEW.md is missing, the system MUST fail the review and report the missing configuration.
- **FR-004**: The system MUST execute only the hygiene checks that are explicitly ticked in PR-REVIEW.md.
- **FR-005**: The system MUST post a single hygiene report comment on the pull request, summarising the pass/fail status of each ticked check.
- **FR-006**: The system MUST execute only the code review agents that are explicitly listed in PR-REVIEW.md.
- **FR-007**: For each configured code review agent, the system MUST post a separate review comment on the pull request.
- **FR-008**: Each code review comment MUST include a structured severity summary (critical, high, medium, low counts) that the system can parse programmatically.
- **FR-009**: If any code review comment contains one or more critical or high severity findings, the system MUST mark the PR status check as failed.
- **FR-010**: If all code review comments contain only medium, low, or no severity findings, the system MUST mark the PR status check as passed.
- **FR-011**: The system MUST update existing review comments in-place when a PR is re-reviewed (e.g., after new commits), rather than creating duplicate comments.
- **FR-012**: The system MUST include the first seven sections of PR-REVIEW.md as context in every code review agent prompt.
- **FR-013**: If the AI provider is unreachable or returns an error, the system MUST fail the review Action and not post partial results.
- **FR-014**: The system MUST obtain the PR diff using standard version control commands from the checked-out repository.
- **FR-015**: The system MUST support a configurable model identifier, defaulting to the project's standard model.
- **FR-016**: The system MUST authenticate with the AI provider using a project-level secret.
- **FR-017**: The system MUST authenticate with the code hosting platform using the standard platform token to post and update comments.

### Key Entities

- **PR-Review Configuration**: The repository-level PR-REVIEW.md file. Contains project context (owner, intent, production status, connected systems, audience, audit level, security level), a tick-list of hygiene checks, and a table of code review agents with optional additional instructions.
- **Hygiene Check**: A named repository-health verification (e.g., README presence, CODEOWNERS presence, dependency currency, devcontainer existence). Each check is either ticked (run) or unticked (skip).
- **Code Review Agent**: A named review profile (e.g., architect reviewer, penetration tester) that analyses the PR diff and produces findings. Each agent has a severity-aware output format.
- **Review Report**: A markdown comment posted on the pull request. Hygiene reports summarise check results. Agent reports contain findings with severity classifications.
- **Severity Summary**: A structured block within each agent report enumerating counts of critical, high, medium, and low findings. Used to determine whether the PR status check passes or fails.

---

## Success Criteria

### Measurable Outcomes

- **SC-001**: Every pull request opened against main receives an automated review comment within 5 minutes.
- **SC-002**: 100% of ticked hygiene checks produce a clear pass or fail result in the hygiene report.
- **SC-003**: Pull requests with critical or high severity findings from any code review agent receive a failed status check.
- **SC-004**: No duplicate review comments are created when a PR is updated with new commits; existing comments are updated in-place.
- **SC-005**: When the review infrastructure is unavailable, the PR status check fails explicitly rather than passing silently.
- **SC-006**: Repositories with different security levels receive appropriately scoped reviews (e.g., high-security repos get security agents; low-security repos do not).
- **SC-007**: Reviewers spend 30% less time on repetitive hygiene checks because they are automated.

---

## Assumptions

- Repositories using this feature are hosted on GitHub and use GitHub Actions for CI.
- The AI provider endpoint is hosted externally (cloud) and accessible via HTTPS with bearer token authentication.
- PR-REVIEW.md is written in Markdown and follows the agreed 9-section structure.
- The system runs on standard cloud-hosted CI runners with internet egress.
- Future AI providers can be added without redesigning the core review orchestration logic.
- The maximum acceptable review latency is 5 minutes for typical pull requests (under 50 files changed).
