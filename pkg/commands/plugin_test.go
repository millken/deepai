package commands

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createPluginDir(t *testing.T, dir, name string) {
	t.Helper()
	manifest := `{"name":"` + name + `","version":"1.0.0","description":"test"}`
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPluginNameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/user/cool-plugin.git": "cool-plugin",
		"https://github.com/user/repo":            "repo",
		"git@github.com:user/team-plugin.git":     "team-plugin",
		"/home/me/src/myplugin":                   "myplugin",
	}
	for in, want := range cases {
		if got := pluginNameFromURL(in); got != want {
			t.Errorf("pluginNameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidatePluginName(t *testing.T) {
	for _, ok := range []string{"p", "my-plugin", "a1-b2"} {
		if err := validatePluginName(ok); err != nil {
			t.Errorf("unexpected error for %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Up", "has space", "has/slash", "..", "under_score"} {
		if err := validatePluginName(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestSafeRemoveTarget(t *testing.T) {
	base := t.TempDir()
	// Valid name → path inside base.
	got, err := safeRemoveTarget(base, "foo")
	if err != nil || filepath.Dir(got) != base {
		t.Fatalf("safeRemoveTarget(foo) = %q, %v", got, err)
	}
	// A name that would escape is rejected by the name validator first.
	if _, err := safeRemoveTarget(base, ".."); err == nil {
		t.Fatal("expected error for '..'")
	}
}

func TestResolveSubdir_Containment(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Subdir inside the repo is resolved.
	got, err := resolveSubdir(base, "inside")
	if err != nil || got != filepath.Join(base, "inside") {
		t.Fatalf("resolveSubdir(inside) = %q, %v", got, err)
	}
	// "./" prefix is fine.
	if _, err := resolveSubdir(base, "./inside"); err != nil {
		t.Fatalf("./inside should resolve: %v", err)
	}
	// "../" escape is rejected.
	if _, err := resolveSubdir(base, "../escape"); err == nil {
		t.Fatal("--subdir ../ must be rejected")
	}
}

func TestRunPluginAdd_GlobalSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	createPluginDir(t, src, "demo")

	if err := runPluginAdd(src, "demo", false, false); err != nil {
		t.Fatalf("add: %v", err)
	}
	link, err := pluginsDir(false)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(link, "demo")
	target, err := os.Readlink(dest)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", dest, err)
	}
	if target != src {
		t.Fatalf("symlink target = %q, want %q", target, src)
	}

	// Duplicate without --force fails.
	if err := runPluginAdd(src, "demo", false, false); err == nil {
		t.Fatal("duplicate add should fail without --force")
	}
	// --force overwrites.
	if err := runPluginAdd(src, "demo", false, true); err != nil {
		t.Fatalf("force add: %v", err)
	}
}

func TestRunPluginAdd_RejectsNonPlugin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	empty := t.TempDir() // no plugin.json
	if err := runPluginAdd(empty, "", false, false); err == nil {
		t.Fatal("add should reject a directory without plugin.json")
	}
}

func TestRunPluginRemove_SymlinkPreservesTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	createPluginDir(t, src, "demo")
	if err := runPluginAdd(src, "demo", false, false); err != nil {
		t.Fatal(err)
	}
	if err := runPluginRemove("demo", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Symlink gone, source intact.
	link, _ := pluginsDir(false)
	if exists(filepath.Join(link, "demo")) {
		t.Fatal("symlink should be removed")
	}
	if _, err := os.Stat(filepath.Join(src, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal("source plugin dir must be preserved when removing a symlink")
	}
}

func TestRunPluginRemove_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := runPluginRemove("nope", false); err == nil {
		t.Fatal("removing a nonexistent plugin should error")
	}
}

func TestRunPluginRemove_DirectoryClone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Simulate a clone install by placing a real plugin dir (not a symlink).
	dir, _ := pluginsDir(false)
	os.MkdirAll(dir, 0o755)
	clone := filepath.Join(dir, "cloned")
	createPluginDir(t, clone, "cloned")
	if err := runPluginRemove("cloned", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if exists(clone) {
		t.Fatal("cloned dir should be removed")
	}
}

func TestRunPluginList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	createPluginDir(t, src, "demo")
	if err := runPluginAdd(src, "demo", false, false); err != nil {
		t.Fatal(err)
	}
	// Should not error; output goes to stdout.
	if err := runPluginList(); err != nil {
		t.Fatalf("list: %v", err)
	}
}

func TestRunPluginList_NonPluginDirIsInvalid(t *testing.T) {
	// A directory in plugins/ that has no plugin.json must read as invalid,
	// not silently "ok" (regression guard for the LoadPlugin ""/"" case).
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, _ := pluginsDir(false)
	if err := os.MkdirAll(filepath.Join(dir, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := runPluginList(); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if !strings.Contains(out, "stray") || !strings.Contains(out, "invalid") {
		t.Fatalf("stray dir must be listed as invalid; got:\n%s", out)
	}
	if strings.Contains(out, "(ok)") {
		t.Fatalf("no entry should be ok when only a non-plugin dir exists; got:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns the
// captured output. Used to assert on runPluginList's printed output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}
