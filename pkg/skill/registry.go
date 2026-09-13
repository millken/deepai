package skill

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	skillFile           = "SKILL.md"
	descriptionKB       = 8000 // fallback char budget for descriptions
	defaultDescRefresh  = 60 * time.Second
	defaultNestedExpiry = 5 * time.Minute
)

// nestedEntry caches skills discovered from a nested directory.
type nestedEntry struct {
	skills   map[string]*Skill
	loadedAt time.Time
}

// Registry manages skill loading, lookup, and hot reload.
type Registry struct {
	mu     sync.RWMutex
	skills map[string]*Skill // name -> Skill

	// Feature 1: Description refresh
	lastDescRefresh     time.Time
	descRefreshInterval time.Duration

	// Feature 4: Monorepo nested discovery
	nestedCache  map[string]*nestedEntry // canonical dir -> entry
	nestedExpiry time.Duration
}

// NewRegistry creates an empty skill registry.
func NewRegistry() *Registry {
	return &Registry{
		skills:              make(map[string]*Skill),
		descRefreshInterval: defaultDescRefresh,
		nestedCache:         make(map[string]*nestedEntry),
		nestedExpiry:        defaultNestedExpiry,
	}
}

// WithRefreshInterval sets the description refresh interval.
func (r *Registry) WithRefreshInterval(d time.Duration) *Registry {
	r.descRefreshInterval = d
	return r
}

// ---------------------------------------------------------------------------
// Feature 1: Description Refresh
// ---------------------------------------------------------------------------

// RefreshDescriptions forces a description refresh on next Descriptions() call.
func (r *Registry) RefreshDescriptions() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastDescRefresh = time.Time{}
}

// maybeRefreshDescriptions checks staleness and refreshes if needed.
func (r *Registry) maybeRefreshDescriptions() {
	r.mu.RLock()
	stale := time.Since(r.lastDescRefresh) >= r.descRefreshInterval
	r.mu.RUnlock()
	if !stale {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastDescRefresh) < r.descRefreshInterval {
		return
	}

	for name, existing := range r.skills {
		skill, err := ParseSkill(existing.Dir)
		if err != nil {
			continue
		}
		if !skill.mtime.Equal(existing.mtime) {
			skill.Source = existing.Source
			r.skills[name] = skill
		}
	}
	r.lastDescRefresh = time.Now()
}

// ---------------------------------------------------------------------------
// Feature 2: Multi-Storage Locations
// ---------------------------------------------------------------------------

// LoadAll loads skills from all standard locations.
// Priority (highest wins): plugin > project > global.
// Non-existent directories are silently skipped; load errors (read/parse) are
// logged via slog. For access to the warnings (e.g. to surface plugin-skill
// failures in the UI), use LoadAllReported.
func (r *Registry) LoadAll(projectDir string, pluginDirs []string) error {
	for _, w := range r.LoadAllReported(projectDir, pluginDirs) {
		slog.Warn("skill load issue", "source", w.Source, "dir", w.Dir, "err", w.Msg)
	}
	return nil
}

// SkillWarning describes a skill load failure at a directory.
type SkillWarning struct {
	Source string // "global" | "project" | "plugin"
	Dir    string
	Msg    string
}

// LoadAllReported is like LoadAll but returns per-directory load warnings
// instead of logging them, so the caller can surface them (e.g. plugin-source
// warnings in the startup report). Missing directories are NOT warnings
// (legitimate); ReadDir/LoadFromDir failures are.
func (r *Registry) LoadAllReported(projectDir string, pluginDirs []string) []SkillWarning {
	var warnings []SkillWarning
	// Global: ~/.deepai/skills/
	if home, err := os.UserHomeDir(); err == nil {
		warnings = append(warnings, r.loadDirReported(filepath.Join(home, ".deepai", "skills"), "global")...)
	}
	// Project: <projectDir>/.deepai/skills/
	if projectDir != "" {
		warnings = append(warnings, r.loadDirReported(filepath.Join(projectDir, ".deepai", "skills"), "project")...)
	}
	// Plugin: <pluginDir>/skills/
	for _, pDir := range pluginDirs {
		warnings = append(warnings, r.loadDirReported(filepath.Join(pDir, "skills"), "plugin")...)
	}
	return warnings
}

// loadDirReported loads skills from dir, tagging them with source. Missing dir
// → nil (legitimate); ReadDir/LoadFromDir failure → a warning describing it.
func (r *Registry) loadDirReported(dir string, source string) []SkillWarning {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}

	// Collect skill names that exist in this directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []SkillWarning{{Source: source, Dir: dir, Msg: "read dir: " + err.Error()}}
	}
	localNames := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(dir, entry.Name(), skillFile)
		if _, err := os.Stat(skillPath); err == nil {
			localNames[entry.Name()] = struct{}{}
		}
	}

	if err := r.LoadFromDir(dir); err != nil {
		return []SkillWarning{{Source: source, Dir: dir, Msg: err.Error()}}
	}

	// Tag skills that came from this dir, and lint the fork/agent binding
	// (M5-1 §2.1) while we already hold the lock and know which skills this
	// directory just (re)loaded.
	var warnings []SkillWarning
	r.mu.Lock()
	for name := range localNames {
		if s, ok := r.skills[name]; ok {
			s.Source = source
			if msg := lintForkBinding(s.Meta); msg != "" {
				warnings = append(warnings, SkillWarning{Source: source, Dir: s.Dir, Msg: msg})
			}
		}
	}
	r.mu.Unlock()
	return warnings
}

// lintForkBinding checks context/agent frontmatter consistency for one
// skill, returning a human-readable warning (empty string if the skill is
// consistent). This is a warning, not a load failure — the skill still
// loads and can be inspected/fixed — because a hard failure here would
// remove an otherwise-usable skill entirely for one metadata mistake.
func lintForkBinding(m Frontmatter) string {
	hasAgent := strings.TrimSpace(m.Agent) != ""
	switch {
	case m.IsFork() && !hasAgent:
		return fmt.Sprintf("fork skill %q has no agent: binding", m.Name)
	case hasAgent && !m.IsFork():
		return fmt.Sprintf("skill %q has agent: %q set but context is not \"fork\" (context=%q)", m.Name, m.Agent, m.Context)
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Core: LoadFromDir, loadSkill, LoadBody
// ---------------------------------------------------------------------------

// LoadBody loads the markdown body of a skill (lazy loading).
// Returns the body and caches it in the skill. Subsequent calls return the cached copy.
func (r *Registry) LoadBody(name string) (string, error) {
	r.mu.RLock()
	skill, ok := r.skills[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	if skill.Loaded {
		return skill.Body, nil
	}

	path := filepath.Join(skill.Dir, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read SKILL.md: %w", err)
	}

	_, body, err := splitFrontmatter(data)
	if err != nil {
		return "", fmt.Errorf("parse SKILL.md body: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if skill.Loaded {
		return skill.Body, nil
	}
	skill.Body = body
	skill.Loaded = true
	return body, nil
}

// LoadFromDir scans a directory for SKILL.md files and loads their frontmatter.
// Does not reload already-registered skills unless their mtime changed.
func (r *Registry) LoadFromDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillDir := filepath.Join(dir, entry.Name())
		skillPath := filepath.Join(skillDir, skillFile)
		if _, err := os.Stat(skillPath); err != nil {
			continue
		}

		if err := r.loadSkill(skillDir); err != nil {
			// Best-effort: a broken SKILL.md shouldn't block the rest, but it
			// shouldn't be silent either — log so plugin/global skill parse
			// failures are diagnosable.
			slog.Warn("skill parse failed; skipping", "dir", skillDir, "err", err)
			continue
		}
	}

	return nil
}

// loadSkill loads or hot-reloads a single skill from its directory.
func (r *Registry) loadSkill(dir string) error {
	skill, err := ParseSkill(dir)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, exists := r.skills[skill.Meta.Name]
	// Skip only when same directory AND same mtime (hot-reload).
	// Different directory means a higher-priority source is overriding.
	if exists && existing.Dir == skill.Dir && existing.mtime.Equal(skill.mtime) {
		return nil
	}

	if exists {
		skill.Source = existing.Source
	}
	r.skills[skill.Meta.Name] = skill
	return nil
}

// ---------------------------------------------------------------------------
// Feature 3: Paths Auto-Activation
// ---------------------------------------------------------------------------

// MatchPaths returns skills whose paths patterns match the given filePath.
// Skills with empty Paths are excluded (they are always active).
func (r *Registry) MatchPaths(filePath string) []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*Skill
	for _, s := range r.skills {
		if len(s.Meta.Paths) == 0 {
			continue
		}
		if matchSkillPaths(s.Meta.Paths, filePath) {
			result = append(result, s)
		}
	}
	return result
}

// DescriptionsFiltered generates description text, optionally filtered by filePath.
// When filePath is empty, behaves like Descriptions().
// When filePath is non-empty, includes only skills with empty Paths OR matching paths.
func (r *Registry) DescriptionsFiltered(filePath string) string {
	desc, _ := r.describeSkills(func(s *Skill) bool {
		return filePath == "" || len(s.Meta.Paths) == 0 || matchSkillPaths(s.Meta.Paths, filePath)
	})
	return desc
}

// DescriptionsForAgent is Descriptions filtered for one subagent's own
// catalog injection (M5-1 §2.6 step 4): it additionally excludes a fork
// skill bound to a DIFFERENT agent type than agentType. Such a skill would
// still be listed as callable, but pkg/skill/tool.go's caller routing always
// refuses it for a mismatched subagent — advertising it just spends a tool
// call on a call that can never succeed. A fork skill bound to agentType
// itself, or an inline (non-fork) skill, is included exactly as in
// Descriptions.
//
// Unlike Descriptions/DescriptionsFiltered, this returns "" (not just a
// header) when every skill got filtered out: pkg/agent/subagent.go's
// `if desc != ""` guard would otherwise never catch a header-only string
// with zero entries, injecting a useless "Available skills:" block with
// nothing under it.
func (r *Registry) DescriptionsForAgent(agentType string) string {
	desc, hasEntries := r.describeSkills(func(s *Skill) bool {
		return !(s.Meta.IsFork() && s.Meta.Agent != agentType)
	})
	if !hasEntries {
		return ""
	}
	return desc
}

// describeSkills is the shared rendering skeleton behind Descriptions,
// DescriptionsFiltered, and DescriptionsForAgent: they differ only in which
// skills `include` lets through, not in refresh/locking/sort/budget
// behavior. The second return reports whether at least one skill line was
// actually written (as opposed to just the header) — Descriptions/
// DescriptionsFiltered ignore it (preserving their existing header-only-on-
// empty behavior); DescriptionsForAgent uses it to collapse to "".
func (r *Registry) describeSkills(include func(*Skill) bool) (string, bool) {
	r.maybeRefreshDescriptions()

	r.mu.RLock()
	defer r.mu.RUnlock()

	var buf strings.Builder
	buf.WriteString("Available skills (use the matching skill when the user request fits):\n")

	var skills []*Skill
	for _, s := range r.skills {
		if !s.Meta.IsAutoInvocable() {
			continue
		}
		if !include(s) {
			continue
		}
		skills = append(skills, s)
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Meta.Name < skills[j].Meta.Name
	})

	total := 0
	for _, s := range skills {
		// M4-3 review r2 F2-a: Description comes verbatim from SKILL.md YAML
		// frontmatter, which can be a block scalar containing blank lines
		// (e.g. a wrapped paragraph). Rendered as-is, an embedded blank line
		// becomes a literal "\n\n" inside this catalog entry —
		// indistinguishable from the boundary pkg/agent's
		// removeSkillDescriptions uses to find where the catalog block ends
		// (the first "\n\n" after the marker, matching AppendSystemPrompt's
		// own section separator), which would truncate the strip early and
		// leak the rest of the catalog into the system prompt once a skill
		// loads. Sanitize the same way renderDelegationPrompt already
		// sanitizes agent descriptions (pkg/agent/promptbuild.go): trim,
		// then collapse newlines to spaces, so a catalog entry can never
		// contain a blank line.
		desc := strings.ReplaceAll(strings.TrimSpace(s.Meta.Description), "\n", " ")
		line := fmt.Sprintf("- /%s: %s\n", s.Meta.Name, desc)
		if total+len(line) > descriptionKB {
			break
		}
		buf.WriteString(line)
		total += len(line)
	}

	return buf.String(), total > 0
}

// matchSkillPaths checks if any glob pattern matches the filePath.
func matchSkillPaths(patterns []string, filePath string) bool {
	for _, pattern := range patterns {
		if matchSkillPath(pattern, filePath) {
			return true
		}
	}
	return false
}

// matchSkillPath matches a single glob pattern against a file path.
// Supports **/ prefix for recursive matching.
func matchSkillPath(pattern, path string) bool {
	if strings.HasPrefix(pattern, "**/") {
		suffix := pattern[3:]
		// **/X matches if any suffix of path matches X as a glob
		// e.g. **/*.md should match docs/readme.md
		if !strings.Contains(suffix, "/") {
			// Simple case: **/*.ext → match basename
			matched, _ := filepath.Match(suffix, filepath.Base(path))
			return matched
		}
		// Multi-component case: **/foo/*.md → walk up path components
		for p := path; p != "" && p != "."; p = filepath.Dir(p) {
			if matched, _ := filepath.Match(suffix, p); matched {
				return true
			}
		}
		return false
	}
	matched, _ := filepath.Match(pattern, path)
	if matched {
		return true
	}
	matched, _ = filepath.Match(pattern, filepath.Base(path))
	return matched
}

// ---------------------------------------------------------------------------
// Feature 4: Monorepo Nested Discovery
// ---------------------------------------------------------------------------

// DiscoverNested walks up from filePath toward rootDir looking for
// .deepai/skills/ directories and loads any found skills.
// Results are cached per directory. Returns nil if nothing found.
func (r *Registry) DiscoverNested(rootDir, filePath string) error {
	dir := filepath.Dir(filePath)
	rootDir = filepath.Clean(rootDir)

	for dir != rootDir && dir != filepath.Dir(dir) {
		canonical := filepath.Clean(dir)

		r.mu.RLock()
		cached, exists := r.nestedCache[canonical]
		r.mu.RUnlock()
		if exists && time.Since(cached.loadedAt) < r.nestedExpiry {
			dir = filepath.Dir(dir)
			continue
		}

		// Check for .deepai/skills/
		nestedDir := filepath.Join(canonical, ".deepai", "skills")
		if _, err := os.Stat(nestedDir); os.IsNotExist(err) {
			dir = filepath.Dir(dir)
			continue
		}

		nestedReg := NewRegistry()
		if err := nestedReg.LoadFromDir(nestedDir); err != nil {
			dir = filepath.Dir(dir)
			continue
		}

		entry := &nestedEntry{
			skills:   nestedReg.skills,
			loadedAt: time.Now(),
		}

		r.mu.Lock()
		r.nestedCache[canonical] = entry
		r.mu.Unlock()

		dir = filepath.Dir(dir)
	}

	return nil
}

// MatchNested returns skills from nested directories that are ancestors of filePath.
func (r *Registry) MatchNested(filePath string) []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*Skill
	for dir, entry := range r.nestedCache {
		if time.Since(entry.loadedAt) >= r.nestedExpiry {
			continue
		}
		canonical := filepath.Clean(dir)
		fileDir := filepath.Clean(filepath.Dir(filePath))
		if strings.HasPrefix(fileDir, canonical) || fileDir == canonical {
			for _, s := range entry.skills {
				result = append(result, s)
			}
		}
	}
	return result
}

// ClearNestedCache removes all cached nested discovery results.
func (r *Registry) ClearNestedCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nestedCache = make(map[string]*nestedEntry)
}

// ---------------------------------------------------------------------------
// Core: Get, AvailableNames, ResolveCommand, Descriptions, List, Reload, Unload, Count
// ---------------------------------------------------------------------------

// Get returns a skill by name. Returns nil if not found.
func (r *Registry) Get(name string) *Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.skills[name]
}

// AvailableNames returns all registered skill names.
func (r *Registry) AvailableNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.skills))
	for name := range r.skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveCommand parses "/command args" input and returns (skillName, args, ok).
func (r *Registry) ResolveCommand(input string) (string, string, bool) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return "", "", false
	}

	parts := strings.SplitN(input, " ", 2)
	name := strings.TrimPrefix(parts[0], "/")

	r.mu.RLock()
	_, ok := r.skills[name]
	r.mu.RUnlock()

	if !ok {
		return "", "", false
	}

	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	return name, args, true
}

// Descriptions generates a summary text for injection into system prompts.
// Excludes skills with disable-model-invocation: true.
// Auto-refreshes if stale (see WithRefreshInterval).
func (r *Registry) Descriptions() string {
	return r.DescriptionsFiltered("")
}

// List returns all registered skills (for iteration).
func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	skills := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Meta.Name < skills[j].Meta.Name
	})
	return skills
}

// Reload checks a specific skill's mtime and reloads if changed.
func (r *Registry) Reload(name string) error {
	r.mu.RLock()
	existing, ok := r.skills[name]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("skill %s not found", name)
	}

	skill, err := ParseSkill(existing.Dir)
	if err != nil {
		return err
	}

	if skill.mtime.Equal(existing.mtime) {
		return nil
	}

	r.mu.Lock()
	skill.Source = existing.Source
	r.skills[name] = skill
	r.mu.Unlock()

	return nil
}

// Unload removes a skill from the registry.
func (r *Registry) Unload(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.skills, name)
}

// Count returns the number of registered skills.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skills)
}
