package memory

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

type Store interface {
	Storage
	Close()
}

func OpenStore(ctx context.Context, databaseURL string) (Store, error) {
	databaseURL = strings.TrimSpace(databaseURL)
	if databaseURL == "" {
		return nil, errors.New("database url is required")
	}
	if isSQLiteURL(databaseURL) {
		path, err := sqlitePathFromURL(databaseURL)
		if err != nil {
			return nil, err
		}
		return NewSQLiteStore(ctx, path)
	}
	return NewPostgresStore(ctx, databaseURL)
}

func isSQLiteURL(databaseURL string) bool {
	databaseURL = strings.TrimSpace(strings.ToLower(databaseURL))
	return strings.HasPrefix(databaseURL, "sqlite://") ||
		strings.HasPrefix(databaseURL, "sqlite:") ||
		strings.HasSuffix(databaseURL, ".db") ||
		strings.HasSuffix(databaseURL, ".sqlite") ||
		strings.HasSuffix(databaseURL, ".sqlite3")
}

func sqlitePathFromURL(databaseURL string) (string, error) {
	databaseURL = strings.TrimSpace(databaseURL)
	if databaseURL == "" {
		return "", errors.New("sqlite path is required")
	}
	if strings.HasPrefix(strings.ToLower(databaseURL), "sqlite://") {
		u, err := url.Parse(databaseURL)
		if err != nil {
			return "", fmt.Errorf("parse sqlite url: %w", err)
		}
		path := strings.TrimSpace(u.Path)
		if u.Host != "" && u.Host != "localhost" {
			path = "//" + u.Host + path
		}
		if path == "" {
			return "", errors.New("sqlite path is required")
		}
		return filepath.Clean(path), nil
	}
	if strings.HasPrefix(strings.ToLower(databaseURL), "sqlite:") {
		path := strings.TrimPrefix(databaseURL, "sqlite:")
		path = strings.TrimPrefix(path, "//")
		path = strings.TrimSpace(path)
		if path == "" {
			return "", errors.New("sqlite path is required")
		}
		return filepath.Clean(path), nil
	}
	return filepath.Clean(databaseURL), nil
}
