package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/millken/deepai/pkg/secret"
	"github.com/spf13/cobra"
)

// sealFn is secret.Seal, indirected so tests can force a roundtrip failure.
var sealFn = secret.Seal

func addKey(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage sealed API keys",
		Long: `Store API keys in ~/.deepai/.env as ciphertext bound to this machine's
disk serial numbers, so a tool that reads the file gets nothing usable.

This is not protection against a local attacker: deepai runs as you, so a
deliberate extraction still succeeds. It stops accidental exposure -- an
agent reading the file, a config pasted into a chat, a .env committed to git.

There is deliberately no command to print a stored key.`,
	}
	cmd.AddCommand(newKeySetCmd(), newKeyListCmd(), newKeySealCmd(), newKeyCheckCmd())
	topLevel.AddCommand(cmd)
}

func newKeySetCmd() *cobra.Command {
	var envVar string
	c := &cobra.Command{
		Use:   "set [provider]",
		Short: "Enter an API key and store it sealed",
		Long: `Prompt for an API key and write it to ~/.deepai/.env in sealed form.

Give a provider name to use its standard variable, or --env-var to name the
variable directly (matching a models[].api_key_env entry in config.yaml).`,
		Example: "  deepai key set anthropic\n  deepai key set --env-var MY_CUSTOM_KEY",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(envVar)
			if name == "" {
				if len(args) == 0 {
					return fmt.Errorf("give a provider name or --env-var; known providers: %s",
						strings.Join(providerNames(), ", "))
				}
				info, ok := providerInfo[strings.ToLower(args[0])]
				if !ok {
					return fmt.Errorf("unknown provider %q; known providers: %s",
						args[0], strings.Join(providerNames(), ", "))
				}
				name = info.envVar
			}

			var apiKey string
			if err := huh.NewInput().
				Title(fmt.Sprintf("API key (%s)", name)).
				Value(&apiKey).
				EchoMode(huh.EchoModePassword).
				Run(); err != nil {
				return err
			}
			if strings.TrimSpace(apiKey) == "" {
				return fmt.Errorf("no key entered")
			}

			sealed, err := sealFn(apiKey)
			if err != nil {
				return fmt.Errorf("seal API key: %w", err)
			}
			// Verify before writing: a sealed value that cannot be revealed
			// on this very host would silently lock the key away.
			if back, err := secret.Reveal(sealed); err != nil || back != apiKey {
				return fmt.Errorf("sealed key failed its own roundtrip check; nothing was written")
			}
			if err := saveEnvValue(EnvFile(), name, sealed); err != nil {
				return fmt.Errorf("save .env: %w", err)
			}

			fmt.Printf("  Sealed %s into %s\n", name, EnvFile())
			if w := sealWarning(); w != "" {
				fmt.Println(w)
			}
			return nil
		},
	}
	c.Flags().StringVar(&envVar, "env-var", "", "environment variable to store the key under")
	return c
}

func newKeyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "Show which API keys are stored and whether they are sealed",
		Example: "  deepai key list",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := EnvFile()
			names := make([]string, 0, len(providerInfo))
			for n := range apiKeyVarNames() {
				names = append(names, n)
			}
			sort.Strings(names)

			fmt.Printf("  %s\n\n", path)
			for _, n := range names {
				v := loadEnvValue(path, n)
				switch {
				case v == "":
					fmt.Printf("  %-22s absent\n", n)
				case secret.IsSealed(v):
					h, err := secret.Inspect(v)
					if err != nil {
						fmt.Printf("  %-22s sealed     unreadable: %v\n", n, err)
						continue
					}
					status := "ok"
					if _, err := secret.Reveal(v); err != nil {
						status = "CANNOT DECRYPT"
					}
					fmt.Printf("  %-22s sealed     %s, %d wrap(s), %s\n", n, h.Mode, h.Wraps, status)
				default:
					fmt.Printf("  %-22s plaintext  run `deepai key seal`\n", n)
				}
			}
			return nil
		},
	}
}

func newKeySealCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "seal",
		Short: "Encrypt any plaintext API keys already in .env",
		Long: `Rewrite ~/.deepai/.env with every plaintext API key sealed in place.

Each value is verified by sealing and revealing it before anything is
written, and the file is replaced atomically, so no plaintext copy is ever
left on disk.`,
		Example: "  deepai key seal",
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := sealEnvFile(EnvFile())
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Println("  No plaintext API keys found; nothing to do.")
				return nil
			}
			fmt.Printf("  Sealed %d key(s) in %s\n", n, EnvFile())
			if w := sealWarning(); w != "" {
				fmt.Println(w)
			}
			return nil
		},
	}
}

func newKeyCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "check",
		Short:   "Show this machine's binding sources and each key's status",
		Example: "  deepai key check",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := secret.Fingerprint()
			fmt.Printf("  Binding: %v\n", info.Mode)
			for _, s := range info.Sources {
				note := "unused (a stronger tier is available)"
				if s.Used {
					note = "in use"
				}
				fmt.Printf("    %-12s %s  %s\n", s.Tier, s.Digest, note)
			}
			if w := sealWarning(); w != "" {
				fmt.Println(w)
			}
			fmt.Println()
			return newKeyListCmd().RunE(cmd, args)
		},
	}
}

// apiKeyVarNames returns the environment variables that hold API keys and
// may therefore be sealed. Base URLs and other settings are excluded --
// sealing them would break config that is not secret.
func apiKeyVarNames() map[string]bool {
	out := make(map[string]bool, len(providerInfo))
	for _, info := range providerInfo {
		out[info.envVar] = true
	}
	return out
}

// envEntry is one line of a .env file. Key is empty for blank lines and
// comments, whose Line is preserved verbatim.
type envEntry struct {
	Key   string
	Value string
	Line  string
}

func parseEnvFile(content string) []envEntry {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	out := make([]envEntry, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, envEntry{Line: line})
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			out = append(out, envEntry{Line: line})
			continue
		}
		out = append(out, envEntry{Key: strings.TrimSpace(k), Value: v, Line: line})
	}
	return out
}

// sealEnvFile seals every plaintext API key in path and returns how many it
// sealed. Every value is sealed and revealed before anything is written, so
// a broken fingerprint layer leaves the file untouched rather than
// destroying the keys. The rewrite is atomic and leaves no plaintext copy --
// notably no .env.bak, which would be a plaintext duplicate that is not
// even covered by .gitignore.
func sealEnvFile(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	sealable := apiKeyVarNames()
	entries := parseEnvFile(string(content))
	sealed := 0
	for i, e := range entries {
		if e.Key == "" || !sealable[e.Key] {
			continue
		}
		v := strings.TrimSpace(e.Value)
		if v == "" || secret.IsSealed(v) {
			continue
		}
		out, err := sealFn(v)
		if err != nil {
			return 0, fmt.Errorf("seal %s: %w", e.Key, err)
		}
		back, err := secret.Reveal(out)
		if err != nil {
			return 0, fmt.Errorf("seal %s: sealed value failed its roundtrip check (%w); %s was not modified", e.Key, err, path)
		}
		if back != v {
			return 0, fmt.Errorf("seal %s: sealed value did not survive its roundtrip check; %s was not modified", e.Key, path)
		}
		entries[i].Line = e.Key + "=" + out
		sealed++
	}
	if sealed == 0 {
		return 0, nil
	}

	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Line)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := writeEnvAtomic(path, []byte(body)); err != nil {
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	return sealed, nil
}
