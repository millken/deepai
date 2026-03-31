package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/millken/deepai/pkg/models"
)

func TestMerge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	current := Document{
		SessionID: "session-1",
		User: UserMemory{
			WorkContext: "Working on deerflow-go",
		},
		Facts: []Fact{
			{ID: "pref-editor", Content: "Prefers vim", Category: "preference", Confidence: 0.7, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		},
	}

	update := Update{
		User: UserMemory{
			TopOfMind: "Ship the memory service",
		},
		Facts: []Fact{
			{ID: "pref-editor", Content: "Prefers neovim", Category: "preference", Confidence: 0.9},
			{ID: "project-main", Content: "Building deerflow-go memory service", Category: "project", Confidence: 0.8},
		},
	}

	got := Merge(current, update, "session-1", now)
	if got.User.WorkContext != "Working on deerflow-go" {
		t.Fatalf("work context = %q", got.User.WorkContext)
	}
	if got.User.TopOfMind != "Ship the memory service" {
		t.Fatalf("top of mind = %q", got.User.TopOfMind)
	}
	if len(got.Facts) != 2 {
		t.Fatalf("facts len = %d", len(got.Facts))
	}
	if got.Facts[0].ID != "pref-editor" || got.Facts[0].Content != "Prefers neovim" {
		t.Fatalf("merged fact = %#v", got.Facts[0])
	}
}

func TestServiceUpdateAndInject(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{}
	extractor := &stubExtractor{
		update: Update{
			User: UserMemory{
				WorkContext: "Maintains deerflow-go",
				TopOfMind:   "Memory reliability",
			},
			History: HistoryMemory{
				RecentMonths: "Rebuilding the agent runtime in Go",
			},
			Facts: []Fact{
				{ID: "project", Content: "Owns deerflow-go", Category: "project", Confidence: 0.95},
			},
			Source: "session-42",
		},
	}

	service := NewService(store, extractor)
	msgs := []models.Message{{
		ID:        "m1",
		SessionID: "session-42",
		Role:      models.RoleHuman,
		Content:   "I'm rebuilding deerflow-go and memory reliability matters most.",
		CreatedAt: time.Now().UTC(),
	}}

	if err := service.Update(context.Background(), "session-42", msgs); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	doc, err := store.Load(context.Background(), "session-42")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if doc.User.WorkContext != "Maintains deerflow-go" {
		t.Fatalf("work context = %q", doc.User.WorkContext)
	}

	injected := service.Inject(context.Background(), "session-42")
	if !strings.Contains(injected, "## User Memory") || !strings.Contains(injected, "Owns deerflow-go") {
		t.Fatalf("Inject() = %q", injected)
	}
}

func TestScheduleUpdateGracefulDegradation(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{}
	extractor := &stubExtractor{err: errors.New("llm exploded")}
	buf := &bytes.Buffer{}

	service := NewService(store, extractor).
		WithLogger(log.New(buf, "", 0)).
		WithUpdateTimeout(200 * time.Millisecond)

	service.ScheduleUpdate("session-err", []models.Message{{
		ID:        "m1",
		SessionID: "session-err",
		Role:      models.RoleHuman,
		Content:   "hello",
		CreatedAt: time.Now().UTC(),
	}})

	deadline := time.Now().Add(time.Second)
	for {
		if strings.Contains(buf.String(), "async update failed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected async failure log, got %q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := store.Load(context.Background(), "session-err"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load() err = %v, want ErrNotFound", err)
	}
}

func TestServiceUpdateFiltersUploadOnlyTurn(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{}
	extractor := &capturingExtractor{
		update: Update{
			User: UserMemory{TopOfMind: "Prefers concise answers"},
		},
	}
	service := NewService(store, extractor)

	const uploadBlock = "<uploaded_files>\nThe following files were uploaded in this message:\n\n- secret.txt (0.0 KB)\n  Path: /mnt/user-data/uploads/secret.txt\n</uploaded_files>"
	msgs := []models.Message{
		{
			ID:        "u1",
			SessionID: "session-upload",
			Role:      models.RoleHuman,
			Content:   uploadBlock,
			CreatedAt: time.Now().UTC(),
		},
		{
			ID:        "a1",
			SessionID: "session-upload",
			Role:      models.RoleAI,
			Content:   "I have read the file.",
			CreatedAt: time.Now().UTC(),
		},
	}

	if err := service.Update(context.Background(), "session-upload", msgs); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if extractor.called {
		t.Fatal("extractor should not be called for upload-only turns")
	}
	if _, err := store.Load(context.Background(), "session-upload"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load() err = %v, want ErrNotFound", err)
	}
}

func TestServiceUpdateStripsUploadBlockBeforeExtractor(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{}
	extractor := &capturingExtractor{
		update: Update{
			User: UserMemory{TopOfMind: "Need a summary"},
		},
	}
	service := NewService(store, extractor)

	const uploadBlock = "<uploaded_files>\nThe following files were uploaded in this message:\n\n- report.pdf (0.0 KB)\n  Path: /mnt/user-data/uploads/report.pdf\n</uploaded_files>"
	msgs := []models.Message{
		{
			ID:        "u1",
			SessionID: "session-mixed",
			Role:      models.RoleHuman,
			Content:   uploadBlock + "\n\nWhat does the report say?",
			CreatedAt: time.Now().UTC(),
		},
		{
			ID:        "a1",
			SessionID: "session-mixed",
			Role:      models.RoleAI,
			Content:   "It summarizes revenue growth.",
			CreatedAt: time.Now().UTC(),
		},
	}

	if err := service.Update(context.Background(), "session-mixed", msgs); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if !extractor.called {
		t.Fatal("extractor should be called")
	}
	if len(extractor.messages) != 2 {
		t.Fatalf("extractor messages len = %d", len(extractor.messages))
	}
	if strings.Contains(extractor.messages[0].Content, "<uploaded_files>") {
		t.Fatalf("human content still contains upload block: %q", extractor.messages[0].Content)
	}
	if strings.Contains(extractor.messages[0].Content, "/mnt/user-data/uploads/") {
		t.Fatalf("human content still contains upload path: %q", extractor.messages[0].Content)
	}
	if !strings.Contains(extractor.messages[0].Content, "What does the report say?") {
		t.Fatalf("human content missing real question: %q", extractor.messages[0].Content)
	}
}

func TestServiceUpdateStripsUploadMentionsFromMemory(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{}
	extractor := &capturingExtractor{
		update: Update{
			User: UserMemory{
				TopOfMind: "User is interested in AI. User uploaded a test file for verification. User prefers concise answers.",
			},
			Facts: []Fact{
				{ID: "upload", Content: "User uploaded a file titled secret.txt", Category: "behavior"},
				{ID: "pref", Content: "User prefers dark mode", Category: "preference"},
			},
		},
	}
	service := NewService(store, extractor)

	msgs := []models.Message{{
		ID:        "m1",
		SessionID: "session-clean",
		Role:      models.RoleHuman,
		Content:   "Please remember my preferences.",
		CreatedAt: time.Now().UTC(),
	}}

	if err := service.Update(context.Background(), "session-clean", msgs); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	doc, err := store.Load(context.Background(), "session-clean")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if strings.Contains(doc.User.TopOfMind, "uploaded a test file") {
		t.Fatalf("top of mind still contains upload mention: %q", doc.User.TopOfMind)
	}
	if !strings.Contains(doc.User.TopOfMind, "User is interested in AI") {
		t.Fatalf("top of mind lost legitimate context: %q", doc.User.TopOfMind)
	}
	if !strings.Contains(doc.User.TopOfMind, "User prefers concise answers") {
		t.Fatalf("top of mind lost legitimate preference: %q", doc.User.TopOfMind)
	}
	if len(doc.Facts) != 1 || doc.Facts[0].Content != "User prefers dark mode" {
		t.Fatalf("facts = %#v", doc.Facts)
	}
}

func TestPostgresStoreSaveLoadUsesTransaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newFakeMemoryDB()
	store := newPostgresStore(db)

	doc := Document{
		SessionID: "session-pg",
		User: UserMemory{
			WorkContext: "Go rewrite",
		},
		History: HistoryMemory{
			EarlierContext: "Original Python implementation",
		},
		Facts: []Fact{
			{ID: "language", Content: "Uses Go", Category: "project", Confidence: 0.99},
		},
		UpdatedAt: time.Date(2026, 3, 28, 13, 0, 0, 0, time.UTC),
	}

	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	if err := store.Save(ctx, doc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if db.beginCount != 1 || db.commitCount != 1 || db.rollbackCount != 0 {
		t.Fatalf("tx counts = begin:%d commit:%d rollback:%d", db.beginCount, db.commitCount, db.rollbackCount)
	}

	got, err := store.Load(ctx, doc.SessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.User.WorkContext != doc.User.WorkContext {
		t.Fatalf("loaded user memory = %#v", got.User)
	}
	if len(got.Facts) != 1 || got.Facts[0].ID != "language" {
		t.Fatalf("loaded facts = %#v", got.Facts)
	}
}

type stubExtractor struct {
	update Update
	err    error
}

func (s *stubExtractor) ExtractUpdate(_ context.Context, _ Document, _ []models.Message) (Update, error) {
	return s.update, s.err
}

type capturingExtractor struct {
	update   Update
	err      error
	called   bool
	messages []models.Message
}

func (c *capturingExtractor) ExtractUpdate(_ context.Context, _ Document, messages []models.Message) (Update, error) {
	c.called = true
	c.messages = cloneMessages(messages)
	return c.update, c.err
}

type fakeStorage struct {
	mu   sync.Mutex
	docs map[string]Document
}

func (f *fakeStorage) AutoMigrate(context.Context) error { return nil }

func (f *fakeStorage) Load(_ context.Context, sessionID string) (Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.docs == nil {
		f.docs = make(map[string]Document)
	}
	doc, ok := f.docs[sessionID]
	if !ok {
		return Document{}, ErrNotFound
	}
	return doc, nil
}

func (f *fakeStorage) Save(_ context.Context, doc Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.docs == nil {
		f.docs = make(map[string]Document)
	}
	f.docs[doc.SessionID] = doc
	return nil
}

type fakeMemoryDB struct {
	memories      map[string]Document
	beginCount    int
	commitCount   int
	rollbackCount int
}

func newFakeMemoryDB() *fakeMemoryDB {
	return &fakeMemoryDB{memories: make(map[string]Document)}
}

func (f *fakeMemoryDB) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	sql = normalizeSQL(sql)
	switch {
	case strings.Contains(sql, "create table if not exists memories"):
		return pgconn.NewCommandTag("MIGRATE"), nil
	default:
		return pgconn.CommandTag{}, errors.New("unexpected exec without transaction")
	}
}

func (f *fakeMemoryDB) Query(_ context.Context, sql string, args ...any) (rows, error) {
	sql = normalizeSQL(sql)
	if strings.Contains(sql, "from memory_facts") {
		sessionID := args[0].(string)
		doc, ok := f.memories[sessionID]
		if !ok {
			return &fakeRows{}, nil
		}
		data := make([][]any, 0, len(doc.Facts))
		for _, fact := range doc.Facts {
			data = append(data, []any{fact.ID, fact.Content, fact.Category, fact.Confidence, fact.CreatedAt, fact.UpdatedAt})
		}
		return &fakeRows{data: data}, nil
	}
	return nil, errors.New("unexpected query")
}

func (f *fakeMemoryDB) QueryRow(_ context.Context, sql string, args ...any) rowScanner {
	sql = normalizeSQL(sql)
	if strings.Contains(sql, "from memories") {
		sessionID := args[0].(string)
		doc, ok := f.memories[sessionID]
		if !ok {
			return fakeRow{err: pgx.ErrNoRows}
		}
		userJSON, _ := json.Marshal(doc.User)
		historyJSON, _ := json.Marshal(doc.History)
		return fakeRow{values: []any{doc.SessionID, userJSON, historyJSON, doc.Source, doc.UpdatedAt}}
	}
	return fakeRow{err: errors.New("unexpected query row")}
}

func (f *fakeMemoryDB) Begin(_ context.Context) (tx, error) {
	f.beginCount++
	return &fakeMemoryTx{db: f}, nil
}

type fakeMemoryTx struct {
	db *fakeMemoryDB
}

func (f *fakeMemoryTx) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	sql = normalizeSQL(sql)
	switch {
	case strings.Contains(sql, "insert into memories"):
		sessionID := arguments[0].(string)
		var doc Document
		doc.SessionID = sessionID
		_ = json.Unmarshal(arguments[1].([]byte), &doc.User)
		_ = json.Unmarshal(arguments[2].([]byte), &doc.History)
		doc.Source = arguments[3].(string)
		doc.UpdatedAt = arguments[4].(time.Time)
		doc.Facts = f.db.memories[sessionID].Facts
		f.db.memories[sessionID] = doc
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "delete from memory_facts"):
		sessionID := arguments[0].(string)
		doc := f.db.memories[sessionID]
		doc.Facts = nil
		f.db.memories[sessionID] = doc
		return pgconn.NewCommandTag("DELETE 1"), nil
	case strings.Contains(sql, "insert into memory_facts"):
		sessionID := arguments[0].(string)
		doc := f.db.memories[sessionID]
		doc.Facts = append(doc.Facts, Fact{
			ID:         arguments[1].(string),
			Content:    arguments[2].(string),
			Category:   arguments[3].(string),
			Confidence: arguments[4].(float64),
			CreatedAt:  arguments[5].(time.Time),
			UpdatedAt:  arguments[6].(time.Time),
		})
		f.db.memories[sessionID] = doc
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	default:
		return pgconn.CommandTag{}, errors.New("unexpected tx exec")
	}
}

func (f *fakeMemoryTx) Query(_ context.Context, _ string, _ ...any) (rows, error) {
	return nil, errors.New("unexpected tx query")
}

func (f *fakeMemoryTx) QueryRow(_ context.Context, _ string, _ ...any) rowScanner {
	return fakeRow{err: errors.New("unexpected tx query row")}
}

func (f *fakeMemoryTx) Commit(_ context.Context) error {
	f.db.commitCount++
	return nil
}

func (f *fakeMemoryTx) Rollback(_ context.Context) error {
	f.db.rollbackCount++
	return nil
}

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch v := dest[i].(type) {
		case *string:
			*v = r.values[i].(string)
		case *[]byte:
			*v = append((*v)[:0], r.values[i].([]byte)...)
		case *time.Time:
			*v = r.values[i].(time.Time)
		default:
			return errors.New("unsupported scan destination")
		}
	}
	return nil
}

type fakeRows struct {
	data [][]any
	idx  int
}

func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return nil }

func (r *fakeRows) Next() bool {
	return r.idx < len(r.data)
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.idx]
	r.idx++
	for i := range dest {
		switch v := dest[i].(type) {
		case *string:
			*v = row[i].(string)
		case *float64:
			*v = row[i].(float64)
		case *time.Time:
			*v = row[i].(time.Time)
		default:
			return errors.New("unsupported rows destination")
		}
	}
	return nil
}

func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(strings.ToLower(sql)), " ")
}
