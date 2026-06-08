package logs

import (
	"log/slog"
	"os"
)

// Config defines optional logging configuration for Setup.
type Config struct {
	Level       slog.Level // minimum log level written to files (default: slog.LevelDebug)
	StderrLevel slog.Level // minimum log level written to stderr; 0 (Debug) means "same as Level"
	DebugFile   string     // non-empty = async write Debug to file; empty = Debug to stderr
	ErrorFile   string     // non-empty = async write Warn/Error to file
}

// stderrMin returns the effective minimum level for the stderr route.
func (c Config) stderrMin() slog.Level {
	if c.StderrLevel != 0 {
		return c.StderrLevel
	}
	return c.Level
}

// FromEnv builds Config from environment variables.
//
// Environment variables:
//   - DEEPAI_DEBUG_FILE: exact file path for debug output
//   - DEEPAI_DEBUG: any non-empty value falls back to $TMPDIR/deepai-debug.log
//
// DEEPAI_DEBUG_FILE takes precedence over DEEPAI_DEBUG.
func FromEnv() Config {
	var cfg Config
	cfg.Level = slog.LevelDebug
	if p := os.Getenv("DEEPAI_DEBUG_FILE"); p != "" {
		cfg.DebugFile = p
	} else if os.Getenv("DEEPAI_DEBUG") != "" {
		cfg.DebugFile = os.TempDir() + "/deepai-debug.log"
	}
	if p := os.Getenv("DEEPAI_ERROR_FILE"); p != "" {
		cfg.ErrorFile = p
	}
	return cfg
}
