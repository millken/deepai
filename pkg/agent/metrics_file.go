package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// FileMetricsSink writes Phase 0 records as JSONL (one JSON object per line) to a
// dedicated file, independent of slog level routing. Each line is self-describing
// via a "type" field ("turn" or "tool"), so a report is a one-liner:
//
//	jq -c 'select(.type=="turn") | {turn, input_tokens, tf: .context.tool_fraction}' metrics.jsonl
//
// Writes are open-append-close per record: low Phase 0 volume makes the syscall
// cost irrelevant, and O_APPEND keeps concurrent writers (subagents sharing one
// path) from interleaving. Write failures are non-fatal — metrics never disrupt a
// run; the first failure is surfaced once via slog.
type FileMetricsSink struct {
	path string
	mu   sync.Mutex
	once sync.Once
}

// NewFileMetricsSink returns a sink writing JSONL to path, creating its parent
// directory if needed.
func NewFileMetricsSink(path string) *FileMetricsSink {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return &FileMetricsSink{path: path}
}

func (s *FileMetricsSink) RecordTurn(m TurnMetrics) {
	s.write(struct {
		Type string `json:"type"`
		TurnMetrics
	}{Type: "turn", TurnMetrics: m})
}

func (s *FileMetricsSink) RecordToolResult(m ToolResultMetric) {
	s.write(struct {
		Type string `json:"type"`
		ToolResultMetric
	}{Type: "tool", ToolResultMetric: m})
}

func (s *FileMetricsSink) write(rec any) {
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.warn(err)
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		s.warn(err)
	}
}

func (s *FileMetricsSink) warn(err error) {
	s.once.Do(func() {
		slog.Warn("token metrics file write failed", "path", s.path, "err", err)
	})
}
