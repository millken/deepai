package plugin

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// DependencyResolver determines the order in which plugins should be loaded
// based on their dependencies.
type DependencyResolver struct {
	strict bool
}

// NewDependencyResolver creates a new dependency resolver.
func NewDependencyResolver() *DependencyResolver {
	return &DependencyResolver{
		strict: true,
	}
}

// SetStrict configures whether to fail on unmet dependencies.
func (r *DependencyResolver) SetStrict(strict bool) {
	r.strict = strict
}

// Resolve returns plugin IDs in dependency order (dependencies first).
func (r *DependencyResolver) Resolve(manifests map[string]*Manifest) ([]string, error) {
	// Build dependency graph
	graph := buildGraph(manifests)

	// Topological sort
	order, err := topologicalSort(graph)
	if err != nil {
		return nil, err
	}

	// Validate version constraints
	if r.strict {
		if err := r.validateVersions(manifests); err != nil {
			return nil, err
		}
	}

	return order, nil
}

// node represents a plugin in the dependency graph.
type node struct {
	id          string
	manifest    *Manifest
	dependsOn   []string
	dependents  []string
	visited     bool
	inProgress  bool
}

// buildGraph creates a dependency graph from manifests.
func buildGraph(manifests map[string]*Manifest) map[string]*node {
	graph := make(map[string]*node)

	// Create nodes
	for id, m := range manifests {
		graph[id] = &node{
			id:        id,
			manifest:  m,
			dependsOn: make([]string, 0),
			dependents: make([]string, 0),
		}
	}

	// Add edges
	for id, m := range manifests {
		n := graph[id]
		for _, dep := range m.Dependencies {
			depID := dep.ID
			if _, exists := graph[depID]; exists {
				n.dependsOn = append(n.dependsOn, depID)
				graph[depID].dependents = append(graph[depID].dependents, id)
			}
		}
	}

	return graph
}

// topologicalSort returns plugin IDs in load order using DFS.
// The result has dependencies before dependents (dependencies first).
func topologicalSort(graph map[string]*node) ([]string, error) {
	order := make([]string, 0, len(graph))

	for id, n := range graph {
		if n.visited {
			continue
		}

		path := []string{id}
		if err := dfs(graph, id, &order, &path); err != nil {
			return nil, err
		}
	}

	// No need to reverse - DFS adds dependencies before dependents
	// because we process all dependencies before adding the node

	return order, nil
}

// dfs performs depth-first search for topological sort.
func dfs(graph map[string]*node, id string, order *[]string, path *[]string) error {
	n, exists := graph[id]
	if !exists {
		return fmt.Errorf("plugin %s not found", id)
	}

	if n.inProgress {
		// Cycle detected
		return fmt.Errorf("circular dependency detected: %s -> %s", strings.Join(*path, " -> "), id)
	}

	if n.visited {
		return nil
	}

	n.inProgress = true

	for _, depID := range n.dependsOn {
		*path = append(*path, depID)
		if err := dfs(graph, depID, order, path); err != nil {
			return err
		}
		*path = (*path)[:len(*path)-1]
	}

	n.inProgress = false
	n.visited = true
	*order = append(*order, id)

	return nil
}

// validateVersions checks that dependency version constraints are satisfied.
func (r *DependencyResolver) validateVersions(manifests map[string]*Manifest) error {
	for id, m := range manifests {
		for _, dep := range m.Dependencies {
			depManifest, exists := manifests[dep.ID]
			if !exists {
				return fmt.Errorf("plugin %s depends on missing plugin %s", id, dep.ID)
			}

			if dep.Version == "" {
				continue
			}

			// Parse version constraint
			constraint, err := semver.NewConstraint(dep.Version)
			if err != nil {
				return fmt.Errorf("invalid version constraint %s for %s: %w", dep.Version, dep.ID, err)
			}

			// Parse actual version
			version, err := semver.NewVersion(depManifest.Version)
			if err != nil {
				// Skip validation for non-semver versions
				continue
			}

			// Check constraint
			if !constraint.Check(version) {
				return fmt.Errorf("plugin %s requires %s version %s, but %s is installed",
					id, dep.ID, dep.Version, depManifest.Version)
			}
		}
	}
	return nil
}

// GetLoadOrder returns the order in which plugins should be loaded.
// This is a convenience method that combines dependency resolution.
func (r *DependencyResolver) GetLoadOrder(manifests map[string]*Manifest) ([]string, error) {
	return r.Resolve(manifests)
}

// CheckDependencies returns missing or version-mismatched dependencies.
func (r *DependencyResolver) CheckDependencies(manifests map[string]*Manifest) []DependencyError {
	var errors []DependencyError

	for id, m := range manifests {
		for _, dep := range m.Dependencies {
			depManifest, exists := manifests[dep.ID]

			if !exists {
				errors = append(errors, DependencyError{
					PluginID:    id,
					Dependency:  dep.ID,
					Type:        "missing",
					Required:    dep.Version,
					Installed:   "",
				})
				continue
			}

			if dep.Version == "" {
				continue
			}

			constraint, err := semver.NewConstraint(dep.Version)
			if err != nil {
				continue
			}

			version, err := semver.NewVersion(depManifest.Version)
			if err != nil {
				continue
			}

			if !constraint.Check(version) {
				errors = append(errors, DependencyError{
					PluginID:    id,
					Dependency:  dep.ID,
					Type:        "version_mismatch",
					Required:    dep.Version,
					Installed:   depManifest.Version,
				})
			}
		}
	}

	return errors
}

// DependencyError represents a dependency problem.
type DependencyError struct {
	PluginID   string `json:"plugin_id"`
	Dependency string `json:"dependency"`
	Type       string `json:"type"` // missing, version_mismatch, circular
	Required   string `json:"required"`
	Installed  string `json:"installed,omitempty"`
}

// Error implements the error interface.
func (e DependencyError) Error() string {
	switch e.Type {
	case "missing":
		return fmt.Sprintf("plugin %s depends on missing plugin %s", e.PluginID, e.Dependency)
	case "version_mismatch":
		return fmt.Sprintf("plugin %s requires %s version %s, but %s is installed",
			e.PluginID, e.Dependency, e.Required, e.Installed)
	case "circular":
		return fmt.Sprintf("circular dependency involving %s and %s", e.PluginID, e.Dependency)
	default:
		return fmt.Sprintf("dependency error: %s -> %s", e.PluginID, e.Dependency)
	}
}
