# pr-reviewer

AI-powered pull request review tool. It reads a `PR-REVIEW.md` configuration file from your repository, runs deterministic hygiene checks, and dispatches AI agents (via Ollama) to review the diff from multiple angles — then posts the results as comments on the pull request.

Every merge to `main` publishes a new binary release automatically.

---

## How it works

1. Reads `PR-REVIEW.md` from the repository root to learn the project's context and which agents/checks to run
2. Computes the diff between base and head commits (excluding vendored files, lock files, generated code)
3. Runs ticked hygiene checks deterministically — no AI required
4. Splits large diffs into 50 KB chunks and runs each configured AI agent in parallel
5. Posts (or updates) a comment per agent on the pull request using HTML markers for idempotency
6. Exits with code `1` if any agent reports a critical or high severity finding

---

## Installation

### Download binary (recommended)

Download the latest release from the [Releases page](https://github.com/nickwhiteley/pr-reviewer/releases) for your platform:

| Platform | Archive |
|---|---|
| Linux x86-64 | `pr-reviewer_linux_amd64.tar.gz` |
| Linux ARM64 | `pr-reviewer_linux_arm64.tar.gz` |
| macOS x86-64 | `pr-reviewer_darwin_amd64.tar.gz` |
| macOS ARM64 | `pr-reviewer_darwin_arm64.tar.gz` |
| Windows x86-64 | `pr-reviewer_windows_amd64.zip` |

### Install with Go

```bash
go install github.com/nickwhiteley/pr-reviewer/cmd/review@latest
```

### Build from source

```bash
git clone https://github.com/nickwhiteley/pr-reviewer.git
cd pr-reviewer
make build        # produces ./pr-reviewer binary
make test         # run tests with -race
```

---

## Usage

```bash
pr-reviewer \
  --pr 42 \
  --repo owner/repo \
  --base abc123 \
  --head def456 \
  --config PR-REVIEW.md
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--pr` | required | Pull request number |
| `--repo` | required | Repository in `owner/name` format |
| `--base` | required | Base commit SHA |
| `--head` | required | Head commit SHA |
| `--config` | `PR-REVIEW.md` | Path to the config file |
| `--model` | `kimi-k2.6:cloud` | Ollama model name |
| `--skip-agents` | false | Run hygiene checks only, skip AI agents |
| `--debug` | false | Enable debug logging |

### Environment variables

| Variable | Description |
|---|---|
| `GITHUB_TOKEN` | GitHub token with `pull-requests: write` permission |
| `OLLAMA_API_KEY` | API key for the Ollama endpoint |
| `OLLAMA_ENDPOINT` | Override the Ollama API URL (default: `https://ollama.com/api/chat`) |

---

## PR-REVIEW.md configuration

Place a `PR-REVIEW.md` file in the root of your repository. The tool reads this file on every run. See [`example/PR-REVIEW.md`](example/PR-REVIEW.md) for a complete template.

### Required sections

```markdown
## Owner
Your Name / Team Name

## Context
Describe what this repository does in 2–3 sentences. The agents use this as
system context when reviewing the diff.

## Production Status
Live / Beta / Internal / Staging

## Security Level
High / Medium / Low
```

### Optional sections

```markdown
## Connected Systems
- PostgreSQL database
- Stripe payments API
- Downstream analytics pipeline

## Intended Audience
Internal engineering teams; external API consumers via documented contracts.

## Audit Level
High
```

### PR Hygiene

Enable deterministic checks with `- [x]`. Disable with `- [ ]`.

```markdown
## PR Hygiene
- [x] H001 README exists
- [x] H002 CODEOWNERS exists
- [ ] H003 Dependencies up to date
- [x] H006 Dev container configured
```

| Check | ID | What it verifies |
|---|---|---|
| README exists | H001 | `README.md` present at repo root |
| CODEOWNERS exists | H002 | `.github/CODEOWNERS` present |
| Dependencies up to date | H003 | `go list -u -m all` (Go) or `npm outdated` (Node) |
| Dev container configured | H006 | `.devcontainer/devcontainer.json` present |

### Code Review Agents

Configure agents as a Markdown table. Each row is one agent invocation.

```markdown
## Code Reviews
| Plugin | Subagent | Additional |
| voltagent-qa-sec | architect-reviewer | |
| voltagent-qa-sec | penetration-tester | focus on input validation |
| voltagent-data-ai | database-optimizer | |
```

**Plugin**: the agent plugin namespace (used in the system prompt).  
**Subagent**: the specific reviewer role within the plugin.  
**Additional**: optional extra instructions appended to that agent's prompt.

---

## GitHub Actions integration

Add to `.github/workflows/pr-review.yml` in your repository:

```yaml
name: PR Review

on:
  pull_request:
    branches: [main]
    types: [opened, synchronize, reopened]

permissions:
  contents: read
  pull-requests: write

jobs:
  review:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Install pr-reviewer
        run: |
          gh release download \
            --repo nickwhiteley/pr-reviewer \
            --pattern 'pr-reviewer_linux_amd64.tar.gz' \
            -D /tmp/pr-reviewer
          tar -xzf /tmp/pr-reviewer/pr-reviewer_linux_amd64.tar.gz -C /usr/local/bin
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}

      - name: Run PR Review
        run: |
          pr-reviewer \
            --pr "${{ github.event.pull_request.number }}" \
            --repo "${{ github.repository }}" \
            --base "${{ github.event.pull_request.base.sha }}" \
            --head "${{ github.event.pull_request.head.sha }}" \
            --config PR-REVIEW.md
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          OLLAMA_API_KEY: ${{ secrets.OLLAMA_API_KEY }}
```

Add `OLLAMA_API_KEY` as a repository secret. The `GITHUB_TOKEN` is provided automatically.

To pin to a specific version, replace `--repo nickwhiteley/pr-reviewer` with `--repo nickwhiteley/pr-reviewer --tag v1.2.3`.

---

## Exit codes

| Code | Meaning |
|---|---|
| `0` | All checks passed; no critical or high findings |
| `1` | Critical or high severity finding detected, or infrastructure failure |

---

## Releases

Every merge to `main` automatically creates a new patch release and publishes multi-platform binaries via goreleaser. Pin a specific version in your workflow to avoid unexpected changes.

---

## Development

```bash
make build    # build binary
make test     # go test -race ./...
make vet      # go vet ./...
make lint     # golangci-lint run
make clean    # remove built binary
```
