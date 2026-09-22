// Package config parses PR-REVIEW.md into a typed configuration.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Config holds the parsed PR-REVIEW.md configuration.
type Config struct {
	Owner            string
	Context          string
	ProductionStatus string
	ConnectedSystems string
	IntendedAudience string
	AuditLevel       string
	SecurityLevel    string
	Hygiene          []HygieneCheck
	Agents           []AgentConfig
	// ExcludedPaths are repository-specific patterns kept out of every
	// agent's diff, on top of diff.DefaultExclusions.
	ExcludedPaths []string
}

// HygieneCheck is a single hygiene item.
type HygieneCheck struct {
	ID     string
	Name   string
	Ticked bool
}

// AgentConfig is a single review agent configuration.
type AgentConfig struct {
	Plugin     string
	Subagent   string
	Additional string
	// Paths scopes this agent to the files it actually reviews. Empty means
	// the whole diff, which is what every agent did before this existed —
	// so an unscoped table keeps its old behaviour. Patterns use the
	// vocabulary in diff.Match, and a leading "!" subtracts: the common case
	// is a role that reviews a package but has no use for its unit tests.
	Paths []string
}

// Scope splits Paths into the patterns an agent's diff is kept to and those
// subtracted from it. A cell of only negations still means "everything
// except", so include stays empty in that case.
func (a AgentConfig) Scope() (include, exclude []string) {
	for _, p := range a.Paths {
		if after, found := strings.CutPrefix(p, "!"); found {
			if after != "" {
				exclude = append(exclude, after)
			}
			continue
		}
		include = append(include, p)
	}
	return include, exclude
}

// Parse reads and parses PR-REVIEW.md from the given path.
func Parse(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read PR-REVIEW.md: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes parses PR-REVIEW.md content.
func ParseBytes(data []byte) (*Config, error) {
	cfg := &Config{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))

	var currentSection string
	var buf strings.Builder

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		if strings.HasPrefix(line, "## ") {
			// flush previous section
			flushSection(cfg, currentSection, buf.String())
			buf.Reset()
			currentSection = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}

		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flushSection(cfg, currentSection, buf.String())

	if cfg.Owner == "" {
		return nil, errors.New(`missing required section: "Owner" (or "Who owns the repo")`)
	}
	if cfg.Context == "" {
		return nil, errors.New(`missing required section: "Context" (or "Context and intent")`)
	}
	if cfg.ProductionStatus == "" {
		return nil, errors.New(`missing required section: "Production Status"`)
	}
	if cfg.SecurityLevel == "" {
		return nil, errors.New(`missing required section: "Security Level"`)
	}

	return cfg, nil
}

func flushSection(cfg *Config, section, content string) {
	section = strings.ToLower(section)
	switch {
	// "owner" is matched exactly, not as a substring — "codeowners" would
	// otherwise collide and overwrite the Owner field.
	case strings.Contains(section, "who owns the repo") || section == "owner":
		cfg.Owner = strings.TrimSpace(content)
	// "context" alone must not swallow other sections, so match it exactly;
	// the long form "context and intent" is matched as a substring.
	case strings.Contains(section, "context and intent") || section == "context":
		cfg.Context = strings.TrimSpace(content)
	case strings.Contains(section, "production status"):
		cfg.ProductionStatus = strings.TrimSpace(content)
	case strings.Contains(section, "connected systems"):
		cfg.ConnectedSystems = strings.TrimSpace(content)
	case strings.Contains(section, "intended audience"):
		cfg.IntendedAudience = strings.TrimSpace(content)
	case strings.Contains(section, "audit level"):
		cfg.AuditLevel = strings.TrimSpace(content)
	case strings.Contains(section, "security level"):
		cfg.SecurityLevel = strings.TrimSpace(content)
	case strings.Contains(section, "pr hygiene"):
		cfg.Hygiene = parseHygiene(content)
	case strings.Contains(section, "code reviews"):
		cfg.Agents = parseAgents(content)
	case strings.Contains(section, "excluded paths"):
		cfg.ExcludedPaths = parseList(content)
	}
}

func parseHygiene(content string) []HygieneCheck {
	var checks []HygieneCheck
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-") {
			continue
		}

		// Extract checkbox state
		ticked := false
		if strings.Contains(line, "[x]") || strings.Contains(line, "[X]") {
			ticked = true
		}
		if !strings.Contains(line, "[") {
			continue // not a checkbox line
		}

		// Extract ID and name: "- [x] H001 description"
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}

		id := ""
		name := ""
		for i, p := range parts {
			if i == 0 && strings.HasPrefix(p, "-") {
				continue
			}
			if p == "[x]" || p == "[X]" || p == "[]" || p == "[ ]" {
				continue
			}
			if id == "" && len(p) >= 3 && p[0] == 'H' && p[1] >= '0' && p[1] <= '9' {
				id = p
				if i+1 < len(parts) {
					name = strings.Join(parts[i+1:], " ")
				}
				break
			}
			if id == "" {
				id = fmt.Sprintf("H%03d", len(checks)+1)
				name = strings.Join(parts[i:], " ")
				break
			}
		}

		if name == "" {
			name = strings.Join(parts, " ")
		}

		checks = append(checks, HygieneCheck{
			ID:     id,
			Name:   name,
			Ticked: ticked,
		})
	}
	return checks
}

// parseList reads a markdown bullet list into its items, ignoring anything
// that is not a bullet so prose around the list is harmless.
func parseList(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") {
			continue
		}
		item := strings.TrimSpace(line[2:])
		// Patterns are conventionally written in backticks.
		item = strings.Trim(item, "`")
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// tableColumns maps the lower-cased header names of a markdown table row to
// their cell indices, or nil if the row is not a header.
//
// Columns are addressed by NAME rather than position because the table grew
// a "Paths" column after repositories were already using it. Reading cell 3
// as Additional and cell 4 as Paths would have made column order load-bearing
// and silently mis-parsed every table that put them the other way round.
func tableColumns(cells []string) map[string]int {
	cols := make(map[string]int, len(cells))
	for i, c := range cells {
		name := strings.ToLower(strings.TrimSpace(c))
		if name != "" {
			cols[name] = i
		}
	}
	if _, ok := cols["plugin"]; !ok {
		return nil
	}
	if _, ok := cols["subagent"]; !ok {
		return nil
	}
	return cols
}

func parseAgents(content string) []AgentConfig {
	var agents []AgentConfig
	// Defaults for a table with no recognisable header, preserving the
	// original positional reading.
	cols := map[string]int{"plugin": 1, "subagent": 2, "additional": 3}

	cell := func(cells []string, name string) string {
		i, ok := cols[name]
		if !ok || i >= len(cells) {
			return ""
		}
		return strings.TrimSpace(cells[i])
	}

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "|") && strings.Contains(line, "---") {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}

		// Split table row
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		// The header row names the columns; it is not an agent.
		if hdr := tableColumns(cells); hdr != nil {
			cols = hdr
			continue
		}

		plugin := cell(cells, "plugin")
		subagent := cell(cells, "subagent")
		if plugin == "" || subagent == "" {
			continue
		}

		agents = append(agents, AgentConfig{
			Plugin:     plugin,
			Subagent:   subagent,
			Additional: cell(cells, "additional"),
			Paths:      splitPatterns(cell(cells, "paths")),
		})
	}
	return agents
}

// splitPatterns reads a Paths cell: patterns separated by commas or spaces,
// optionally in backticks. An empty cell yields nil, meaning "the whole diff".
func splitPatterns(cell string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(cell, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		f = strings.Trim(strings.TrimSpace(f), "`")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
