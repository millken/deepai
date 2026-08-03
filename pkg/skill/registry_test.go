package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistry_LoadFromDir(t *testing.T) {
	dir := t.TempDir()

	createSkillDir(t, filepath.Join(dir, "code-review"), "code-review", "Code review skill")
	createSkillDir(t, filepath.Join(dir, "deploy"), "deploy", "Deploy skill")
	os.MkdirAll(filepath.Join(dir, "not-a-skill"), 0755)

	reg := NewRegistry()
	if err := reg.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}

	if reg.Count() != 2 {
		t.Errorf("Count = %d, want 2", reg.Count())
	}

	if s := reg.Get("code-review"); s == nil || s.Meta.Description != "Code review skill" {
		t.Error("code-review skill not found or wrong description")
	}

	if s := reg.Get("deploy"); s == nil || s.Meta.Description != "Deploy skill" {
		t.Error("deploy skill not found or wrong description")
	}
}

func TestRegistry_ResolveCommand(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, filepath.Join(dir, "fix-issue"), "fix-issue", "Fix GitHub issue")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	tests := []struct {
		input    string
		wantName string
		wantArgs string
		wantOK   bool
	}{
		{"/fix-issue 123", "fix-issue", "123", true},
		{"/fix-issue", "fix-issue", "", true},
		{"/nonexistent", "", "", false},
		{"fix-issue 123", "", "", false},
		{"", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			name, args, ok := reg.ResolveCommand(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if args != tt.wantArgs {
				t.Errorf("args = %q, want %q", args, tt.wantArgs)
			}
		})
	}
}

func TestRegistry_Descriptions(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, filepath.Join(dir, "auto-skill"), "auto-skill", "Auto-invoked skill", false)
	createSkillDir(t, filepath.Join(dir, "manual-skill"), "manual-skill", "Manual only skill", true)

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	desc := reg.Descriptions()

	if !strings.Contains(desc, "/auto-skill") {
		t.Error("auto-skill missing from descriptions")
	}
	if strings.Contains(desc, "/manual-skill") {
		t.Error("manual-skill should not appear in descriptions")
	}
}

// TestDescriptionsFiltered_SanitizesBlankLinesInDescription is the RED test
// for M4-3 review r2 F2-a: a SKILL.md YAML frontmatter description can be a
// block scalar containing a blank line (e.g. a wrapped paragraph). Rendered
// verbatim, that blank line becomes a literal "\n\n" inside the catalog
// entry — indistinguishable, to pkg/agent's react.go removeSkillDescriptions
// (which ends the catalog block at the FIRST "\n\n" after the marker, since
// that is where AppendSystemPrompt's own section separator normally lives),
// from the boundary between the catalog and whatever was appended after it.
// A multi-line description therefore truncates the strip early, leaking the
// rest of the catalog (and any content the model appended after it) into
// the system prompt indefinitely once a skill is loaded and carried
// (M4-3). Fix (this test targets it directly, at the source): sanitize each
// description the same way pkg/agent's renderDelegationPrompt already
// sanitizes agent descriptions — trim, then collapse newlines to spaces —
// so the rendered catalog can never contain an embedded blank line.
func TestDescriptionsFiltered_SanitizesBlankLinesInDescription(t *testing.T) {
	reg := NewRegistry()
	reg.skills = map[string]*Skill{
		"one": {Meta: Frontmatter{
			Name:        "one",
			Description: "does one thing.\n\ncontinued description line",
		}},
		"two": {Meta: Frontmatter{
			Name:        "two",
			Description: "does two things",
		}},
	}

	desc := reg.Descriptions()

	if strings.Contains(desc, "\n\n") {
		t.Fatalf("rendered catalog contains a blank line — react.go's removeSkillDescriptions would "+
			"mistake it for the catalog's end boundary and leak the rest of the catalog into the system "+
			"prompt (review r2 F2-a), got: %q", desc)
	}
	// ReplaceAll("\n", " ") (matching renderDelegationPrompt's exact
	// approach) turns the description's blank line ("\n\n") into two
	// spaces, not one — checking for the collapsed prefix/suffix (rather
	// than asserting exact single-space spacing) avoids over-specifying
	// that incidental detail.
	if !strings.Contains(desc, "- /one: does one thing.") || !strings.Contains(desc, "continued description line") {
		t.Fatalf("expected the multi-line description collapsed onto one line, got: %q", desc)
	}
	if !strings.Contains(desc, "- /two: does two things") {
		t.Fatalf("expected the single-line description unaffected, got: %q", desc)
	}
}

func TestRegistry_AvailableNames(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, filepath.Join(dir, "aaa"), "aaa", "First")
	createSkillDir(t, filepath.Join(dir, "zzz"), "zzz", "Last")
	createSkillDir(t, filepath.Join(dir, "mmm"), "mmm", "Middle")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	names := reg.AvailableNames()
	if len(names) != 3 {
		t.Fatalf("len = %d, want 3", len(names))
	}
	if names[0] != "aaa" || names[1] != "mmm" || names[2] != "zzz" {
		t.Errorf("names = %v, want [aaa mmm zzz]", names)
	}
}

func TestRegistry_Reload(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test-skill")
	createSkillDir(t, skillDir, "test-skill", "Original description")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	if s := reg.Get("test-skill"); s.Meta.Description != "Original description" {
		t.Fatal("initial load failed")
	}

	createSkillDir(t, skillDir, "test-skill", "Updated description")
	if err := os.Chtimes(filepath.Join(skillDir, "SKILL.md"), time.Now().Add(time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := reg.Reload("test-skill"); err != nil {
		t.Fatal(err)
	}

	if s := reg.Get("test-skill"); s.Meta.Description != "Updated description" {
		t.Errorf("after reload: Description = %q, want %q", s.Meta.Description, "Updated description")
	}
}

func TestRegistry_LoadBody(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test-skill")
	createSkillDir(t, skillDir, "test-skill", "Test skill")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	skill := reg.Get("test-skill")
	if skill.Loaded || skill.Body != "" {
		t.Fatal("skill should not be loaded initially")
	}

	body, err := reg.LoadBody("test-skill")
	if err != nil {
		t.Fatal(err)
	}
	if body == "" || !strings.Contains(body, "Some body content") {
		t.Fatal("body should be loaded")
	}

	skill = reg.Get("test-skill")
	if !skill.Loaded || skill.Body != body {
		t.Error("body should be cached")
	}

	body2, _ := reg.LoadBody("test-skill")
	if body2 != body {
		t.Error("second LoadBody should return cached")
	}

	_, err = reg.LoadBody("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent skill")
	}
}

func TestRegistry_Unload(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, filepath.Join(dir, "temp"), "temp", "Temporary")

	reg := NewRegistry()
	reg.LoadFromDir(dir)
	reg.Unload("temp")

	if reg.Count() != 0 {
		t.Error("expected 0 skills after unload")
	}
}

func TestRegistry_List(t *testing.T) {
	dir := t.TempDir()
	createSkillDir(t, filepath.Join(dir, "b"), "b-skill", "B")
	createSkillDir(t, filepath.Join(dir, "a"), "a-skill", "A")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	skills := reg.List()
	if len(skills) != 2 || skills[0].Meta.Name != "a-skill" || skills[1].Meta.Name != "b-skill" {
		t.Error("skills not sorted by name")
	}
}

// ---------------------------------------------------------------------------
// Feature 1: Description Refresh
// ---------------------------------------------------------------------------

func TestDescriptions_AutoRefresh(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "refresh-test")
	createSkillDir(t, skillDir, "refresh-test", "Original")

	reg := NewRegistry().WithRefreshInterval(50 * time.Millisecond)
	reg.LoadFromDir(dir)

	desc1 := reg.Descriptions()
	if !strings.Contains(desc1, "Original") {
		t.Fatal("initial description should contain Original")
	}

	// Modify and wait for refresh
	createSkillDir(t, skillDir, "refresh-test", "Updated")
	if err := os.Chtimes(filepath.Join(skillDir, "SKILL.md"), time.Now().Add(time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	desc2 := reg.Descriptions()
	if !strings.Contains(desc2, "Updated") {
		t.Error("description should auto-refresh after interval")
	}
}

func TestRefreshDescriptions_Explicit(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "explicit-test")
	createSkillDir(t, skillDir, "explicit-test", "Before")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	reg.RefreshDescriptions()
	createSkillDir(t, skillDir, "explicit-test", "After")
	if err := os.Chtimes(filepath.Join(skillDir, "SKILL.md"), time.Now().Add(time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	desc := reg.Descriptions()
	if !strings.Contains(desc, "After") {
		t.Error("description should refresh after explicit RefreshDescriptions")
	}
}

// ---------------------------------------------------------------------------
// Feature 2: Multi-Storage Locations
// ---------------------------------------------------------------------------

func TestLoadAll_ProjectOverridesGlobal(t *testing.T) {
	globalDir := t.TempDir()
	projectDir := t.TempDir()
	createSkillDir(t, filepath.Join(globalDir, ".deepai", "skills", "my-skill"), "my-skill", "Global version")
	createSkillDir(t, filepath.Join(projectDir, ".deepai", "skills", "my-skill"), "my-skill", "Project version")

	reg := NewRegistry()
	reg.LoadAll(projectDir, nil)

	s := reg.Get("my-skill")
	if s == nil {
		t.Fatal("skill not found")
	}
	if s.Meta.Description != "Project version" {
		t.Errorf("project should override global, got %q", s.Meta.Description)
	}
	if s.Source != "project" {
		t.Errorf("source = %q, want %q", s.Source, "project")
	}
}

func TestLoadAll_PluginOverridesProject(t *testing.T) {
	projectDir := t.TempDir()
	pluginDir := t.TempDir()
	createSkillDir(t, filepath.Join(projectDir, ".deepai", "skills", "my-skill"), "my-skill", "Project version")
	createSkillDir(t, filepath.Join(pluginDir, "skills", "my-skill"), "my-skill", "Plugin version")

	reg := NewRegistry()
	reg.LoadAll(projectDir, []string{pluginDir})

	s := reg.Get("my-skill")
	if s == nil {
		t.Fatal("skill not found")
	}
	if s.Meta.Description != "Plugin version" {
		t.Errorf("plugin should override project, got %q", s.Meta.Description)
	}
	if s.Source != "plugin" {
		t.Errorf("source = %q, want %q", s.Source, "plugin")
	}
}

func TestLoadAll_NonexistentDirs(t *testing.T) {
	reg := NewRegistry()
	// Should not error on non-existent dirs
	err := reg.LoadAll("/nonexistent/project", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg.Count() != 0 {
		t.Error("no skills should be loaded")
	}
}

// ---------------------------------------------------------------------------
// Feature 3: Paths Auto-Activation
// ---------------------------------------------------------------------------

func TestMatchPaths(t *testing.T) {
	dir := t.TempDir()
	createSkillDirWithPaths(t, filepath.Join(dir, "go-skill"), "go-skill", "Go files skill", "*.go")
	createSkillDirWithPaths(t, filepath.Join(dir, "readme-skill"), "readme-skill", "Readme skill", "**/*.md")
	createSkillDir(t, filepath.Join(dir, "always-skill"), "always-skill", "Always active")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	matched := reg.MatchPaths("src/main.go")
	if len(matched) != 1 || matched[0].Meta.Name != "go-skill" {
		t.Errorf("expected go-skill, got %v", namesOf(matched))
	}

	matched = reg.MatchPaths("docs/readme.md")
	if len(matched) != 1 || matched[0].Meta.Name != "readme-skill" {
		t.Errorf("expected readme-skill, got %v", namesOf(matched))
	}

	// No paths skill excluded from MatchPaths
	matched = reg.MatchPaths("anything")
	if len(matched) != 0 {
		t.Errorf("skills with empty paths should not appear in MatchPaths, got %v", namesOf(matched))
	}
}

func TestDescriptionsFiltered(t *testing.T) {
	dir := t.TempDir()
	createSkillDirWithPaths(t, filepath.Join(dir, "go-skill"), "go-skill", "Go files", "*.go")
	createSkillDir(t, filepath.Join(dir, "any-skill"), "any-skill", "Any file")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	// Empty path = all skills
	desc := reg.DescriptionsFiltered("")
	if !strings.Contains(desc, "/go-skill") || !strings.Contains(desc, "/any-skill") {
		t.Error("empty filter should include all skills")
	}

	// Filtered by path — skills with empty Paths are always active
	desc = reg.DescriptionsFiltered("src/main.go")
	if !strings.Contains(desc, "/go-skill") {
		t.Error("go-skill should appear for .go files")
	}
	if !strings.Contains(desc, "/any-skill") {
		t.Error("any-skill has no paths constraint, should always appear")
	}
}

// ---------------------------------------------------------------------------
// Feature 4: Monorepo Nested Discovery
// ---------------------------------------------------------------------------

func TestDiscoverNested_FindsSkills(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "pkg", "auth")
	os.MkdirAll(subDir, 0755)
	createSkillDir(t, filepath.Join(subDir, ".deepai", "skills", "auth-convention"), "auth-convention", "Auth conventions")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	filePath := filepath.Join(subDir, "handler.go")
	err := reg.DiscoverNested(dir, filePath)
	if err != nil {
		t.Fatal(err)
	}

	nested := reg.MatchNested(filePath)
	if len(nested) != 1 {
		t.Fatalf("expected 1 nested skill, got %d", len(nested))
	}
	if nested[0].Meta.Name != "auth-convention" {
		t.Errorf("expected auth-convention, got %q", nested[0].Meta.Name)
	}
}

func TestDiscoverNested_CacheHit(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "pkg", "auth")
	os.MkdirAll(subDir, 0755)
	createSkillDir(t, filepath.Join(subDir, ".deepai", "skills", "cached"), "cached", "Cached skill")

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	filePath := filepath.Join(subDir, "handler.go")
	reg.DiscoverNested(dir, filePath)
	first := reg.Count()

	// Modify the skill - should not be re-scanned (cache hit)
	createSkillDir(t, filepath.Join(subDir, ".deepai", "skills", "cached"), "cached", "Modified")
	second := reg.Count()
	if second != first {
		t.Errorf("cache hit: count should not change, got %d then %d", first, second)
	}
}

func TestDiscoverNested_NoSkillsDir(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "pkg", "no-skills")
	os.MkdirAll(subDir, 0755)

	reg := NewRegistry()
	reg.LoadFromDir(dir)

	err := reg.DiscoverNested(dir, filepath.Join(subDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.Count() != 0 {
		t.Error("no skills should be loaded from dirs without .deepai/skills/")
	}
}

func TestClearNestedCache(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "pkg", "auth")
	os.MkdirAll(subDir, 0755)
	createSkillDir(t, filepath.Join(subDir, ".deepai", "skills", "clearable"), "clearable", "Clearable")

	reg := NewRegistry()
	reg.LoadFromDir(dir)
	reg.DiscoverNested(dir, filepath.Join(subDir, "main.go"))
	reg.ClearNestedCache()

	if len(reg.nestedCache) != 0 {
		t.Error("nested cache should be empty after ClearNestedCache")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func createSkillDir(t *testing.T, dir, name, desc string, disableModel ...bool) {
	t.Helper()
	os.MkdirAll(dir, 0755)

	fm := "---\n"
	fm += "name: " + name + "\n"
	fm += "description: " + desc + "\n"
	if len(disableModel) > 0 && disableModel[0] {
		fm += "disable-model-invocation: true\n"
	}
	fm += "---\n\nSome body content.\n"

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(fm), 0644); err != nil {
		t.Fatal(err)
	}
}

func createSkillDirWithPaths(t *testing.T, dir, name, desc string, paths ...string) {
	t.Helper()
	os.MkdirAll(dir, 0755)

	fm := "---\n"
	fm += "name: " + name + "\n"
	fm += "description: " + desc + "\n"
	if len(paths) > 0 {
		fm += "paths:\n"
		for _, p := range paths {
			fm += "  - \"" + p + "\"\n"
		}
	}
	fm += "---\n\nSome body content.\n"

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(fm), 0644); err != nil {
		t.Fatal(err)
	}
}

func namesOf(skills []*Skill) []string {
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Meta.Name
	}
	return names
}

func TestLoadAllReported_ReadDirFailureWarns(t *testing.T) {
	// <pluginRoot>/skills is a file (not a dir) → ReadDir fails → warning.
	pluginRoot := t.TempDir()
	skillsPath := filepath.Join(pluginRoot, "skills")
	if err := os.WriteFile(skillsPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	warnings := reg.LoadAllReported("", []string{pluginRoot})
	if len(warnings) != 1 {
		t.Fatalf("want 1 warning, got %+v", warnings)
	}
	w := warnings[0]
	if w.Source != "plugin" {
		t.Fatalf("source = %q, want plugin", w.Source)
	}
	if w.Dir != skillsPath {
		t.Fatalf("dir = %q, want %q", w.Dir, skillsPath)
	}
}

func TestLoadAllReported_MissingDirSilent(t *testing.T) {
	reg := NewRegistry()
	warnings := reg.LoadAllReported("", []string{"/nonexistent/deepai-plugin"})
	if len(warnings) != 0 {
		t.Fatalf("missing dir should produce no warning, got %+v", warnings)
	}
}
