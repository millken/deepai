package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresEventStoreConfig configures the PostgreSQL-backed event store.
type PostgresEventStoreConfig struct {
	DSN           string        // PostgreSQL connection string
	BatchTimeout  time.Duration // auto-flush interval (default 2s)
	BatchSize     int           // auto-flush threshold (default 100)
	RequestsTable string        // default "proxy_requests"
	EventsTable   string        // default "proxy_events"
	Logger        *log.Logger   // optional logger for flush errors
}

// PostgresEventStore implements EventStore backed by PostgreSQL.
//
// Two tables:
//   - proxy_requests: one row per request, summary fields + req_body + resp_body
//   - proxy_events:   all raw events as JSONB, for detailed timeline queries
//
// Events are batched in memory and flushed periodically or when the batch is full.
type PostgresEventStore struct {
	cfg   PostgresEventStoreConfig
	pool  *pgxpool.Pool
	mu    sync.Mutex
	batch []LogEvent
	done  chan struct{}
	once  sync.Once
}

// NewPostgresEventStore connects to PostgreSQL, creates tables, and starts the flush loop.
func NewPostgresEventStore(cfg PostgresEventStoreConfig) (*PostgresEventStore, error) {
	if cfg.BatchTimeout <= 0 {
		cfg.BatchTimeout = 2 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.RequestsTable == "" {
		cfg.RequestsTable = "proxy_requests"
	}
	if cfg.EventsTable == "" {
		cfg.EventsTable = "proxy_events"
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres store: parse dsn: %w", err)
	}
	poolCfg.MinConns = 2
	poolCfg.MaxConns = 10

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres store: connect: %w", err)
	}

	s := &PostgresEventStore{
		cfg:   cfg,
		pool:  pool,
		batch: make([]LogEvent, 0, cfg.BatchSize),
		done:  make(chan struct{}),
	}

	if err := s.migrate(context.Background()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres store: migrate: %w", err)
	}

	go s.flushLoop()
	return s, nil
}

func (s *PostgresEventStore) migrate(ctx context.Context) error {
	queries := []string{
		// Main table: one row per request, includes bodies for direct querying.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id             TEXT    PRIMARY KEY,
			method         TEXT    NOT NULL DEFAULT '',
			path           TEXT    NOT NULL DEFAULT '',
			api_format     TEXT    NOT NULL DEFAULT '',
			model          TEXT    NOT NULL DEFAULT '',
			stream         BOOLEAN NOT NULL DEFAULT FALSE,
			status_code    INT     NOT NULL DEFAULT 0,
			duration       TEXT    NOT NULL DEFAULT '',
			input_tokens   INT     NOT NULL DEFAULT 0,
			output_tokens  INT     NOT NULL DEFAULT 0,
			error_msg      TEXT    NOT NULL DEFAULT '',
			client_id      TEXT    NOT NULL DEFAULT '',
			req_body       TEXT    NOT NULL DEFAULT '',
			resp_body      TEXT    NOT NULL DEFAULT '',
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`, s.cfg.RequestsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_created_at ON %s (created_at DESC)`,
			s.cfg.RequestsTable, s.cfg.RequestsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_model ON %s (model)`,
			s.cfg.RequestsTable, s.cfg.RequestsTable),

		// Events table: all raw events as JSONB for timeline queries.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id         BIGSERIAL PRIMARY KEY,
			request_id TEXT    NOT NULL,
			payload    JSONB   NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`, s.cfg.EventsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_request_id ON %s (request_id)`,
			s.cfg.EventsTable, s.cfg.EventsTable),
	}
	for _, q := range queries {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// Append buffers events for batch writing.
func (s *PostgresEventStore) Append(ctx context.Context, events ...LogEvent) error {
	s.mu.Lock()
	s.batch = append(s.batch, events...)
	if len(s.batch) >= s.cfg.BatchSize {
		batch := s.batch
		s.batch = make([]LogEvent, 0, s.cfg.BatchSize)
		s.mu.Unlock()
		return s.flush(ctx, batch)
	}
	s.mu.Unlock()
	return nil
}

func (s *PostgresEventStore) flush(ctx context.Context, events []LogEvent) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	eventSQL := fmt.Sprintf(
		`INSERT INTO %s (request_id, payload) VALUES ($1, $2)`, s.cfg.EventsTable)

	reqInsertSQL := fmt.Sprintf(
		`INSERT INTO %s (id, method, path, api_format, model, stream, client_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO NOTHING`, s.cfg.RequestsTable)

	reqDoneSQL := fmt.Sprintf(
		`UPDATE %s SET status_code = $2, duration = $3, error_msg = $4,
		 input_tokens = GREATEST(input_tokens, $5), output_tokens = GREATEST(output_tokens, $6)
		 WHERE id = $1`, s.cfg.RequestsTable)

	reqUsageSQL := fmt.Sprintf(
		`UPDATE %s SET input_tokens = GREATEST(input_tokens, $2), output_tokens = GREATEST(output_tokens, $3)
		 WHERE id = $1`, s.cfg.RequestsTable)

	reqBodySQL := fmt.Sprintf(
		`UPDATE %s SET req_body = $2 WHERE id = $1`, s.cfg.RequestsTable)

	respBodySQL := fmt.Sprintf(
		`UPDATE %s SET resp_body = $2 WHERE id = $1`, s.cfg.RequestsTable)

	for _, evt := range events {
		// Strip body from events table to avoid double storage;
		// body is stored separately in the requests table.
		storeEvt := evt
		if storeEvt.Type == EventReqBody || storeEvt.Type == EventRespBody {
			storeEvt.Body = nil
			storeEvt.BodySize = 0
		}
		payload, err := json.Marshal(storeEvt)
		if err != nil {
			continue
		}
		if _, err := tx.Exec(ctx, eventSQL, evt.ID, payload); err != nil {
			return fmt.Errorf("postgres store: insert event %s/%s: %w", evt.ID, evt.Type, err)
		}

		// Update requests table — errors here cause transaction rollback
		// to keep both tables consistent.
		switch evt.Type {
		case EventStart:
			if _, err := tx.Exec(ctx, reqInsertSQL,
				evt.ID, evt.Method, evt.Path, evt.APIFormat,
				evt.Model, evt.Streaming, evt.ClientID, evt.Timestamp); err != nil {
				return fmt.Errorf("postgres store: insert request %s: %w", evt.ID, err)
			}
		case EventDone:
			if _, err := tx.Exec(ctx, reqDoneSQL,
				evt.ID, evt.StatusCode, evt.Duration, evt.Error,
				evt.InputTokens, evt.OutputTokens); err != nil {
				return fmt.Errorf("postgres store: update done %s: %w", evt.ID, err)
			}
		case EventUsage:
			if _, err := tx.Exec(ctx, reqUsageSQL,
				evt.ID, evt.InputTokens, evt.OutputTokens); err != nil {
				return fmt.Errorf("postgres store: update usage %s: %w", evt.ID, err)
			}
		case EventReqBody:
			if len(evt.Body) > 0 {
				if _, err := tx.Exec(ctx, reqBodySQL, evt.ID, string(evt.Body)); err != nil {
					return fmt.Errorf("postgres store: update req_body %s: %w", evt.ID, err)
				}
			}
		case EventRespBody:
			if len(evt.Body) > 0 {
				if _, err := tx.Exec(ctx, respBodySQL, evt.ID, string(evt.Body)); err != nil {
					return fmt.Errorf("postgres store: update resp_body %s: %w", evt.ID, err)
				}
			}
		}
	}

	return tx.Commit(ctx)
}

func (s *PostgresEventStore) flushLoop() {
	ticker := time.NewTicker(s.cfg.BatchTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			batch := s.batch
			s.batch = make([]LogEvent, 0, s.cfg.BatchSize)
			s.mu.Unlock()
			if len(batch) > 0 {
				if err := s.flush(context.Background(), batch); err != nil {
					s.logf("postgres flush error: %v (%d events dropped)", err, len(batch))
				}
			}
		case <-s.done:
			return
		}
	}
}

// ListRequests returns paginated request summaries, newest first.
func (s *PostgresEventStore) ListRequests(ctx context.Context, offset, limit int) ([]RequestSummary, error) {
	sql := fmt.Sprintf(
		`SELECT id, method, path, api_format, model, stream, status_code, duration,
		        input_tokens, output_tokens, error_msg, client_id, created_at
		 FROM %s ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		s.cfg.RequestsTable,
	)

	rows, err := s.pool.Query(ctx, sql, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []RequestSummary
	for rows.Next() {
		var sum RequestSummary
		var createdAt time.Time
		if err := rows.Scan(
			&sum.ID, &sum.Method, &sum.Path, &sum.APIFormat, &sum.Model, &sum.Streaming,
			&sum.StatusCode, &sum.Duration, &sum.InputTokens, &sum.OutputTokens,
			&sum.Error, &sum.ClientID, &createdAt,
		); err != nil {
			return nil, err
		}
		sum.CreatedAt = createdAt
		results = append(results, sum)
	}
	return results, rows.Err()
}

// GetTimeline returns all events for a specific request.
// Loads body from the requests table for req_body/resp_body events.
func (s *PostgresEventStore) GetTimeline(ctx context.Context, id string) ([]LogEvent, error) {
	sql := fmt.Sprintf(
		`SELECT payload FROM %s WHERE request_id = $1 ORDER BY id ASC`,
		s.cfg.EventsTable,
	)

	rows, err := s.pool.Query(ctx, sql, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []LogEvent
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var evt LogEvent
		if json.Unmarshal(payload, &evt) != nil {
			continue
		}
		events = append(events, evt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Enrich body events from the requests table.
	if len(events) > 0 {
		var reqBody, respBody string
		bodySQL := fmt.Sprintf(
			`SELECT req_body, resp_body FROM %s WHERE id = $1`, s.cfg.RequestsTable)
		if err := s.pool.QueryRow(ctx, bodySQL, id).Scan(&reqBody, &respBody); err != nil {
			// Body enrichment is best-effort; log but don't fail.
			s.logf("postgres: load bodies for %s: %v", id, err)
		}

		for i := range events {
			switch events[i].Type {
			case EventReqBody:
				if reqBody != "" {
					events[i].Body = RawBody(reqBody)
				}
			case EventRespBody:
				if respBody != "" {
					events[i].Body = RawBody(respBody)
				}
			}
		}
	}

	if len(events) == 0 {
		return nil, ErrNotFound
	}
	return events, nil
}

// RequestCount returns the total number of tracked requests.
func (s *PostgresEventStore) RequestCount() int {
	sql := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, s.cfg.RequestsTable)
	var count int
	if err := s.pool.QueryRow(context.Background(), sql).Scan(&count); err != nil {
		return 0
	}
	return count
}

// Close flushes remaining events and releases the connection pool.
func (s *PostgresEventStore) Close() error {
	var flushErr error
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		batch := s.batch
		s.batch = nil
		s.mu.Unlock()
		if len(batch) > 0 {
			flushErr = s.flush(context.Background(), batch)
			if flushErr != nil {
				s.logf("postgres close flush error: %v (%d events dropped)", flushErr, len(batch))
			}
		}
	})
	s.pool.Close()
	return flushErr
}

func (s *PostgresEventStore) logf(format string, args ...any) {
	if s.cfg.Logger != nil {
		s.cfg.Logger.Printf(format, args...)
	}
}

var _ EventStore = (*PostgresEventStore)(nil)
