package chat

import (
	"fmt"
	"io"
	"os"

	"charm.land/huh/v2"
	"github.com/millken/deepai/pkg/models"
)

// PickSession shows an interactive session picker and returns the selected session.
// Falls back to the latest session in non-TTY environments.
func PickSession(repo models.SessionRepository) (*models.Session, error) {
	metas, err := repo.ListRecent(50)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	if len(metas) == 0 {
		return nil, fmt.Errorf("no sessions found")
	}

	// Non-TTY: return latest.
	if !isTTY() {
		sess, err := repo.Latest()
		if err != nil {
			return nil, err
		}
		if sess == nil {
			return nil, fmt.Errorf("no sessions found")
		}
		fmt.Fprintf(os.Stderr, "  Auto-resuming latest session: %s\n", sess.ID)
		return sess, nil
	}

	// Build option labels.
	type sessionOption struct {
		id    string
		label string
	}
	opts := make([]sessionOption, len(metas))
	huhOpts := make([]huh.Option[string], len(metas))
	for i, m := range metas {
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		label := fmt.Sprintf("%-40s %3d msgs", Truncate(title, 40), m.MsgCount)
		opts[i] = sessionOption{id: m.ID, label: label}
		huhOpts[i] = huh.NewOption(label, m.ID)
	}

	var selected string
	if err := huh.NewSelect[string]().
		Title("Select a session to resume").
		Options(huhOpts...).
		Filtering(true).
		Value(&selected).
		Run(); err != nil {
		return nil, fmt.Errorf("session picker: %w", err)
	}

	if selected == "" {
		return nil, fmt.Errorf("no session selected")
	}

	return repo.Load(selected)
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func Truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// ShowDeletePicker shows an interactive picker for multiple matching sessions.
// Returns the selected session ID.
func ShowDeletePicker(metas []models.SessionMeta) (string, error) {
	if !isTTY() || len(metas) == 1 {
		return metas[0].ID, nil
	}

	opts := make([]huh.Option[string], len(metas))
	for i, m := range metas {
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		label := fmt.Sprintf("%-40s %3d msgs", Truncate(title, 40), m.MsgCount)
		opts[i] = huh.NewOption(label, m.ID)
	}

	var selected string
	if err := huh.NewSelect[string]().
		Title("Multiple matches found. Which session to delete?").
		Options(opts...).
		Value(&selected).
		Run(); err != nil {
		return "", fmt.Errorf("delete picker: %w", err)
	}
	return selected, nil
}

// IsTerminal checks if w is a terminal.
func IsTerminal(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return false
		}
		return fi.Mode()&os.ModeCharDevice != 0
	}
	return false
}

// ResolveMultiple handles multiple matches for delete operations.
func ResolveMultiple(repo models.SessionRepository, input string) (*models.Session, []models.SessionMeta, error) {
	candidates, err := repo.ResolveAll(input)
	if err != nil {
		return nil, nil, err
	}

	if len(candidates) == 1 {
		sess, err := repo.Load(candidates[0].ID)
		return sess, nil, err
	}

	// Multiple matches for delete: return candidates for interactive selection.
	return nil, candidates, nil
}
