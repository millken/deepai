package bot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/secret"
)

const MaxSecretValueBytes = 16 << 10

var (
	ErrSecretNotFound = errors.New("secret not found")
	ErrInvalidSecret  = errors.New("invalid secret")

	secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// SecretMeta describes one per-bot credential stored in SecretStore.
// Plaintext values are never returned in API or list responses.
type SecretMeta struct {
	BotID     string    `json:"bot_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SecretStore manages machine-sealed credentials isolated per bot_id so
// third-party API keys can be referenced via `{{secret:NAME}}` placeholders
// without exposing plaintext secrets to the LLM or conversation transcript.
type SecretStore interface {
	SetSecret(ctx context.Context, botID, name, plaintext string) (SecretMeta, error)
	GetSecret(ctx context.Context, botID, name string) (string, error)
	ListSecrets(ctx context.Context, botID string) ([]SecretMeta, error)
	DeleteSecret(ctx context.Context, botID, name string) error
	DeleteAllSecretsForBot(ctx context.Context, botID string) error
}

func normalizeSecretRef(botID, name string) (string, string, error) {
	b := effectiveBotID(botID)
	n := strings.TrimSpace(name)
	if !secretNamePattern.MatchString(n) {
		return "", "", fmt.Errorf("%w: secret name %q must match ^[A-Za-z_][A-Za-z0-9_]{0,63}$", ErrInvalidSecret, name)
	}
	return b, n, nil
}

func validateSecretValue(plaintext string) error {
	if strings.TrimSpace(plaintext) == "" {
		return fmt.Errorf("%w: secret value cannot be empty", ErrInvalidSecret)
	}
	if len(plaintext) > MaxSecretValueBytes {
		return fmt.Errorf("%w: secret value exceeds %d bytes", ErrInvalidSecret, MaxSecretValueBytes)
	}
	return nil
}

// ─────────────────────── MemorySecretStore ───────────────────────

type memSecretEntry struct {
	meta  SecretMeta
	value string
}

// MemorySecretStore keeps per-bot secrets in memory for `--ephemeral` and tests.
type MemorySecretStore struct {
	mu   sync.RWMutex
	rows map[[2]string]memSecretEntry
}

func NewMemorySecretStore() *MemorySecretStore {
	return &MemorySecretStore{rows: map[[2]string]memSecretEntry{}}
}

func (s *MemorySecretStore) SetSecret(_ context.Context, botID, name, plaintext string) (SecretMeta, error) {
	b, n, err := normalizeSecretRef(botID, name)
	if err != nil {
		return SecretMeta{}, err
	}
	if err := validateSecretValue(plaintext); err != nil {
		return SecretMeta{}, err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{b, n}
	created := now
	if prev, ok := s.rows[key]; ok {
		created = prev.meta.CreatedAt
	}
	meta := SecretMeta{BotID: b, Name: n, CreatedAt: created, UpdatedAt: now}
	s.rows[key] = memSecretEntry{meta: meta, value: plaintext}
	return meta, nil
}

func (s *MemorySecretStore) GetSecret(_ context.Context, botID, name string) (string, error) {
	b, n, err := normalizeSecretRef(botID, name)
	if err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ent, ok := s.rows[[2]string{b, n}]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, n)
	}
	return ent.value, nil
}

func (s *MemorySecretStore) ListSecrets(_ context.Context, botID string) ([]SecretMeta, error) {
	b := effectiveBotID(botID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []SecretMeta
	for key, ent := range s.rows {
		if key[0] == b {
			out = append(out, ent.meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *MemorySecretStore) DeleteSecret(_ context.Context, botID, name string) error {
	b, n, err := normalizeSecretRef(botID, name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{b, n}
	if _, ok := s.rows[key]; !ok {
		return fmt.Errorf("%w: %s", ErrSecretNotFound, n)
	}
	delete(s.rows, key)
	return nil
}

func (s *MemorySecretStore) DeleteAllSecretsForBot(_ context.Context, botID string) error {
	b := effectiveBotID(botID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.rows {
		if key[0] == b {
			delete(s.rows, key)
		}
	}
	return nil
}

// ─────────────────────── SQLiteSecretStore ───────────────────────

// SQLiteSecretStore persists machine-sealed secrets in bot.db using pkg/secret.
type SQLiteSecretStore struct {
	db *sql.DB
}

func NewSQLiteSecretStore(db *sql.DB) (*SQLiteSecretStore, error) {
	if db == nil {
		return nil, errors.New("bot: sqlite db is required for secret store")
	}
	s := &SQLiteSecretStore{db: db}
	if err := s.Migrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteSecretStore) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS bot_secrets (
			bot_id     TEXT NOT NULL,
			name       TEXT NOT NULL,
			ciphertext TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (bot_id, name)
		)`)
	return err
}

func (s *SQLiteSecretStore) SetSecret(ctx context.Context, botID, name, plaintext string) (SecretMeta, error) {
	b, n, err := normalizeSecretRef(botID, name)
	if err != nil {
		return SecretMeta{}, err
	}
	if err := validateSecretValue(plaintext); err != nil {
		return SecretMeta{}, err
	}
	sealed, err := secret.Seal(plaintext)
	if err != nil {
		return SecretMeta{}, fmt.Errorf("seal secret: %w", err)
	}
	now := time.Now().UTC()
	nowMS := now.UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO bot_secrets (bot_id, name, ciphertext, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(bot_id, name) DO UPDATE SET
			ciphertext = excluded.ciphertext,
			updated_at = excluded.updated_at`,
		b, n, sealed, nowMS, nowMS)
	if err != nil {
		return SecretMeta{}, fmt.Errorf("store secret: %w", err)
	}
	var createdMS, updatedMS int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT created_at, updated_at FROM bot_secrets WHERE bot_id = ? AND name = ?`, b, n,
	).Scan(&createdMS, &updatedMS); err != nil {
		return SecretMeta{BotID: b, Name: n, CreatedAt: now, UpdatedAt: now}, nil
	}
	return SecretMeta{
		BotID:     b,
		Name:      n,
		CreatedAt: time.UnixMilli(createdMS).UTC(),
		UpdatedAt: time.UnixMilli(updatedMS).UTC(),
	}, nil
}

func (s *SQLiteSecretStore) GetSecret(ctx context.Context, botID, name string) (string, error) {
	b, n, err := normalizeSecretRef(botID, name)
	if err != nil {
		return "", err
	}
	var ct string
	if err := s.db.QueryRowContext(ctx,
		`SELECT ciphertext FROM bot_secrets WHERE bot_id = ? AND name = ?`, b, n,
	).Scan(&ct); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrSecretNotFound, n)
		}
		return "", fmt.Errorf("load secret: %w", err)
	}
	pt, err := secret.Reveal(ct)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %s: %w", n, err)
	}
	return pt, nil
}

func (s *SQLiteSecretStore) ListSecrets(ctx context.Context, botID string) ([]SecretMeta, error) {
	b := effectiveBotID(botID)
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at, updated_at FROM bot_secrets WHERE bot_id = ? ORDER BY name ASC`, b)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var name string
		var createdMS, updatedMS int64
		if err := rows.Scan(&name, &createdMS, &updatedMS); err != nil {
			return nil, err
		}
		out = append(out, SecretMeta{
			BotID:     b,
			Name:      name,
			CreatedAt: time.UnixMilli(createdMS).UTC(),
			UpdatedAt: time.UnixMilli(updatedMS).UTC(),
		})
	}
	return out, rows.Err()
}

func (s *SQLiteSecretStore) DeleteSecret(ctx context.Context, botID, name string) error {
	b, n, err := normalizeSecretRef(botID, name)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM bot_secrets WHERE bot_id = ? AND name = ?`, b, n)
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("%w: %s", ErrSecretNotFound, n)
	}
	return nil
}

func (s *SQLiteSecretStore) DeleteAllSecretsForBot(ctx context.Context, botID string) error {
	b := effectiveBotID(botID)
	_, err := s.db.ExecContext(ctx, `DELETE FROM bot_secrets WHERE bot_id = ?`, b)
	return err
}
