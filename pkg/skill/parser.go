package skill

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	frontmatterOpen  = "---\n"
	frontmatterClose = "\n---\n"
	maxDescription   = 250
	maxNameLength    = 64
)

// nameRegex validates skill names: lowercase letters, digits, hyphens only.
var nameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ParseSkill reads a SKILL.md file and returns a Skill with parsed frontmatter.
// The body is NOT loaded — call Registry.LoadBody to load it on demand.
func ParseSkill(dir string) (*Skill, error) {
	path := filepath.Join(dir, "SKILL.md")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SKILL.md: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat SKILL.md: %w", err)
	}

	fm, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("parse SKILL.md: %w", err)
	}

	// Trim description if needed
	if len(fm.Description) > maxDescription {
		fm.Description = fm.Description[:maxDescription]
	}

	// Fallback: if name is empty, use directory name
	if fm.Name == "" {
		fm.Name = filepath.Base(dir)
	}

	// Validate name format
	if len(fm.Name) > maxNameLength {
		return nil, fmt.Errorf("name %q exceeds %d characters", fm.Name, maxNameLength)
	}
	if !nameRegex.MatchString(fm.Name) {
		return nil, fmt.Errorf("name %q invalid: must contain only lowercase letters, digits, and hyphens", fm.Name)
	}

	// Fallback: if description is empty, use first paragraph of body
	if fm.Description == "" && body != "" {
		fm.Description = firstParagraph(body)
		if len(fm.Description) > maxDescription {
			fm.Description = fm.Description[:maxDescription]
		}
	}

	return &Skill{
		Dir:    dir,
		Meta:   fm,
		Loaded: false, // body loaded lazily via Registry.LoadBody
		mtime:  info.ModTime(),
	}, nil
}

// splitFrontmatter separates YAML frontmatter from markdown body.
func splitFrontmatter(data []byte) (Frontmatter, string, error) {
	var fm Frontmatter

	if !bytes.HasPrefix(data, []byte(frontmatterOpen)) {
		// No frontmatter, entire content is body
		return fm, string(data), nil
	}

	// Find closing ---
	rest := data[len(frontmatterOpen):]
	closeIdx := bytes.Index(rest, []byte("\n---"))
	if closeIdx < 0 {
		return fm, string(data), nil
	}

	fmData := rest[:closeIdx]
	bodyData := rest[closeIdx+len("\n---"):]

	// Trim leading newline from body
	bodyData = bytes.TrimPrefix(bodyData, []byte("\n"))

	if err := yaml.Unmarshal(fmData, &fm); err != nil {
		return fm, string(bodyData), fmt.Errorf("parse frontmatter: %w", err)
	}

	return fm, string(bodyData), nil
}

// firstParagraph extracts the first non-empty paragraph from markdown content.
func firstParagraph(s string) string {
	lines := strings.Split(s, "\n")
	var buf strings.Builder
	started := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if started {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue // skip headings
		}
		if !started {
			started = true
		}
		buf.WriteString(trimmed)
		buf.WriteString(" ")
	}

	return strings.TrimSpace(buf.String())
}
