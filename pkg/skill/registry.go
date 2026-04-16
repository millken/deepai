package skill

import (
	"fmt"
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

// WithNestedExpiry sets the nested discovery cache TTL.
func (r *Registry) WithNestedExpiry(d time.Duration) *Registry {
	r.nestedExpiry = d
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
// Non-existent directories are silently skipped.
func (r *Registry) LoadAll(projectDir string, pluginDirs []string) error {
	// Global: ~/.deepai/skills/
	home, err := os.UserHomeDir()
	if err == nil {
		r.loadDirSilent(filepath.Join(home, ".deepai", "skills"), "global")
	}

	// Project: <projectDir>/.deepai/skills/
	if projectDir != "" {
		r.loadDirSilent(filepath.Join(projectDir, ".deepai", "skills"), "project")
	}

	// Plugin: <pluginDir>/skills/
	for _, pDir := range pluginDirs {
		r.loadDirSilent(filepath.Join(pDir, "skills"), "plugin")
	}

	return nil
}

// loadDirSilent loads from dir with source tag, ignoring non-existent dirs.
// Skills loaded from this dir are tagged with the given source.
func (r *Registry) loadDirSilent(dir string, source string) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return
	}

	// Collect skill names that exist in this directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
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
		return
	}

	// Tag skills that came from this dir
	r.mu.Lock()
	for name := range localNames {
		if s, ok := r.skills[name]; ok {
			s.Source = source
		}
	}
	r.mu.Unlock()
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
		// If filePath given, filter by paths
		if filePath != "" && len(s.Meta.Paths) > 0 && !matchSkillPaths(s.Meta.Paths, filePath) {
			continue
		}
		skills = append(skills, s)
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Meta.Name < skills[j].Meta.Name
	})

	total := 0
	for _, s := range skills {
		line := fmt.Sprintf("- /%s: %s\n", s.Meta.Name, s.Meta.Description)
		if total+len(line) > descriptionKB {
			break
		}
		buf.WriteString(line)
		total += len(line)
	}

	return buf.String()
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

		// Check cache
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

		// Load skills from nested directory
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
