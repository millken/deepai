package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/commands"
	"github.com/spf13/cobra"
)

func openBotSecretStores() (*botSessionStore, *bot.SQLiteSecretStore, func(), error) {
	if err := commands.EnsureHome(); err != nil {
		return nil, nil, nil, err
	}
	if err := os.MkdirAll(botHome(), 0o700); err != nil {
		return nil, nil, nil, err
	}
	dsn := botDB() + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, nil, err
	}
	sessStore := newBotSessionStore(db)
	if err := sessStore.Migrate(); err != nil {
		_ = db.Close()
		return nil, nil, nil, err
	}
	secStore, err := bot.NewSQLiteSecretStore(db)
	if err != nil {
		_ = db.Close()
		return nil, nil, nil, err
	}
	return sessStore, secStore, func() { _ = db.Close() }, nil
}

func newSecretCmd() *cobra.Command {
	var botID string
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage encrypted per-bot third-party API secrets (set / list / delete)",
	}
	cmd.PersistentFlags().StringVar(&botID, "bot", bot.DefaultBotID, "Target bot ID")

	setCmd := &cobra.Command{
		Use:   "set <NAME> <VALUE>",
		Short: "Seal and store a credential for a bot (referenced via '{{secret:NAME}}')",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			bots, secrets, cleanup, err := openBotSecretStores()
			if err != nil {
				return err
			}
			defer cleanup()
			target := strings.TrimSpace(botID)
			if target == "" {
				target = bot.DefaultBotID
			}
			if _, err := bots.GetBot(cmd.Context(), target); err != nil {
				return err
			}
			meta, err := secrets.SetSecret(cmd.Context(), target, args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("stored secret %s for bot %s (use '{{secret:%s}}' in connectors/http_request)\n", meta.Name, meta.BotID, meta.Name)
			return nil
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List stored secret names for a bot (values are never displayed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			bots, secrets, cleanup, err := openBotSecretStores()
			if err != nil {
				return err
			}
			defer cleanup()
			target := strings.TrimSpace(botID)
			if target == "" {
				target = bot.DefaultBotID
			}
			if _, err := bots.GetBot(cmd.Context(), target); err != nil {
				return err
			}
			list, err := secrets.ListSecrets(cmd.Context(), target)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Printf("no secrets configured for bot %s\n", target)
				return nil
			}
			for _, m := range list {
				fmt.Printf("%s\tplaceholder={{secret:%s}}\tupdated=%s\n", m.Name, m.Name, m.UpdatedAt.Local().Format(time.RFC3339))
			}
			return nil
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <NAME>",
		Short: "Delete a stored secret from a bot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bots, secrets, cleanup, err := openBotSecretStores()
			if err != nil {
				return err
			}
			defer cleanup()
			target := strings.TrimSpace(botID)
			if target == "" {
				target = bot.DefaultBotID
			}
			if _, err := bots.GetBot(cmd.Context(), target); err != nil {
				return err
			}
			if err := secrets.DeleteSecret(cmd.Context(), target, args[0]); err != nil {
				return err
			}
			fmt.Printf("deleted secret %s from bot %s\n", strings.TrimSpace(args[0]), target)
			return nil
		},
	}

	cmd.AddCommand(setCmd, listCmd, deleteCmd)
	return cmd
}
