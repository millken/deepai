package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const appName = ".deepai"

var (
	homeDir     string
	homeDirOnce sync.Once
)

// Home returns the deepai home directory (~/.deepai).
func Home() string {
	homeDirOnce.Do(func() {
		h, err := os.UserHomeDir()
		if err != nil {
			return
		}
		homeDir = filepath.Join(h, appName)
	})
	return homeDir
}

// ConfigFile returns the path to config.yaml.
func ConfigFile() string {
	return filepath.Join(Home(), "config.yaml")
}

// EnvFile returns the path to .env.
func EnvFile() string {
	return filepath.Join(Home(), ".env")
}

// SessionsDir returns the path to the sessions directory.
func SessionsDir() string {
	return filepath.Join(Home(), "sessions")
}

// SandboxDir returns the path to the sandbox session root (~/.deepai/sandbox).
// Sandbox session directories are created here — outside the user's working
// directory — so cleanup on exit can never touch project files.
func SandboxDir() string {
	return filepath.Join(Home(), "sandbox")
}

// DBFile returns the path to the unified SQLite database.
func DBFile() string {
	return filepath.Join(Home(), "deepai.db")
}

// InputHistoryFile returns the path to the CLI input history file.
func InputHistoryFile() string {
	return filepath.Join(Home(), "input_history")
}

// LogsDir returns the path to the logs directory.
func LogsDir() string {
	return filepath.Join(Home(), "logs")
}

// MemoriesDir returns the path to the memories directory.
func MemoriesDir() string {
	return filepath.Join(Home(), "memories")
}

// OffloadDir returns the path to the offload directory (~/.deepai/offload).
// Large tool results (>24KB) are written here so the full content is
// recoverable while only a summary stays in the context window.
func OffloadDir() string {
	return filepath.Join(Home(), "offload")
}

// GlobalInstructions returns the path to the global DEEPAI.md.
func GlobalInstructions() string {
	return filepath.Join(Home(), "DEEPAI.md")
}

// ProjectInstructions returns the path to the project-level DEEPAI.md.
func ProjectInstructions(workDir string) string {
	return filepath.Join(workDir, appName, "DEEPAI.md")
}

// EnsureHome creates ~/.deepai and all standard sub-directories.
func EnsureHome() error {
	dir := Home()
	if dir == "" {
		return fmt.Errorf("cannot determine home directory")
	}
	for _, sub := range []string{"sessions", "logs", "memories", "offload"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			return fmt.Errorf("create %s/%s: %w", dir, sub, err)
		}
	}
	return os.MkdirAll(dir, 0700)
}
