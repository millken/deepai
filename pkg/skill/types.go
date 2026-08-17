package skill

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Skill represents a single skill loaded from a SKILL.md file.
type Skill struct {
	Dir    string      // skill directory path
	Meta   Frontmatter // parsed frontmatter
	Body   string      // markdown body (lazy loaded)
	Loaded bool        // whether body has been loaded
	Source string      // origin: "global", "project", "plugin", ""
	mtime  time.Time   // file modification time for hot reload
}

// Frontmatter holds the YAML frontmatter fields from SKILL.md.
// All fields are optional.
type Frontmatter struct {
	Name                   string     `yaml:"name"`
	Description            string     `yaml:"description"`
	ArgumentHint           string     `yaml:"argument-hint"`
	DisableModelInvocation bool       `yaml:"disable-model-invocation"`
	UserInvocable          *bool      `yaml:"user-invocable"` // nil = true
	AllowedTools           StringList `yaml:"allowed-tools"`
	Model                  string     `yaml:"model"`
	Effort                 string     `yaml:"effort"`
	Paths                  StringList `yaml:"paths"`
	Shell                  string     `yaml:"shell"`

	// DeepAI extensions
	MaxTurns    *int     `yaml:"max-turns,omitempty"`
	Temperature *float64 `yaml:"temperature,omitempty"`
}

// IsUserInvocable returns whether the skill can be invoked by users.
// nil is treated as true.
func (f *Frontmatter) IsUserInvocable() bool {
	if f.UserInvocable == nil {
		return true
	}
	return *f.UserInvocable
}

// IsAutoInvocable returns whether the LLM can auto-invoke this skill.
func (f *Frontmatter) IsAutoInvocable() bool {
	return !f.DisableModelInvocation
}

// DisplayName returns the skill name for display, falling back to directory name.
func (s *Skill) DisplayName() string {
	if s.Meta.Name != "" {
		return s.Meta.Name
	}
	return s.Dir
}

// DynamicInjection represents a parsed !`command` block.
type DynamicInjection struct {
	Raw     string // original !`command` text
	Command string // extracted command
	Start   int    // start position in content
	End     int    // end position in content
	HasArgs bool   // whether command references $ARGUMENTS / $N
}

// StringList is a []string that supports YAML unmarshaling from both
// a list (["Read", "Grep"]) and a space-separated string ("Read Grep Glob").
type StringList []string

func (sl *StringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		// String format: "Read Grep Glob"
		*sl = strings.Fields(value.Value)
		return nil
	}
	if value.Kind == yaml.SequenceNode {
		// List format: ["Read", "Grep"]
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*sl = list
		return nil
	}
	return nil
}
