package bot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// TokenRole controls authorization for scoped API tokens (docs/BOT_NEXT_DESIGN.md §6.1).
type TokenRole string

const (
	RoleAdmin    TokenRole = "admin"
	RoleUser     TokenRole = "user"
	RoleReadOnly TokenRole = "readonly"
)

var (
	ErrForbidden     = errors.New("forbidden")
	ErrTokenNotFound = errors.New("token not found")
	ErrInvalidToken  = errors.New("invalid token request")
)

// Principal is the authenticated caller attached to a request context.
type Principal struct {
	ID     string
	Name   string
	Role   TokenRole
	BotIDs []string // empty = all bots allowed
}

// AllowsBot reports whether p may access botID.
func (p Principal) AllowsBot(botID string) bool {
	if p.Role == RoleAdmin || len(p.BotIDs) == 0 {
		return true
	}
	return slices.Contains(p.BotIDs, effectiveBotID(botID))
}

// CanWrite reports whether p may perform session/schedule mutations.
func (p Principal) CanWrite() bool {
	return p.Role == RoleAdmin || p.Role == RoleUser
}

// IsAdmin reports whether p holds full administrative privileges.
func (p Principal) IsAdmin() bool {
	return p.Role == RoleAdmin
}

type principalCtxKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

func principalFromContext(ctx context.Context) Principal {
	if ctx != nil {
		if p, ok := ctx.Value(principalCtxKey{}).(Principal); ok && p.Role != "" {
			return p
		}
	}
	return Principal{ID: "master", Name: "master", Role: RoleAdmin}
}

// TokenRecord describes one issued scoped token (plain Token is returned ONLY
// once at creation time and is never persisted).
type TokenRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       TokenRole  `json:"role"`
	BotIDs     []string   `json:"bot_ids,omitempty"`
	Token      string     `json:"token,omitempty"` // one-time plaintext on create only
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// CreateTokenRequest is the body of POST /v1/tokens.
type CreateTokenRequest struct {
	Name      string     `json:"name"`
	Role      TokenRole  `json:"role"`
	BotIDs    []string   `json:"bot_ids,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	TTL       string     `json:"ttl,omitempty"` // optional duration string e.g. "720h"
}

// TokenStore persists scoped API tokens (bot_tokens in bot.db or in-memory).
type TokenStore interface {
	CreateToken(ctx context.Context, req CreateTokenRequest) (TokenRecord, error)
	LookupToken(ctx context.Context, tokenHash string) (TokenRecord, error)
	ListTokens(ctx context.Context) ([]TokenRecord, error)
	RevokeToken(ctx context.Context, id string) error
	TouchToken(ctx context.Context, id string, at time.Time) error
}

// HashToken returns the SHA-256 hex digest of a raw bearer token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func generateScopedToken() (id, raw, hash string, err error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	id = "tok_" + randomHex(6)
	raw = "dbt_" + hex.EncodeToString(b[:])
	return id, raw, HashToken(raw), nil
}

func validateCreateToken(req CreateTokenRequest, now time.Time) (TokenRecord, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 80 {
		return TokenRecord{}, fmt.Errorf("%w: name is required (max 80 chars)", ErrInvalidToken)
	}
	role := TokenRole(strings.ToLower(strings.TrimSpace(string(req.Role))))
	switch role {
	case RoleAdmin, RoleUser, RoleReadOnly:
	default:
		return TokenRecord{}, fmt.Errorf("%w: role must be admin, user, or readonly", ErrInvalidToken)
	}
	var exp *time.Time
	if req.ExpiresAt != nil {
		t := req.ExpiresAt.UTC()
		if !t.After(now) {
			return TokenRecord{}, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidToken)
		}
		exp = &t
	} else if ttl := strings.TrimSpace(req.TTL); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil || d <= 0 {
			return TokenRecord{}, fmt.Errorf("%w: invalid ttl %q", ErrInvalidToken, ttl)
		}
		t := now.Add(d)
		exp = &t
	}
	var bots []string
	seen := map[string]struct{}{}
	for _, b := range req.BotIDs {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		if _, dup := seen[b]; !dup {
			seen[b] = struct{}{}
			bots = append(bots, b)
		}
	}
	return TokenRecord{
		Name:      name,
		Role:      role,
		BotIDs:    bots,
		ExpiresAt: exp,
		CreatedAt: now,
	}, nil
}

// ─────────────────────── MemoryTokenStore ───────────────────────

type MemoryTokenStore struct {
	mu     sync.Mutex
	byID   map[string]TokenRecord
	byHash map[string]string // hash -> id
}

func NewMemoryTokenStore() *MemoryTokenStore {
	return &MemoryTokenStore{byID: map[string]TokenRecord{}, byHash: map[string]string{}}
}

func (s *MemoryTokenStore) CreateToken(_ context.Context, req CreateTokenRequest) (TokenRecord, error) {
	rec, err := validateCreateToken(req, time.Now().UTC())
	if err != nil {
		return TokenRecord{}, err
	}
	id, raw, hash, err := generateScopedToken()
	if err != nil {
		return TokenRecord{}, err
	}
	rec.ID = id
	s.mu.Lock()
	s.byID[id] = rec
	s.byHash[hash] = id
	s.mu.Unlock()
	out := rec
	out.Token = raw
	return out, nil
}

func (s *MemoryTokenStore) LookupToken(_ context.Context, tokenHash string) (TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byHash[tokenHash]
	if !ok {
		return TokenRecord{}, ErrTokenNotFound
	}
	rec := s.byID[id]
	rec.BotIDs = append([]string(nil), rec.BotIDs...)
	return rec, nil
}

func (s *MemoryTokenStore) ListTokens(_ context.Context) ([]TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TokenRecord, 0, len(s.byID))
	for _, rec := range s.byID {
		cp := rec
		cp.Token = ""
		cp.BotIDs = append([]string(nil), rec.BotIDs...)
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b TokenRecord) int {
		if a.CreatedAt.Equal(b.CreatedAt) {
			return strings.Compare(a.ID, b.ID)
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return -1
		}
		return 1
	})
	return out, nil
}

func (s *MemoryTokenStore) RevokeToken(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return ErrTokenNotFound
	}
	now := time.Now().UTC()
	rec.RevokedAt = &now
	s.byID[id] = rec
	return nil
}

func (s *MemoryTokenStore) TouchToken(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return ErrTokenNotFound
	}
	t := at.UTC()
	rec.LastUsedAt = &t
	s.byID[id] = rec
	return nil
}

// ─────────────────────── SQLiteTokenStore ───────────────────────

// SQLiteTokenStore persists bot_tokens in bot.db (docs/BOT_NEXT_DESIGN.md §6.1).
type SQLiteTokenStore struct {
	db *sql.DB
}

func NewSQLiteTokenStore(db *sql.DB) (*SQLiteTokenStore, error) {
	s := &SQLiteTokenStore{db: db}
	if err := s.Migrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteTokenStore) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS bot_tokens (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL,
			token_hash   TEXT NOT NULL UNIQUE,
			role         TEXT NOT NULL,
			bot_ids      TEXT NOT NULL DEFAULT '[]',
			expires_at   INTEGER,
			revoked_at   INTEGER,
			last_used_at INTEGER,
			created_at   INTEGER NOT NULL
		)`)
	return err
}

func (s *SQLiteTokenStore) CreateToken(ctx context.Context, req CreateTokenRequest) (TokenRecord, error) {
	rec, err := validateCreateToken(req, time.Now().UTC())
	if err != nil {
		return TokenRecord{}, err
	}
	id, raw, hash, err := generateScopedToken()
	if err != nil {
		return TokenRecord{}, err
	}
	rec.ID = id
	botsJSON, _ := json.Marshal(rec.BotIDs)
	if len(rec.BotIDs) == 0 {
		botsJSON = []byte("[]")
	}
	var expUnix any
	if rec.ExpiresAt != nil {
		expUnix = rec.ExpiresAt.UnixMilli()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO bot_tokens (id, name, token_hash, role, bot_ids, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Name, hash, string(rec.Role), string(botsJSON), expUnix, rec.CreatedAt.UnixMilli())
	if err != nil {
		return TokenRecord{}, err
	}
	rec.Token = raw
	return rec, nil
}

func (s *SQLiteTokenStore) LookupToken(ctx context.Context, tokenHash string) (TokenRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, role, bot_ids, expires_at, revoked_at, last_used_at, created_at
		FROM bot_tokens WHERE token_hash = ?`, tokenHash)
	return scanTokenRecord(row)
}

func (s *SQLiteTokenStore) ListTokens(ctx context.Context) ([]TokenRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, role, bot_ids, expires_at, revoked_at, last_used_at, created_at
		FROM bot_tokens ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRecord
	for rows.Next() {
		rec, err := scanTokenRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLiteTokenStore) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE bot_tokens SET revoked_at = ? WHERE id = ?`, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

func (s *SQLiteTokenStore) TouchToken(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE bot_tokens SET last_used_at = ? WHERE id = ?`, at.UTC().UnixMilli(), id)
	return err
}

func scanTokenRecord(scanner interface{ Scan(...any) error }) (TokenRecord, error) {
	var rec TokenRecord
	var role, botsJSON string
	var expMS, revMS, lastMS sql.NullInt64
	var createdMS int64
	if err := scanner.Scan(&rec.ID, &rec.Name, &role, &botsJSON, &expMS, &revMS, &lastMS, &createdMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TokenRecord{}, ErrTokenNotFound
		}
		return TokenRecord{}, err
	}
	rec.Role = TokenRole(role)
	_ = json.Unmarshal([]byte(botsJSON), &rec.BotIDs)
	rec.CreatedAt = time.UnixMilli(createdMS).UTC()
	if expMS.Valid {
		t := time.UnixMilli(expMS.Int64).UTC()
		rec.ExpiresAt = &t
	}
	if revMS.Valid {
		t := time.UnixMilli(revMS.Int64).UTC()
		rec.RevokedAt = &t
	}
	if lastMS.Valid {
		t := time.UnixMilli(lastMS.Int64).UTC()
		rec.LastUsedAt = &t
	}
	return rec, nil
}
