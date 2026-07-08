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

func parseAgents(content string) []AgentConfig {
	var agents []AgentConfig
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
		// Skip header row if it contains "Plugin" or "Subagent"
		first := strings.TrimSpace(cells[1])
		if strings.EqualFold(first, "Plugin") || strings.EqualFold(first, "plugin") {
			continue
		}

		plugin := first
		subagent := ""
		additional := ""
		if len(cells) >= 3 {
			subagent = strings.TrimSpace(cells[2])
		}
		if len(cells) >= 4 {
			additional = strings.TrimSpace(cells[3])
		}

		if plugin == "" || subagent == "" {
			continue
		}

		agents = append(agents, AgentConfig{
			Plugin:     plugin,
			Subagent:   subagent,
			Additional: additional,
		})
	}
	return agents
}
