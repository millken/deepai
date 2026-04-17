package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FileStore persists one memory document per session as JSON on local disk.
type FileStore struct {
	root string
}

func NewFileStore(root string) (*FileStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("file store root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve file store root: %w", err)
	}
	return &FileStore{root: absRoot}, nil
}

func (s *FileStore) AutoMigrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || strings.TrimSpace(s.root) == "" {
		return errors.New("file store is not initialized")
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("create file store root: %w", err)
	}
	return nil
}

func (s *FileStore) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *FileStore) Load(ctx context.Context, sessionID string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	path, err := s.documentPath(sessionID)
	if err != nil {
		return Document{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Document{}, ErrNotFound
		}
		return Document{}, fmt.Errorf("read memory %q: %w", sessionID, err)
	}

	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return Document{}, fmt.Errorf("decode memory %q: %w", sessionID, err)
	}
	if err := prepareDocument(&doc); err != nil {
		return Document{}, fmt.Errorf("normalize memory %q: %w", sessionID, err)
	}
	return doc, nil
}

func (s *FileStore) Save(ctx context.Context, doc Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := prepareDocument(&doc); err != nil {
		return err
	}
	if err := s.AutoMigrate(ctx); err != nil {
		return err
	}

	path, err := s.documentPath(doc.SessionID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode memory %q: %w", doc.SessionID, err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write memory %q: %w", doc.SessionID, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("commit memory %q: %w", doc.SessionID, err)
	}
	return nil
}

func (s *FileStore) Delete(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.documentPath(sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("delete memory %q: %w", sessionID, err)
	}
	return nil
}

func (s *FileStore) documentPath(sessionID string) (string, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return "", errors.New("file store is not initialized")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", errors.New("memory session_id is required")
	}
	return filepath.Join(s.root, url.PathEscape(sessionID)+".json"), nil
}
