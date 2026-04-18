package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/millken/deepai/pkg/chat"
	"github.com/millken/deepai/pkg/models"
	"github.com/spf13/cobra"
)

func addSession(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage chat sessions",
	}

	cmd.AddCommand(
		cmdSessionList(),
		cmdSessionShow(),
		cmdSessionRename(),
		cmdSessionExport(),
		cmdSessionDelete(),
		cmdSessionPrune(),
		cmdSessionStats(),
	)

	topLevel.AddCommand(cmd)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdSessionList() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, cleanup, err := openSessionRepo()
			if err != nil {
				return err
			}
			defer cleanup()

			metas, err := repo.ListRecent(limit)
			if err != nil {
				return err
			}
			if len(metas) == 0 {
				fmt.Fprintln(os.Stderr, "  No sessions found.")
				return nil
			}

			// Print table header.
			fmt.Fprintf(os.Stderr, "  %-25s %-40s %5s %s\n", "ID", "TITLE", "MSGS", "CREATED")
			fmt.Fprintln(os.Stderr, "  "+dashes(25)+" "+dashes(40)+" "+dashes(5)+" "+dashes(19))
			for _, m := range metas {
				title := m.Title
				if title == "" {
					title = "(untitled)"
				}
				title = chat.Truncate(title, 40)
				created := m.CreatedAt.Format("2006-01-02 15:04")
				fmt.Fprintf(os.Stderr, "  %-25s %-40s %5d %s\n", m.ID, title, m.MsgCount, created)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "Number of sessions to show")
	return cmd
}

// ---------------------------------------------------------------------------
// show
// ---------------------------------------------------------------------------

func cmdSessionShow() *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "show <ID|TITLE>",
		Short: "Show session details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, cleanup, err := openSessionRepo()
			if err != nil {
				return err
			}
			defer cleanup()

			sess, err := repo.Resolve(args[0])
			if err != nil {
				return err
			}

			msgs, err := repo.LoadMessages(sess.ID)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "ID:         %s\n", sess.ID)
			fmt.Fprintf(os.Stderr, "Title:      %s\n", sess.Title)
			fmt.Fprintf(os.Stderr, "Created:    %s\n", sess.CreatedAt.Format("2006-01-02 15:04:05"))
			fmt.Fprintf(os.Stderr, "Messages:   %d\n", len(msgs))

			displayMsgs := msgs
			if !full && len(displayMsgs) > 5 {
				displayMsgs = displayMsgs[len(displayMsgs)-5:]
			}
			fmt.Fprintln(os.Stderr, "\n--- Messages ---")
			for _, m := range displayMsgs {
				tag := string(m.Role)
				content := m.Content
				if len(content) > 200 {
					content = content[:200] + "..."
				}
				fmt.Fprintf(os.Stderr, "  [%s] %s\n", tag, content)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "Show all messages")
	return cmd
}

// ---------------------------------------------------------------------------
// rename
// ---------------------------------------------------------------------------

func cmdSessionRename() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <ID|TITLE> <NEW_TITLE>",
		Short: "Rename a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, cleanup, err := openSessionRepo()
			if err != nil {
				return err
			}
			defer cleanup()

			sess, err := repo.Resolve(args[0])
			if err != nil {
				return err
			}

			if err := repo.Rename(sess.ID, args[1]); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "  Session %q renamed to %q\n", sess.ID, args[1])
			return nil
		},
	}
	return cmd
}

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

func cmdSessionExport() *cobra.Command {
	var sessionID string
	cmd := &cobra.Command{
		Use:   "export <OUTPUT>",
		Short: "Export sessions to JSONL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, cleanup, err := openSessionRepo()
			if err != nil {
				return err
			}
			defer cleanup()

			var exports []models.SessionExport
			if sessionID != "" {
				exp, err := repo.ExportSession(sessionID)
				if err != nil {
					return err
				}
				exports = []models.SessionExport{*exp}
			} else {
				exports, err = repo.ExportAll()
				if err != nil {
					return err
				}
			}

			var w *os.File
			if args[0] == "-" {
				w = os.Stdout
			} else {
				w, err = os.Create(args[0])
				if err != nil {
					return err
				}
				defer w.Close()
			}

			enc := json.NewEncoder(w)
			total := 0
			for _, exp := range exports {
				for _, m := range exp.Messages {
					if err := enc.Encode(m); err != nil {
						return fmt.Errorf("encode message: %w", err)
					}
					total++
				}
			}

			if args[0] != "-" {
				fmt.Fprintf(os.Stderr, "  Exported %d messages.\n", total)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Export a specific session")
	return cmd
}

// ---------------------------------------------------------------------------
// delete
// ---------------------------------------------------------------------------

func cmdSessionDelete() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete [<ID|TITLE>]",
		Short: "Delete a session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, cleanup, err := openSessionRepo()
			if err != nil {
				return err
			}
			defer cleanup()

			// No argument: show full session picker.
			if len(args) == 0 {
				metas, err := repo.ListRecent(50)
				if err != nil {
					return err
				}
				if len(metas) == 0 {
					fmt.Fprintln(os.Stderr, "  No sessions found.")
					return nil
				}
				selected, err := chat.ShowDeletePicker(metas)
				if err != nil {
					return err
				}
				return confirmAndDelete(repo, selected, yes)
			}

			input := args[0]

			// Check for multiple matches.
			sess, candidates, err := chat.ResolveMultiple(repo, input)
			if err != nil {
				return err
			}
			if sess != nil {
				// Single match.
				return confirmAndDelete(repo, sess.ID, yes)
			}
			// Multiple matches: show picker.
			if len(candidates) > 1 {
				selected, err := chat.ShowDeletePicker(candidates)
				if err != nil {
					return err
				}
				return confirmAndDelete(repo, selected, yes)
			}
			return fmt.Errorf("no session matching %q", input)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	return cmd
}

func confirmAndDelete(repo models.SessionRepository, id string, skipConfirm bool) error {
	if !skipConfirm {
		sess, err := repo.Load(id)
		if err != nil {
			return err
		}
		msgCount := "?"
		if metas, err := repo.ListRecent(1000); err == nil {
			for _, m := range metas {
				if m.ID == id {
					msgCount = strconv.Itoa(m.MsgCount)
					break
				}
			}
		}
		fmt.Fprintf(os.Stderr, "  Delete session %q (%s messages)? [y/N] ", sess.Title, msgCount)
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "y" && confirm != "Y" {
			fmt.Fprintln(os.Stderr, "  Cancelled.")
			return nil
		}
	}

	if err := repo.Delete(id); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "  Deleted.")
	return nil
}

// ---------------------------------------------------------------------------
// prune
// ---------------------------------------------------------------------------

func cmdSessionPrune() *cobra.Command {
	var olderThan int
	var yes bool
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old completed sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, cleanup, err := openSessionRepo()
			if err != nil {
				return err
			}
			defer cleanup()

			// Always dry-run first to get the count.
			count, err := repo.Prune(olderThan, true)
			if err != nil {
				return err
			}

			if dryRun {
				fmt.Fprintf(os.Stderr, "  Would prune %d sessions older than %d days.\n", count, olderThan)
				return nil
			}

			if count == 0 {
				fmt.Fprintln(os.Stderr, "  Nothing to prune.")
				return nil
			}

			if !yes {
				fmt.Fprintf(os.Stderr, "  Delete %d sessions older than %d days? [y/N] ", count, olderThan)
				var confirm string
				fmt.Scanln(&confirm)
				if confirm != "y" && confirm != "Y" {
					fmt.Fprintln(os.Stderr, "  Cancelled.")
					return nil
				}
			}

			count, err = repo.Prune(olderThan, false)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "  Pruned %d sessions.\n", count)
			return nil
		},
	}
	cmd.Flags().IntVar(&olderThan, "older-than", 90, "Days threshold")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show count without deleting")
	return cmd
}

// ---------------------------------------------------------------------------
// stats
// ---------------------------------------------------------------------------

func cmdSessionStats() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show session statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, cleanup, err := openSessionRepo()
			if err != nil {
				return err
			}
			defer cleanup()

			stats, err := repo.Stats()
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "  Sessions:   %d\n", stats.SessionCount)
			fmt.Fprintf(os.Stderr, "  Messages:   %d\n", stats.MessageCount)
			if !stats.OldestAt.IsZero() {
				fmt.Fprintf(os.Stderr, "  Oldest:     %s\n", stats.OldestAt.Format("2006-01-02"))
			}
			if !stats.LatestAt.IsZero() {
				fmt.Fprintf(os.Stderr, "  Latest:     %s\n", stats.LatestAt.Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
	return cmd
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// openSessionRepo creates a standalone session store with its own DB connection.
// This is intentional: session subcommands (list/show/delete/etc.) are short-lived
// and don't need shared DB with memory service. Migrate() is idempotent.
func openSessionRepo() (models.SessionRepository, func(), error) {
	dbPath := DBFile()
	store, err := chat.NewSQLiteSessionStore(dbPath)
	if err != nil {
		return nil, nil, err
	}
	return store, func() { store.Close() }, nil
}

func dashes(n int) string {
	return strings.Repeat("-", n)
}
