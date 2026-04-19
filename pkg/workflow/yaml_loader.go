package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ResolveWorkflow resolves a workflow by name.
// Priority: .deepai/workflows/<name>.yaml > BuiltinWorkflows.
func ResolveWorkflow(name, workDir string) (*Workflow, error) {
	if err := validateSafeName(name); err != nil {
		return nil, err
	}
	yamlWf, err := loadWorkflowYAML(name, workDir)
	if err != nil {
		return nil, err
	}
	if yamlWf != nil {
		return yamlWf, nil
	}
	if builtin, ok := BuiltinWorkflows[name]; ok {
		return &builtin, nil
	}
	return nil, fmt.Errorf("workflow %q not found", name)
}

// ListWorkflows returns names of all available workflows (builtin + YAML).
func ListWorkflows(workDir string) []string {
	names := make(map[string]bool)
	for name := range BuiltinWorkflows {
		names[name] = true
	}
	if workDir != "" {
		dir := filepath.Join(workDir, ".deepai", "workflows")
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				n := entry.Name()
				ext := ""
				if strings.HasSuffix(n, ".yaml") {
					ext = ".yaml"
				} else if strings.HasSuffix(n, ".yml") {
					ext = ".yml"
				} else {
					continue
				}
				names[strings.TrimSuffix(n, ext)] = true
			}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func loadWorkflowYAML(name, workDir string) (*Workflow, error) {
	if workDir == "" {
		return nil, nil
	}
	dir := filepath.Join(workDir, ".deepai", "workflows")
	var data []byte
	var path string
	var err error
	for _, ext := range []string{".yaml", ".yml"} {
		path = filepath.Join(dir, name+ext)
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workflow yaml %s: %w", path, err)
	}

	var wf Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow yaml %s: %w", path, err)
	}
	if err := wf.Validate(); err != nil {
		return nil, err
	}
	return &wf, nil
}

func validateSafeName(name string) error {
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid name %q: must not contain \"..\" or path separators", name)
	}
	return nil
}
