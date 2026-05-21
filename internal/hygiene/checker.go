// Package hygiene runs repository hygiene checks.
package hygiene

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Result is the outcome of a single hygiene check.
type Result struct {
	ID      string
	Name    string
	Passed  bool
	Details string
}

// Runner executes ticked hygiene checks.
type Runner struct{}

// NewRunner creates a new hygiene runner.
func NewRunner() *Runner {
	return &Runner{}
}

// Run executes the given checks and returns results.
func (r *Runner) Run(ctx context.Context, checks []Check) []Result {
	var results []Result
	for _, c := range checks {
		if !c.Ticked {
			continue
		}
		res := r.runOne(ctx, c)
		results = append(results, res)
	}
	return results
}

// Check is a single hygiene check definition.
type Check struct {
	ID     string
	Name   string
	Ticked bool
}

func (r *Runner) runOne(ctx context.Context, c Check) Result {
	switch c.ID {
	case "H001":
		return checkFileExists(c, "README.md")
	case "H002":
		return checkFileExists(c, ".github/CODEOWNERS")
	case "H003":
		return r.checkDependencies(ctx, c)
	case "H006":
		if path, ok := findUpward(".devcontainer/devcontainer.json"); ok {
			return Result{ID: c.ID, Name: c.Name, Passed: true, Details: path + " exists"}
		}
		return Result{ID: c.ID, Name: c.Name, Passed: false, Details: ".devcontainer/devcontainer.json not found"}
	default:
		return Result{ID: c.ID, Name: c.Name, Passed: false, Details: "unknown check ID"}
	}
}

func checkFileExists(c Check, path string) Result {
	if _, err := os.Stat(path); err != nil {
		return Result{ID: c.ID, Name: c.Name, Passed: false, Details: fmt.Sprintf("%s not found", path)}
	}
	return Result{ID: c.ID, Name: c.Name, Passed: true, Details: fmt.Sprintf("%s exists", path)}
}

func (r *Runner) checkDependencies(ctx context.Context, c Check) Result {
	if _, err := os.Stat("go.mod"); err == nil {
		return r.checkGoDeps(ctx, c)
	}
	if _, err := os.Stat("package.json"); err == nil {
		return r.checkNodeDeps(ctx, c)
	}
	return Result{ID: c.ID, Name: c.Name, Passed: false, Details: "no go.mod or package.json found"}
}

func (r *Runner) checkGoDeps(ctx context.Context, c Check) Result {
	cmd := exec.CommandContext(ctx, "go", "list", "-u", "-m", "all")
	out, err := cmd.Output()
	if err != nil {
		return Result{ID: c.ID, Name: c.Name, Passed: false, Details: fmt.Sprintf("go list failed: %v", err)}
	}

	var outdated []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "go ") {
			continue
		}
		// Format: "module version [update]"
		if strings.Contains(line, "[") && strings.Contains(line, "]") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				outdated = append(outdated, parts[0])
			}
		}
	}

	if len(outdated) > 0 {
		return Result{
			ID:      c.ID,
			Name:    c.Name,
			Passed:  false,
			Details: fmt.Sprintf("%d outdated Go modules: %s", len(outdated), strings.Join(outdated, ", ")),
		}
	}
	return Result{ID: c.ID, Name: c.Name, Passed: true, Details: "all Go modules up to date"}
}

func (r *Runner) checkNodeDeps(ctx context.Context, c Check) Result {
	cmd := exec.CommandContext(ctx, "npm", "outdated", "--json")
	cmd.Dir = "."
	out, err := cmd.Output()
	if err != nil {
		// npm outdated exits 1 when there are outdated packages
		if exitErr, ok := err.(*exec.ExitError); ok {
			out = exitErr.Stderr
		} else {
			return Result{ID: c.ID, Name: c.Name, Passed: false, Details: fmt.Sprintf("npm outdated failed: %v", err)}
		}
	}

	if len(out) == 0 || strings.TrimSpace(string(out)) == "{}" {
		return Result{ID: c.ID, Name: c.Name, Passed: true, Details: "all npm packages up to date"}
	}

	// Parse JSON output to count outdated packages
	lines := strings.Split(string(out), "\n")
	var outdated []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "{" || line == "}" {
			continue
		}
		// Each outdated package line starts with "  \"package\": {"
		if strings.HasPrefix(line, "\"") {
			name := strings.Split(line, "\"")[1]
			outdated = append(outdated, name)
		}
	}

	if len(outdated) > 0 {
		return Result{
			ID:      c.ID,
			Name:    c.Name,
			Passed:  false,
			Details: fmt.Sprintf("%d outdated npm packages: %s", len(outdated), strings.Join(outdated, ", ")),
		}
	}
	return Result{ID: c.ID, Name: c.Name, Passed: true, Details: "all npm packages up to date"}
}

// FormatReport returns a markdown report from results.
func FormatReport(results []Result) string {
	if len(results) == 0 {
		return "## PR Hygiene\n\nNo hygiene checks were ticked.\n"
	}
	var b strings.Builder
	b.WriteString("## PR Hygiene\n\n")
	b.WriteString("| Check | Status | Details |\n")
	b.WriteString("|-------|--------|---------|\n")
	for _, r := range results {
		status := "✅ Pass"
		if !r.Passed {
			status = "❌ Fail"
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s |\n", r.ID+" "+r.Name, status, r.Details))
	}
	b.WriteString("\n<!-- wd-auto-review:type=hygiene -->\n")
	return b.String()
}

// FormatReportWithContext returns a markdown report that includes the project context sections.
func FormatReportWithContext(results []Result, context string) string {
	if context != "" {
		return "## Context\n\n" + context + "\n\n" + FormatReport(results)
	}
	return FormatReport(results)
}

// Helper to find if a path exists within a project root
func findUpward(name string) (string, bool) {
	dir, _ := os.Getwd()
	for {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}
