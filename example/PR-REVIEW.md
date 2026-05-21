# PR-REVIEW.md — example configuration
# Copy this file to the root of your repository and customise each section.

## Owner
Your Name / Team Name

## Context
Describe what this repository does in 2–3 sentences so that AI agents have
accurate context when reviewing pull requests. Include the primary user-facing
purpose, the technology stack, and any critical constraints (e.g., regulated
environment, high-availability requirement).

## Production Status
Live / Beta / Internal / Staging

## Connected Systems
- List external services this codebase integrates with
- e.g. PostgreSQL database, Stripe payments, downstream analytics pipeline

## Intended Audience
Describe who uses this system — internal staff, external customers, other
engineering teams — so agents can calibrate their severity judgements.

## Audit Level
High / Medium / Low

## Security Level
High / Medium / Low

## PR Hygiene
- [x] H001 README.md is present and up to date
- [x] H002 CODEOWNERS exists
- [ ] H003 All runtime and dependency versions are up to date
- [x] H006 Dev container definitions exist and are appropriate

## Code Reviews
| Plugin            | Subagent             | Additional |
| ----------------- | -------------------- | ---------- |
| voltagent-qa-sec  | architect-reviewer   |            |
| voltagent-qa-sec  | penetration-tester   |            |
| voltagent-qa-sec  | performance-engineer |            |
| voltagent-data-ai | database-optimizer   |            |
