package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/millken/deepai/pkg/models"
)

func TestSessionCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newPostgresStore(newFakeDB())

	now := time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
	session := Session{
		ID:        "session-1",
		UserID:    "user-1",
		State:     SessionStateActive,
		Metadata:  map[string]string{"source": "test"},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	got, err := store.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if got.ID != session.ID || got.UserID != session.UserID || got.State != session.State {
		t.Fatalf("GetSession() = %#v", got)
	}

	session.State = SessionStateCompleted
	session.Metadata["source"] = "updated"
	session.UpdatedAt = now.Add(time.Minute)
	if err := store.UpdateSession(ctx, session); err != nil {
		t.Fatalf("UpdateSession() error = %v", err)
	}
	if err := store.UpdateSessionState(ctx, session.ID, SessionStateArchived); err != nil {
		t.Fatalf("UpdateSessionState() error = %v", err)
	}

	got, err = store.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSession() after update error = %v", err)
	}
	if got.State != SessionStateArchived {
		t.Fatalf("session state = %q, want %q", got.State, SessionStateArchived)
	}
	if got.Metadata["source"] != "updated" {
		t.Fatalf("session metadata = %#v", got.Metadata)
	}

	if err := store.DeleteSession(ctx, session.ID); err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}
	if _, err := store.GetSession(ctx, session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSession() after delete error = %v, want ErrNotFound", err)
	}
}

func TestMessageCRUD(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newPostgresStore(newFakeDB())

	now := time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC)
	session := Session{
		ID:        "session-2",
		UserID:    "user-2",
		State:     SessionStateActive,
		Metadata:  map[string]string{"kind": "message"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	msg1 := Message{
		ID:        "msg-1",
		SessionID: session.ID,
		Role:      RoleHuman,
		Content:   "hello",
		Metadata:  map[string]string{"step": "1"},
		CreatedAt: now,
	}
	msg2 := Message{
		ID:        "msg-2",
		SessionID: session.ID,
		Role:      RoleAI,
		Content:   "thinking",
		ToolCalls: []ToolCall{
			{
				ID:          "call-1",
				Name:        "bash",
				Arguments:   map[string]any{"cmd": "pwd"},
				Status:      models.CallStatusCompleted,
				RequestedAt: now,
				StartedAt:   now,
				CompletedAt: now.Add(time.Second),
			},
		},
		Metadata:  map[string]string{"step": "2"},
		CreatedAt: now.Add(time.Second),
	}
	toolResult := &ToolResult{
		CallID:      "call-1",
		ToolName:    "bash",
		Status:      models.CallStatusCompleted,
		Content:     "/tmp",
		CompletedAt: now.Add(2 * time.Second),
	}
	msg3 := Message{
		ID:         "msg-3",
		SessionID:  session.ID,
		Role:       RoleTool,
		Content:    "/tmp",
		ToolResult: toolResult,
		Metadata:   map[string]string{"step": "3"},
		CreatedAt:  now.Add(2 * time.Second),
	}

	if err := store.CreateMessage(ctx, msg1); err != nil {
		t.Fatalf("CreateMessage(msg1) error = %v", err)
	}
	if err := store.SaveMessage(ctx, msg2); err != nil {
		t.Fatalf("SaveMessage(msg2) error = %v", err)
	}
	if err := store.CreateMessage(ctx, msg3); err != nil {
		t.Fatalf("CreateMessage(msg3) error = %v", err)
	}

	gotMessage, err := store.GetMessage(ctx, msg2.ID)
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if gotMessage.ToolCalls[0].Name != "bash" {
		t.Fatalf("GetMessage() tool call = %#v", gotMessage.ToolCalls)
	}

	msg2.Content = "updated"
	msg2.Metadata["step"] = "updated"
	if err := store.UpdateMessage(ctx, msg2); err != nil {
		t.Fatalf("UpdateMessage() error = %v", err)
	}

	messages, err := store.ListMessages(ctx, session.ID)
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("ListMessages() len = %d, want 3", len(messages))
	}
	if messages[1].Content != "updated" {
		t.Fatalf("ListMessages()[1] = %#v", messages[1])
	}

	loaded, err := store.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if len(loaded.Messages) != 3 {
		t.Fatalf("GetSession().Messages len = %d, want 3", len(loaded.Messages))
	}

	replacement := Session{
		ID:        session.ID,
		UserID:    session.UserID,
		State:     SessionStateCompleted,
		Metadata:  map[string]string{"kind": "replacement"},
		CreatedAt: now,
		UpdatedAt: now.Add(3 * time.Second),
		Messages: []Message{
			{
				ID:        "msg-4",
				SessionID: session.ID,
				Role:      RoleSystem,
				Content:   "replacement message",
				Metadata:  map[string]string{"step": "4"},
				CreatedAt: now.Add(3 * time.Second),
			},
		},
	}
	if err := store.SaveSession(ctx, replacement); err != nil {
		t.Fatalf("SaveSession() error = %v", err)
	}

	loaded, err = store.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSession() after SaveSession error = %v", err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].ID != "msg-4" {
		t.Fatalf("GetSession() after SaveSession = %#v", loaded.Messages)
	}

	if err := store.DeleteMessage(ctx, "msg-4"); err != nil {
		t.Fatalf("DeleteMessage() error = %v", err)
	}
	if _, err := store.GetMessage(ctx, "msg-4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMessage() after delete error = %v, want ErrNotFound", err)
	}

	if err := store.DeleteMessages(ctx, session.ID); err != nil {
		t.Fatalf("DeleteMessages() error = %v", err)
	}
	messages, err = store.LoadSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("LoadSession() error = %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("LoadSession() len = %d, want 0", len(messages))
	}
}

type fakeDB struct {
	sessions map[string]Session
	messages map[string]Message
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		sessions: make(map[string]Session),
		messages: make(map[string]Message),
	}
}

func (f *fakeDB) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	sql = normalizeSQL(sql)

	switch {
	case strings.Contains(sql, "create table if not exists sessions"):
		return pgconn.NewCommandTag("MIGRATE"), nil
	case strings.Contains(sql, "insert into sessions") && strings.Contains(sql, "on conflict"):
		session, err := decodeSessionArgs(arguments)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		f.sessions[session.ID] = cloneSession(session)
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "insert into sessions"):
		session, err := decodeSessionArgs(arguments)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		if _, exists := f.sessions[session.ID]; exists {
			return pgconn.CommandTag{}, errors.New("duplicate session")
		}
		f.sessions[session.ID] = cloneSession(session)
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "update sessions set state"):
		id := arguments[0].(string)
		session, ok := f.sessions[id]
		if !ok {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		session.State = arguments[1].(SessionState)
		session.UpdatedAt = arguments[2].(time.Time)
		f.sessions[id] = cloneSession(session)
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "update sessions"):
		id := arguments[0].(string)
		session, ok := f.sessions[id]
		if !ok {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		session.UserID = arguments[1].(string)
		session.State = arguments[2].(SessionState)
		_ = json.Unmarshal(arguments[3].([]byte), &session.Metadata)
		session.UpdatedAt = arguments[4].(time.Time)
		f.sessions[id] = cloneSession(session)
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "delete from sessions"):
		id := arguments[0].(string)
		if _, ok := f.sessions[id]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(f.sessions, id)
		for msgID, msg := range f.messages {
			if msg.SessionID == id {
				delete(f.messages, msgID)
			}
		}
		return pgconn.NewCommandTag("DELETE 1"), nil
	case strings.Contains(sql, "insert into messages") && strings.Contains(sql, "on conflict"):
		msg, err := decodeMessageArgs(arguments)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		f.messages[msg.ID] = cloneMessage(msg)
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "insert into messages"):
		msg, err := decodeMessageArgs(arguments)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		if _, exists := f.messages[msg.ID]; exists {
			return pgconn.CommandTag{}, errors.New("duplicate message")
		}
		f.messages[msg.ID] = cloneMessage(msg)
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "update messages"):
		id := arguments[0].(string)
		if _, ok := f.messages[id]; !ok {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		msg, err := decodeMessageArgs(arguments)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		f.messages[id] = cloneMessage(msg)
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "delete from messages where session_id"):
		sessionID := arguments[0].(string)
		for msgID, msg := range f.messages {
			if msg.SessionID == sessionID {
				delete(f.messages, msgID)
			}
		}
		return pgconn.NewCommandTag("DELETE"), nil
	case strings.Contains(sql, "delete from messages where id"):
		id := arguments[0].(string)
		if _, ok := f.messages[id]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(f.messages, id)
		return pgconn.NewCommandTag("DELETE 1"), nil
	default:
		return pgconn.CommandTag{}, errors.New("unexpected exec sql: " + sql)
	}
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (rows, error) {
	sql = normalizeSQL(sql)
	if !strings.Contains(sql, "from messages") {
		return nil, errors.New("unexpected query sql: " + sql)
	}
	sessionID := args[0].(string)
	records := make([]Message, 0)
	for _, msg := range f.messages {
		if msg.SessionID == sessionID {
			records = append(records, cloneMessage(msg))
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})

	out := make([][]any, 0, len(records))
	for _, msg := range records {
		toolCalls, _ := json.Marshal(defaultToolCalls(msg.ToolCalls))
		metadata, _ := json.Marshal(defaultMap(msg.Metadata))
		var toolResult *string
		if msg.ToolResult != nil {
			raw, _ := json.Marshal(msg.ToolResult)
			value := string(raw)
			toolResult = &value
		}
		out = append(out, []any{msg.ID, msg.SessionID, msg.Role, msg.Content, toolCalls, toolResult, metadata, msg.CreatedAt})
	}
	return &fakeRows{rows: out}, nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) rowScanner {
	sql = normalizeSQL(sql)

	switch {
	case strings.Contains(sql, "from sessions"):
		id := args[0].(string)
		session, ok := f.sessions[id]
		if !ok {
			return fakeRow{err: pgx.ErrNoRows}
		}
		metadata, _ := json.Marshal(defaultMap(session.Metadata))
		return fakeRow{values: []any{session.ID, session.UserID, session.State, metadata, session.CreatedAt, session.UpdatedAt}}
	case strings.Contains(sql, "from messages"):
		id := args[0].(string)
		msg, ok := f.messages[id]
		if !ok {
			return fakeRow{err: pgx.ErrNoRows}
		}
		toolCalls, _ := json.Marshal(defaultToolCalls(msg.ToolCalls))
		metadata, _ := json.Marshal(defaultMap(msg.Metadata))
		var toolResult *string
		if msg.ToolResult != nil {
			raw, _ := json.Marshal(msg.ToolResult)
			value := string(raw)
			toolResult = &value
		}
		return fakeRow{values: []any{msg.ID, msg.SessionID, msg.Role, msg.Content, toolCalls, toolResult, metadata, msg.CreatedAt}}
	default:
		return fakeRow{err: errors.New("unexpected query row sql: " + sql)}
	}
}

func (f *fakeDB) Begin(_ context.Context) (tx, error) {
	return &fakeTx{
		parent:   f,
		sessions: cloneSessions(f.sessions),
		messages: cloneMessages(f.messages),
	}, nil
}

type fakeTx struct {
	parent    *fakeDB
	sessions  map[string]Session
	messages  map[string]Message
	committed bool
}

func (f *fakeTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return (&fakeDB{sessions: f.sessions, messages: f.messages}).Exec(ctx, sql, arguments...)
}

func (f *fakeTx) Query(ctx context.Context, sql string, args ...any) (rows, error) {
	return (&fakeDB{sessions: f.sessions, messages: f.messages}).Query(ctx, sql, args...)
}

func (f *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) rowScanner {
	return (&fakeDB{sessions: f.sessions, messages: f.messages}).QueryRow(ctx, sql, args...)
}

func (f *fakeTx) Commit(_ context.Context) error {
	f.parent.sessions = cloneSessions(f.sessions)
	f.parent.messages = cloneMessages(f.messages)
	f.committed = true
	return nil
}

func (f *fakeTx) Rollback(_ context.Context) error {
	return nil
}

type fakeRows struct {
	rows [][]any
	idx  int
}

func (f *fakeRows) Close() {}

func (f *fakeRows) Err() error { return nil }

func (f *fakeRows) Next() bool {
	if f.idx >= len(f.rows) {
		return false
	}
	f.idx++
	return true
}

func (f *fakeRows) Scan(dest ...any) error {
	if f.idx == 0 || f.idx > len(f.rows) {
		return errors.New("scan called without current row")
	}
	return assignScannedValues(dest, f.rows[f.idx-1])
}

type fakeRow struct {
	values []any
	err    error
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	return assignScannedValues(dest, f.values)
}

func assignScannedValues(dest []any, values []any) error {
	if len(dest) != len(values) {
		return errors.New("scan destination/value length mismatch")
	}
	for i := range dest {
		if err := assignScannedValue(dest[i], values[i]); err != nil {
			return err
		}
	}
	return nil
}

func assignScannedValue(dest any, value any) error {
	dst := reflect.ValueOf(dest)
	if dst.Kind() != reflect.Pointer || dst.IsNil() {
		return errors.New("scan destination must be a non-nil pointer")
	}

	if value == nil {
		dst.Elem().Set(reflect.Zero(dst.Elem().Type()))
		return nil
	}

	src := reflect.ValueOf(value)
	target := dst.Elem()

	if src.Type().AssignableTo(target.Type()) {
		target.Set(src)
		return nil
	}
	if src.Type().ConvertibleTo(target.Type()) {
		target.Set(src.Convert(target.Type()))
		return nil
	}
	if target.Kind() == reflect.Pointer {
		elem := reflect.New(target.Type().Elem())
		if src.Type().AssignableTo(target.Type().Elem()) {
			elem.Elem().Set(src)
			target.Set(elem)
			return nil
		}
		if src.Type().ConvertibleTo(target.Type().Elem()) {
			elem.Elem().Set(src.Convert(target.Type().Elem()))
			target.Set(elem)
			return nil
		}
	}
	return errors.New("unsupported scan assignment")
}

func decodeSessionArgs(args []any) (Session, error) {
	session := Session{
		ID:        args[0].(string),
		UserID:    args[1].(string),
		State:     args[2].(SessionState),
		CreatedAt: args[4].(time.Time),
		UpdatedAt: args[5].(time.Time),
	}
	if err := json.Unmarshal(args[3].([]byte), &session.Metadata); err != nil {
		return Session{}, err
	}
	return session, nil
}

func decodeMessageArgs(args []any) (Message, error) {
	msg := Message{
		ID:        args[0].(string),
		SessionID: args[1].(string),
		Role:      args[2].(Role),
		Content:   args[3].(string),
		CreatedAt: args[7].(time.Time),
	}
	if raw := args[4].([]byte); len(raw) > 0 {
		if err := json.Unmarshal(raw, &msg.ToolCalls); err != nil {
			return Message{}, err
		}
	}
	if raw := args[5]; raw != nil {
		text := raw.(string)
		var result ToolResult
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			return Message{}, err
		}
		msg.ToolResult = &result
	}
	if err := json.Unmarshal(args[6].([]byte), &msg.Metadata); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(strings.ToLower(sql)), " ")
}

func cloneSessions(in map[string]Session) map[string]Session {
	out := make(map[string]Session, len(in))
	for k, v := range in {
		out[k] = cloneSession(v)
	}
	return out
}

func cloneMessages(in map[string]Message) map[string]Message {
	out := make(map[string]Message, len(in))
	for k, v := range in {
		out[k] = cloneMessage(v)
	}
	return out
}

func cloneSession(in Session) Session {
	out := in
	out.Metadata = cloneStringMap(in.Metadata)
	out.Messages = append([]Message(nil), in.Messages...)
	return out
}

func cloneMessage(in Message) Message {
	out := in
	out.Metadata = cloneStringMap(in.Metadata)
	if in.ToolCalls != nil {
		out.ToolCalls = append([]ToolCall(nil), in.ToolCalls...)
	}
	if in.ToolResult != nil {
		result := *in.ToolResult
		out.ToolResult = &result
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
