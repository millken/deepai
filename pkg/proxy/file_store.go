package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const defaultMaxInlineBodySize = 4096 // 4 KB

// FileEventStore implements EventStore by appending JSON lines to a file.
// Large bodies are stored in a separate bodies/ directory.
// Only summaries are kept in memory; timeline queries read from disk.
type FileEventStore struct {
	mu            sync.RWMutex
	file          *os.File
	w             *bufio.Writer
	bodiesDir     string
	maxInlineBody int
	summaries     []RequestSummary
	summaryByID   map[string]int
	bodySeq       map[string]int // "id/type" → sequence counter for unique body filenames
}

// FileEventStoreConfig configures the file-based event store.
type FileEventStoreConfig struct {
	Path              string // JSONL file path
	MaxInlineBodySize int    // bodies larger than this go to separate files (default 4KB)
}

// NewFileEventStore opens or creates an event log at the given path.
func NewFileEventStore(cfg FileEventStoreConfig) (*FileEventStore, error) {
	if cfg.MaxInlineBodySize <= 0 {
		cfg.MaxInlineBodySize = defaultMaxInlineBodySize
	}

	f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	bodiesDir := filepath.Join(filepath.Dir(cfg.Path), "bodies")
	if err := os.MkdirAll(bodiesDir, 0o755); err != nil {
		f.Close()
		return nil, fmt.Errorf("create bodies dir: %w", err)
	}

	s := &FileEventStore{
		file:          f,
		w:             bufio.NewWriter(f),
		bodiesDir:     bodiesDir,
		maxInlineBody: cfg.MaxInlineBodySize,
		summaries:     make([]RequestSummary, 0),
		summaryByID:   make(map[string]int),
		bodySeq:       make(map[string]int),
	}

	if err := s.rebuildIndex(); err != nil {
		f.Close()
		return nil, err
	}

	return s, nil
}

func (s *FileEventStore) rebuildIndex() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	if _, err := s.file.Seek(0, 0); err != nil {
		return err
	}

	dec := json.NewDecoder(s.file)
	for dec.More() {
		var evt LogEvent
		if err := dec.Decode(&evt); err != nil {
			break
		}
		updateSummaryFromEvent(&s.summaries, s.summaryByID, evt)
	}
	return nil
}

func (s *FileEventStore) Append(_ context.Context, events ...LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, evt := range events {
		// Handle large body storage.
		if evt.Type == EventReqBody || evt.Type == EventRespBody {
			evt = s.maybeWriteBodyFile(evt)
		}

		data, err := json.Marshal(evt)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if _, err := s.w.Write(data); err != nil {
			return err
		}

		updateSummaryFromEvent(&s.summaries, s.summaryByID, evt)
	}

	return s.w.Flush()
}

// maybeWriteBodyFile writes body to a separate file if it exceeds maxInlineBody.
func (s *FileEventStore) maybeWriteBodyFile(evt LogEvent) LogEvent {
	if len(evt.Body) <= s.maxInlineBody {
		evt.BodySize = len(evt.Body)
		return evt
	}

	suffix := ".req.json"
	if evt.Type == EventRespBody {
		suffix = ".resp.json"
	}
	// Include a per-ID sequence number so that multiple body events of the
	// same type for the same request don't overwrite each other.
	key := evt.ID + "/" + string(evt.Type)
	s.bodySeq[key]++
	filename := fmt.Sprintf("%s.%d%s", evt.ID, s.bodySeq[key], suffix)
	fpath := filepath.Join(s.bodiesDir, filename)

	if err := os.WriteFile(fpath, evt.Body, 0o644); err == nil {
		evt.BodyFile = filepath.Join("bodies", filename)
		evt.BodySize = len(evt.Body)
		evt.Body = nil
	}
	// If write fails, keep body inline as fallback.
	return evt
}

func (s *FileEventStore) ListRequests(_ context.Context, offset, limit int) ([]RequestSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := len(s.summaries)
	if offset >= n {
		return nil, nil
	}
	end := offset + limit
	if end > n {
		end = n
	}
	out := make([]RequestSummary, end-offset)
	copy(out, s.summaries[offset:end])
	return out, nil
}

// scanFileEvents scans the JSONL file, calling fn for each event.
// Stops when fn returns false or EOF.
// Caller must hold at least RLock. Data is guaranteed flushed because Append
// always flushes before returning.
func (s *FileEventStore) scanFileEvents(fn func(LogEvent) bool) error {
	if _, err := s.file.Seek(0, 0); err != nil {
		return err
	}
	dec := json.NewDecoder(s.file)
	for dec.More() {
		var evt LogEvent
		if err := dec.Decode(&evt); err != nil {
			break
		}
		if !fn(evt) {
			break
		}
	}
	return nil
}

func (s *FileEventStore) GetTimeline(_ context.Context, id string) ([]LogEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.summaryByID[id]; !ok {
		return nil, ErrNotFound
	}

	var result []LogEvent
	if err := s.scanFileEvents(func(evt LogEvent) bool {
		if evt.ID == id {
			result = append(result, evt)
		}
		return true
	}); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, ErrNotFound
	}
	return result, nil
}

func (s *FileEventStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		s.w.Flush()
	}
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// RequestCount returns the total number of tracked requests (for testing).
func (s *FileEventStore) RequestCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.summaries)
}
