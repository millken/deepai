package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/millken/deepai/pkg/claudeplugin"
	"github.com/spf13/cobra"
)

// pluginNameRE matches valid plugin names: lowercase letters, digits, hyphens.
var pluginNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func addPlugin(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage Claude plugins (install/add/list/remove)",
	}
	cmd.AddCommand(newPluginInstallCmd(), newPluginAddCmd(), newPluginListCmd(), newPluginRemoveCmd())
	topLevel.AddCommand(cmd)
}

func newPluginInstallCmd() *cobra.Command {
	var name, subdir string
	var project, force bool
	c := &cobra.Command{
		Use:   "install <git-url>",
		Short: "Clone a plugin from a git URL into the plugins directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginInstall(args[0], name, subdir, project, force)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Plugin name (default: derived from URL)")
	c.Flags().StringVar(&subdir, "subdir", "", "Use this subdirectory of the repo as the plugin root (marketplace single-plugin)")
	c.Flags().BoolVar(&project, "project", false, "Install into <cwd>/.deepai/plugins instead of ~/.deepai/plugins")
	c.Flags().BoolVar(&force, "force", false, "Overwrite if already installed")
	return c
}

func newPluginAddCmd() *cobra.Command {
	var name string
	var project, force bool
	c := &cobra.Command{
		Use:   "add <path>",
		Short: "Symlink a local plugin directory into the plugins directory (dev)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginAdd(args[0], name, project, force)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Plugin name (default: directory basename)")
	c.Flags().BoolVar(&project, "project", false, "Link into <cwd>/.deepai/plugins instead of ~/.deepai/plugins")
	c.Flags().BoolVar(&force, "force", false, "Overwrite if already installed")
	return c
}

func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed plugins (global + project)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginList()
		},
	}
}

func newPluginRemoveCmd() *cobra.Command {
	var project bool
	c := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed plugin (clone or symlink)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginRemove(args[0], project)
		},
	}
	c.Flags().BoolVar(&project, "project", false, "Remove from <cwd>/.deepai/plugins instead of ~/.deepai/plugins")
	return c
}

// --- run ---

func runPluginInstall(url, name, subdir string, project, force bool) error {
	pluginsDir, err := pluginsDir(project)
	if err != nil {
		return err
	}
	if name == "" {
		name = pluginNameFromURL(url)
	}
	if err := validatePluginName(name); err != nil {
		return err
	}
	dest := filepath.Join(pluginsDir, name)
	if exists(dest) {
		if !force {
			return fmt.Errorf("plugin %q already exists at %s (use --force to overwrite)", name, dest)
		}
		if err := removePlugin(pluginsDir, name); err != nil {
			return err
		}
	}

	// Clone into a staging dir alongside the destination (same filesystem so the
	// final rename is atomic; cleaned up on any failure).
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return fmt.Errorf("create plugins dir: %w", err)
	}
	staging := filepath.Join(pluginsDir, ".staging-"+name)
	_ = os.RemoveAll(staging)
	cmd := exec.Command("git", "clone", "--depth", "1", url, staging)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("git clone: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	src := staging
	label := name
	if strings.TrimSpace(subdir) != "" {
		resolved, err := resolveSubdir(staging, subdir)
		if err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
		src = resolved
		label = fmt.Sprintf("%s (subdir %s)", name, subdir)
	}
	if err := validatePluginDir(src); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("not a valid plugin (%s): %w", src, err)
	}

	if err := os.Rename(src, dest); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("move into place: %w", err)
	}
	// If we moved a subdir out, drop the rest of the staging clone.
	if src != staging {
		_ = os.RemoveAll(staging)
	}
	fmt.Printf("installed %s -> %s\n", label, dest)
	return nil
}

func runPluginAdd(src, name string, project, force bool) error {
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	if err := validatePluginDir(abs); err != nil {
		return fmt.Errorf("not a valid plugin (%s): %w", abs, err)
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	if err := validatePluginName(name); err != nil {
		return err
	}
	pluginsDir, err := pluginsDir(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(pluginsDir, name)
	if exists(dest) {
		if !force {
			return fmt.Errorf("plugin %q already exists at %s (use --force to overwrite)", name, dest)
		}
		if err := removePlugin(pluginsDir, name); err != nil {
			return err
		}
	}
	if err := os.Symlink(abs, dest); err != nil {
		return fmt.Errorf("symlink: %w", err)
	}
	fmt.Printf("linked %s -> %s\n", name, abs)
	return nil
}

func runPluginList() error {
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}
	scopes := []struct {
		name string
		dir  string
	}{
		{"global", globalPluginsDir()},
		{"project", projectPluginsDir(workdir)},
	}
	any := false
	for _, sc := range scopes {
		entries, err := os.ReadDir(sc.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && e.Type() != os.ModeSymlink {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			dir := filepath.Join(sc.dir, name)
			// Reuse validatePluginDir so "dir exists but isn't a plugin" (no
			// manifest → LoadPlugin returns a zero Plugin with no problem) is
			// flagged invalid, not silently "ok".
			if err := validatePluginDir(dir); err != nil {
				fmt.Printf("%-20s %-8s %s  (invalid: %s)\n", name, sc.name, dir, err)
			} else {
				display := name
				if p, _ := claudeplugin.LoadPlugin(dir); p.Name != "" {
					display = p.Name
				}
				fmt.Printf("%-20s %-8s %s  (ok)\n", display, sc.name, dir)
			}
			any = true
		}
	}
	if !any {
		fmt.Println("(no plugins installed)")
	}
	return nil
}

func runPluginRemove(name string, project bool) error {
	pluginsDir, err := pluginsDir(project)
	if err != nil {
		return err
	}
	target, err := safeRemoveTarget(pluginsDir, name)
	if err != nil {
		return err
	}
	if !exists(target) {
		return fmt.Errorf("no such plugin: %s (in %s)", name, pluginsDir)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	fmt.Printf("removed %s\n", name)
	return nil
}

// --- helpers ---

func globalPluginsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".deepai", "plugins")
}

func projectPluginsDir(workdir string) string {
	return filepath.Join(workdir, ".deepai", "plugins")
}

func pluginsDir(project bool) (string, error) {
	if project {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return projectPluginsDir(wd), nil
	}
	dir := globalPluginsDir()
	if dir == "" {
		return "", fmt.Errorf("cannot resolve home directory")
	}
	return dir, nil
}

func pluginNameFromURL(url string) string {
	u := url
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return strings.TrimSuffix(u, ".git")
}

func validatePluginName(name string) error {
	if !pluginNameRE.MatchString(name) {
		return fmt.Errorf("invalid plugin name %q: must be lowercase letters, digits, and hyphens", name)
	}
	return nil
}

// validatePluginDir rejects a directory that is not a valid plugin. It covers
// both bad manifests (LoadPlugin returns a problem) and missing manifests
// (LoadPlugin returns a zero Plugin with no problem — silently "not a plugin"),
// since the CLI must not install a dir that has no plugin.json.
func validatePluginDir(dir string) error {
	p, problem := claudeplugin.LoadPlugin(dir)
	if p.Name == "" {
		if problem != "" {
			return fmt.Errorf("%s", problem)
		}
		return fmt.Errorf("missing or invalid .claude-plugin/plugin.json")
	}
	return nil
}

// safeRemoveTarget returns pluginsDir/name as an absolute path, verifying it
// stays directly within pluginsDir (rejects ".." and any escape). The name is
// also shape-checked, so a symlink/clone target can't trick this into removing
// files outside the plugins directory.
func safeRemoveTarget(pluginsDir, name string) (string, error) {
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	target := filepath.Join(pluginsDir, name)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	absBase, err := filepath.Abs(pluginsDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("refuse to remove path outside plugins directory: %s", target)
	}
	return absTarget, nil
}

// removePlugin validates the target is within pluginsDir and removes it
// (a symlink is unlinked, not its target). Used by the --force paths and remove.
func removePlugin(pluginsDir, name string) error {
	target, err := safeRemoveTarget(pluginsDir, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(target)
}

// resolveSubdir joins subdir onto staging and verifies the result stays within
// staging, rejecting ../ escapes (mirrors the mcpServers path containment). The
// --subdir option must only point inside the cloned repo.
func resolveSubdir(staging, subdir string) (string, error) {
	rel := strings.TrimPrefix(subdir, "./")
	full := filepath.Join(staging, rel)
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	absBase, err := filepath.Abs(staging)
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(absBase, absFull)
	if err != nil || r == "." || strings.HasPrefix(r, "..") {
		return "", fmt.Errorf("--subdir escapes plugin repo: %s", subdir)
	}
	return full, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
