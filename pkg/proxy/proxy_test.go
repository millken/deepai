package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- MemoryEventStore tests ---

func TestMemoryEventStoreAppend(t *testing.T) {
	s := NewMemoryEventStore()
	ctx := context.Background()

	err := s.Append(ctx, []LogEvent{
		{Timestamp: time.Now().UTC(), Type: EventStart, ID: "r-1", Method: "POST", Path: "/v1/chat/completions", Model: "gpt-4o", APIFormat: "openai"},
		{Timestamp: time.Now().UTC(), Type: EventReqBody, ID: "r-1", Body: RawBody(`{"model":"gpt-4o"}`), BodySize: 15},
		{Timestamp: time.Now().UTC(), Type: EventRespBody, ID: "r-1", Body: RawBody(`{"id":"chatcmpl-1"}`), BodySize: 18},
		{Timestamp: time.Now().UTC(), Type: EventDone, ID: "r-1", StatusCode: 200, Duration: "100ms"},
	}...)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	if s.RequestCount() != 1 {
		t.Fatalf("RequestCount = %d, want 1", s.RequestCount())
	}
	if s.EventCount() != 4 {
		t.Fatalf("EventCount = %d, want 4", s.EventCount())
	}
}

func TestMemoryEventStoreTimeline(t *testing.T) {
	s := NewMemoryEventStore()
	ctx := context.Background()

	s.Append(ctx, []LogEvent{
		{Timestamp: time.Now().UTC(), Type: EventStart, ID: "r-1", Model: "gpt-4o"},
		{Timestamp: time.Now().UTC(), Type: EventDelta, ID: "r-1", Text: "Hello"},
		{Timestamp: time.Now().UTC(), Type: EventDone, ID: "r-1", StatusCode: 200},
	}...)

	timeline, err := s.GetTimeline(ctx, "r-1")
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(timeline) != 3 {
		t.Fatalf("timeline len = %d, want 3", len(timeline))
	}
	if timeline[0].Type != EventStart {
		t.Errorf("event[0] type = %q, want start", timeline[0].Type)
	}
	if timeline[1].Text != "Hello" {
		t.Errorf("event[1] text = %q, want Hello", timeline[1].Text)
	}
	if timeline[2].StatusCode != 200 {
		t.Errorf("event[2] status = %d, want 200", timeline[2].StatusCode)
	}

	_, err = s.GetTimeline(ctx, "nonexistent")
	if err != ErrNotFound {
		t.Fatalf("GetTimeline nonexistent = %v, want ErrNotFound", err)
	}
}

func TestMemoryEventStoreListRequests(t *testing.T) {
	s := NewMemoryEventStore()
	ctx := context.Background()

	for i := range 3 {
		s.Append(ctx, []LogEvent{
			{Timestamp: time.Now().UTC(), Type: EventStart, ID: fmt.Sprintf("r-%d", i), Model: fmt.Sprintf("model-%d", i)},
			{Timestamp: time.Now().UTC(), Type: EventDone, ID: fmt.Sprintf("r-%d", i), StatusCode: 200},
		}...)
	}

	summaries, err := s.ListRequests(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("ListRequests count = %d, want 3", len(summaries))
	}

	page, _ := s.ListRequests(ctx, 1, 1)
	if len(page) != 1 || page[0].ID != "r-1" {
		t.Errorf("ListRequests(1,1) = %v, want [r-1]", page)
	}

	empty, _ := s.ListRequests(ctx, 10, 10)
	if len(empty) != 0 {
		t.Errorf("ListRequests beyond range = %d, want 0", len(empty))
	}
}

func TestMemoryEventStoreConcurrent(t *testing.T) {
	s := NewMemoryEventStore()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Append(ctx, LogEvent{Timestamp: time.Now().UTC(), Type: EventStart, ID: fmt.Sprintf("c-%d", i)})
		}(i)
	}
	wg.Wait()

	if s.RequestCount() != 100 {
		t.Fatalf("RequestCount = %d, want 100", s.RequestCount())
	}
}

// --- Helper: test environment ---

type testEnv struct {
	upstream *httptest.Server
	proxy    *Proxy
	store    *MemoryEventStore
}

func newTestEnv(t *testing.T, handler http.HandlerFunc) *testEnv {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	store := NewMemoryEventStore()
	p, err := NewProxy(slog.Default(), Config{
		Addr:           ":0",
		OpenAI:         UpstreamConfig{BaseURL: upstream.URL, APIKey: "sk-test-openai"},
		Anthropic:      UpstreamConfig{BaseURL: upstream.URL, APIKey: "sk-test-anthropic"},
		MaxRequestBody: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	p.WithStore(store)

	return &testEnv{
		upstream: upstream,
		proxy:    p,
		store:    store,
	}
}

func (e *testEnv) do(req *http.Request) (*http.Response, []byte) {
	rec := httptest.NewRecorder()
	e.proxy.routes().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.Bytes()
}

// --- Non-streaming proxy test ---

func TestProxyNonStreaming(t *testing.T) {
	upstreamCalled := false
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-openai" {
			t.Errorf("upstream auth = %q, want Bearer sk-test-openai", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-123",
			"model": "gpt-4o",
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "hello"}},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, respBody := env.do(req)

	if !upstreamCalled {
		t.Fatal("upstream was not called")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)

	if env.store.RequestCount() != 1 {
		t.Fatalf("store request count = %d, want 1", env.store.RequestCount())
	}

	timeline, _ := env.store.GetTimeline(context.Background(), func() string {
		summaries, _ := env.store.ListRequests(context.Background(), 0, 1)
		return summaries[0].ID
	}())

	// Should have: start, req_body, resp_body, done
	types := make([]EventType, len(timeline))
	for i, evt := range timeline {
		types[i] = evt.Type
	}

	if len(timeline) != 4 {
		t.Fatalf("timeline events = %d (types: %v), want 4", len(timeline), types)
	}
	if timeline[0].Type != EventStart {
		t.Errorf("event[0] = %q, want start", timeline[0].Type)
	}
	if timeline[1].Type != EventReqBody {
		t.Errorf("event[1] = %q, want req_body", timeline[1].Type)
	}
	if timeline[2].Type != EventRespBody {
		t.Errorf("event[2] = %q, want resp_body", timeline[2].Type)
	}
	if timeline[3].Type != EventDone {
		t.Errorf("event[3] = %q, want done", timeline[3].Type)
	}

	// Check start event fields.
	start := timeline[0]
	if start.Model != "gpt-4o" {
		t.Errorf("start model = %q, want gpt-4o", start.Model)
	}
	if start.APIFormat != "openai" {
		t.Errorf("start format = %q, want openai", start.APIFormat)
	}
	if start.Streaming {
		t.Error("start streaming = true, want false")
	}

	// Check done event fields.
	done := timeline[3]
	if done.StatusCode != http.StatusOK {
		t.Errorf("done status = %d, want 200", done.StatusCode)
	}

	// Verify response body forwarded correctly.
	var respData map[string]any
	json.Unmarshal(respBody, &respData)
	if respData["model"] != "gpt-4o" {
		t.Errorf("response model = %v, want gpt-4o", respData["model"])
	}
}

// --- Streaming proxy test ---

func TestProxyStreaming(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		chunks := []string{
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"He"}}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"llo"}}]}` + "\n\n",
			`data: {"id":"chatcmpl-1","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			fmt.Fprint(w, chunk)
			flusher.Flush()
		}
	}))

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	env.proxy.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	time.Sleep(50 * time.Millisecond)

	summaries, _ := env.store.ListRequests(context.Background(), 0, 1)
	if len(summaries) != 1 {
		t.Fatalf("store requests = %d, want 1", len(summaries))
	}

	timeline, _ := env.store.GetTimeline(context.Background(), summaries[0].ID)

	// Find events by type.
	var usageEvt *LogEvent
	var doneEvt *LogEvent
	for i := range timeline {
		switch timeline[i].Type {
		case EventUsage:
			usageEvt = &timeline[i]
		case EventDone:
			doneEvt = &timeline[i]
		}
	}

	if usageEvt == nil {
		t.Fatal("usage event not found")
	}
	if usageEvt.InputTokens != 10 {
		t.Errorf("usage input = %d, want 10", usageEvt.InputTokens)
	}
	if usageEvt.OutputTokens != 5 {
		t.Errorf("usage output = %d, want 5", usageEvt.OutputTokens)
	}

	if doneEvt == nil {
		t.Fatal("done event not found")
	}
	if doneEvt.StatusCode != 200 {
		t.Errorf("done status = %d, want 200", doneEvt.StatusCode)
	}
	if doneEvt.TTFB == "" {
		t.Error("done ttfb is empty")
	}
	if doneEvt.ChunkCount != 2 {
		t.Errorf("done chunks = %d, want 2", doneEvt.ChunkCount)
	}

	// Verify client received SSE.
	respBody := rec.Body.String()
	if !strings.Contains(respBody, `data: {"id":"chatcmpl-1"`) {
		t.Errorf("client response missing SSE data: %s", respBody)
	}
	if !strings.Contains(respBody, "data: [DONE]") {
		t.Error("client response missing [DONE]")
	}
}

// --- Anthropic format test ---

func TestProxyAnthropicFormat(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-test-anthropic" {
			t.Errorf("upstream x-api-key = %q, want sk-test-anthropic", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "msg-123",
			"model":   "claude-3-5-sonnet-20241022",
			"content": []map[string]any{{"type": "text", "text": "hi from claude"}},
			"usage":   map[string]int{"input_tokens": 20, "output_tokens": 10},
		})
	}))

	body := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hello"}],"max_tokens":1024}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, _ := env.do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)

	summaries, _ := env.store.ListRequests(context.Background(), 0, 1)
	if len(summaries) != 1 {
		t.Fatalf("store requests = %d, want 1", len(summaries))
	}
	if summaries[0].APIFormat != "anthropic" {
		t.Errorf("format = %q, want anthropic", summaries[0].APIFormat)
	}
}

// --- Health check test ---

func TestProxyHealth(t *testing.T) {
	env := newTestEnv(t, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	resp, body := env.do(req)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result map[string]string
	json.Unmarshal(body, &result)
	if result["status"] != "ok" {
		t.Errorf("health status = %q, want ok", result["status"])
	}
}

// --- extractStreamEvents tests ---

func TestExtractStreamEventsOpenAI(t *testing.T) {
	data := []byte(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n" +
			"data: {\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":25,\"total_tokens\":75}}\n\n" +
			"data: [DONE]\n\n",
	)

	events, chunkCount, _ := extractStreamEvents(data, "openai", "test-1", "req-1")

	usageEvents := filterEvents(events, EventUsage)
	if len(usageEvents) != 1 {
		t.Fatalf("usage events = %d, want 1", len(usageEvents))
	}
	if usageEvents[0].InputTokens != 50 {
		t.Errorf("input = %d, want 50", usageEvents[0].InputTokens)
	}
	if usageEvents[0].OutputTokens != 25 {
		t.Errorf("output = %d, want 25", usageEvents[0].OutputTokens)
	}
	if chunkCount != 2 {
		t.Errorf("chunkCount = %d, want 2", chunkCount)
	}
}

func TestExtractStreamEventsAnthropic(t *testing.T) {
	data := []byte(
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" there\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":30}}\n\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":30}},\"usage\":{\"input_tokens\":60}}\n\n" +
			"data: [DONE]\n\n",
	)

	events, chunkCount, _ := extractStreamEvents(data, "anthropic", "test-1", "req-1")

	usageEvents := filterEvents(events, EventUsage)
	if len(usageEvents) < 1 {
		t.Fatal("no usage events found")
	}
	// Should pick up input_tokens=60 and output_tokens=30.
	var maxInput, maxOutput int
	for _, u := range usageEvents {
		if u.InputTokens > maxInput {
			maxInput = u.InputTokens
		}
		if u.OutputTokens > maxOutput {
			maxOutput = u.OutputTokens
		}
	}
	if maxInput != 60 {
		t.Errorf("max input = %d, want 60", maxInput)
	}
	if maxOutput != 30 {
		t.Errorf("max output = %d, want 30", maxOutput)
	}
	if chunkCount != 2 {
		t.Errorf("chunkCount = %d, want 2", chunkCount)
	}
}

func filterEvents(events []LogEvent, typ EventType) []LogEvent {
	var out []LogEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// --- detectAPIFormat test ---

func TestDetectAPIFormat(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", "openai"},
		{"/v1/messages", "anthropic"},
		{"/v1/messages/batches", "anthropic"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest("POST", tt.path, nil)
		got := detectAPIFormat(req)
		if got != tt.want {
			t.Errorf("detectAPIFormat(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// --- FileEventStore tests ---

func TestFileEventStoreCRUD(t *testing.T) {
	path := t.TempDir() + "/proxy.jsonl"
	s, err := NewFileEventStore(FileEventStoreConfig{Path: path})
	if err != nil {
		t.Fatalf("NewFileEventStore: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	s.Append(ctx, []LogEvent{
		{Timestamp: time.Now().UTC(), Type: EventStart, ID: "f-1", Method: "POST", Path: "/v1/chat/completions", APIFormat: "openai", Model: "gpt-4o"},
		{Timestamp: time.Now().UTC(), Type: EventReqBody, ID: "f-1", Body: RawBody(`{"model":"gpt-4o"}`), BodySize: 15},
		{Timestamp: time.Now().UTC(), Type: EventDone, ID: "f-1", StatusCode: 200, Duration: "100ms"},
	}...)

	s.Append(ctx, []LogEvent{
		{Timestamp: time.Now().UTC(), Type: EventStart, ID: "f-2", Method: "POST", Path: "/v1/messages", APIFormat: "anthropic", Model: "claude-3"},
		{Timestamp: time.Now().UTC(), Type: EventDone, ID: "f-2", StatusCode: 200},
	}...)

	if s.RequestCount() != 2 {
		t.Fatalf("RequestCount = %d, want 2", s.RequestCount())
	}

	// GetTimeline.
	timeline, err := s.GetTimeline(ctx, "f-1")
	if err != nil {
		t.Fatalf("GetTimeline f-1: %v", err)
	}
	if len(timeline) != 3 {
		t.Fatalf("f-1 timeline len = %d, want 3", len(timeline))
	}
	if timeline[0].Model != "gpt-4o" {
		t.Errorf("f-1 start model = %q, want gpt-4o", timeline[0].Model)
	}

	// ListRequests.
	summaries, _ := s.ListRequests(ctx, 0, 10)
	if len(summaries) != 2 {
		t.Fatalf("ListRequests count = %d, want 2", len(summaries))
	}
	if summaries[0].ID != "f-1" || summaries[1].ID != "f-2" {
		t.Errorf("order = %v, want [f-1,f-2]", []string{summaries[0].ID, summaries[1].ID})
	}

	// Nonexistent.
	_, err = s.GetTimeline(ctx, "nonexistent")
	if err != ErrNotFound {
		t.Fatalf("GetTimeline nonexistent = %v, want ErrNotFound", err)
	}
}

func TestFileEventStoreReopen(t *testing.T) {
	path := t.TempDir() + "/proxy.jsonl"

	s1, _ := NewFileEventStore(FileEventStoreConfig{Path: path})
	ctx := context.Background()
	s1.Append(ctx, []LogEvent{
		{Timestamp: time.Now().UTC(), Type: EventStart, ID: "a-1", Model: "gpt-4o"},
		{Timestamp: time.Now().UTC(), Type: EventDone, ID: "a-1", StatusCode: 200},
	}...)
	s1.Append(ctx, []LogEvent{
		{Timestamp: time.Now().UTC(), Type: EventStart, ID: "a-2", Model: "claude-3"},
		{Timestamp: time.Now().UTC(), Type: EventDone, ID: "a-2", StatusCode: 200},
	}...)
	s1.Close()

	s2, err := NewFileEventStore(FileEventStoreConfig{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if s2.RequestCount() != 2 {
		t.Fatalf("reopened RequestCount = %d, want 2", s2.RequestCount())
	}

	timeline, _ := s2.GetTimeline(ctx, "a-2")
	if len(timeline) < 1 {
		t.Fatal("reopened timeline empty for a-2")
	}
	if timeline[0].Model != "claude-3" {
		t.Errorf("reopened model = %q, want claude-3", timeline[0].Model)
	}
}

func TestFileEventStoreBodyTruncation(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/proxy.jsonl"
	s, _ := NewFileEventStore(FileEventStoreConfig{Path: path, MaxInlineBodySize: 100})
	defer s.Close()

	ctx := context.Background()

	// Body larger than threshold.
	largeBody := strings.Repeat(`{"key":"value"}`, 20) // ~260 bytes
	s.Append(ctx, LogEvent{Timestamp: time.Now().UTC(), Type: EventStart, ID: "big-1", Model: "gpt-4o"})
	s.Append(ctx, LogEvent{Timestamp: time.Now().UTC(), Type: EventReqBody, ID: "big-1", Body: RawBody(largeBody), BodySize: len(largeBody)})
	s.Append(ctx, LogEvent{Timestamp: time.Now().UTC(), Type: EventDone, ID: "big-1", StatusCode: 200})

	timeline, _ := s.GetTimeline(ctx, "big-1")

	// Find req_body event.
	var reqBodyEvt *LogEvent
	for i := range timeline {
		if timeline[i].Type == EventReqBody {
			reqBodyEvt = &timeline[i]
			break
		}
	}
	if reqBodyEvt == nil {
		t.Fatal("req_body event not found")
	}
	if reqBodyEvt.BodyFile == "" {
		t.Error("BodyFile should be set for large body")
	}
	if reqBodyEvt.BodySize != len(largeBody) {
		t.Errorf("BodySize = %d, want %d", reqBodyEvt.BodySize, len(largeBody))
	}
	if len(reqBodyEvt.Body) != 0 {
		t.Error("Body should be empty when stored externally")
	}

	// Verify external file exists.
	if _, err := s.GetTimeline(ctx, "big-1"); err != nil {
		t.Fatalf("GetTimeline after truncation: %v", err)
	}
}

func TestFileEventStoreWithProxy(t *testing.T) {
	path := t.TempDir() + "/proxy.jsonl"
	fileStore, err := NewFileEventStore(FileEventStoreConfig{Path: path})
	if err != nil {
		t.Fatalf("NewFileEventStore: %v", err)
	}
	defer fileStore.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-1", "model": "gpt-4o",
		})
	}))
	defer upstream.Close()

	p, _ := NewProxy(slog.Default(), Config{
		Addr:           ":0",
		OpenAI:         UpstreamConfig{BaseURL: upstream.URL, APIKey: "sk-test"},
		MaxRequestBody: 1 << 20,
	})
	p.WithStore(fileStore)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	p.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	time.Sleep(50 * time.Millisecond)

	if fileStore.RequestCount() != 1 {
		t.Fatalf("fileStore RequestCount = %d, want 1", fileStore.RequestCount())
	}

	summaries, _ := fileStore.ListRequests(context.Background(), 0, 1)
	if summaries[0].Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", summaries[0].Model)
	}
}

// --- RawBody serialization tests ---

func TestRawBodyJSON(t *testing.T) {
	evt := LogEvent{
		Type: EventReqBody,
		ID:   "json-test",
		Body: RawBody(`{"model":"gpt-4o","stream":true}`),
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	if !strings.Contains(s, `"body":{"model":"gpt-4o"`) {
		t.Errorf("Body not raw JSON: %s", s[:300])
	}
	if strings.Contains(s, `"body":"ey`) {
		t.Error("Body appears to be base64 encoded")
	}
}

func TestRawBodyNonJSON(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	evt := LogEvent{
		Type: EventRespBody,
		ID:   "sse-test",
		Body: RawBody(sse),
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	if !strings.Contains(s, "event: message_start") {
		t.Errorf("SSE content lost: %s", s[:300])
	}
}

// --- streamingRecorder test ---

func TestStreamingRecorder(t *testing.T) {
	rec := httptest.NewRecorder()
	started := time.Now()
	sr := newStreamingRecorder(rec, started)

	data := []byte("data: hello\n\ndata: world\n\n")
	n, err := sr.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}

	sr.mu.Lock()
	bufLen := sr.buf.Len()
	firstByte := sr.firstByte
	sr.mu.Unlock()

	if bufLen != len(data) {
		t.Errorf("buf len = %d, want %d", bufLen, len(data))
	}
	if firstByte.IsZero() {
		t.Error("firstByte not set")
	}
}

// --- reconstructStreamBody tests ---

func TestReconstructAnthropicStream(t *testing.T) {
	data := []byte(
		"event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"role\":\"assistant\",\"model\":\"claude-3\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"sig1\"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Hello \"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"world\"}}\n\n" +
			"event: content_block_stop\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi \"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"there\"}}\n\n" +
			"event: content_block_stop\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
			"event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"Read\",\"input\":{}}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file\\\":\\\"a.go\\\"}\"}}\n\n" +
			"event: content_block_stop\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
			"event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":50}}\n\n" +
			"event: message_stop\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)

	out := reconstructStreamBody(data, "anthropic")

	var msg map[string]json.RawMessage
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("reconstructed body is not valid JSON: %v", err)
	}

	// Check id.
	if string(msg["id"]) != `"msg-1"` {
		t.Errorf("id = %s, want msg-1", msg["id"])
	}

	// Check content blocks.
	var content []map[string]any
	if err := json.Unmarshal(msg["content"], &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if len(content) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(content))
	}

	// Block 0: thinking
	if content[0]["type"] != "thinking" {
		t.Errorf("block[0] type = %v, want thinking", content[0]["type"])
	}
	if content[0]["thinking"] != "Hello world" {
		t.Errorf("block[0] thinking = %q, want 'Hello world'", content[0]["thinking"])
	}

	// Block 1: text
	if content[1]["type"] != "text" {
		t.Errorf("block[1] type = %v, want text", content[1]["type"])
	}
	if content[1]["text"] != "Hi there" {
		t.Errorf("block[1] text = %q, want 'Hi there'", content[1]["text"])
	}

	// Block 2: tool_use
	if content[2]["type"] != "tool_use" {
		t.Errorf("block[2] type = %v, want tool_use", content[2]["type"])
	}
	if content[2]["name"] != "Read" {
		t.Errorf("block[2] name = %v, want Read", content[2]["name"])
	}

	// Check usage.
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	json.Unmarshal(msg["usage"], &usage)
	if usage.InputTokens != 10 {
		t.Errorf("usage.input = %d, want 10", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("usage.output = %d, want 50", usage.OutputTokens)
	}
}

func TestReconstructOpenAIStream(t *testing.T) {
	data := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
			"data: [DONE]\n\n",
	)

	out := reconstructStreamBody(data, "openai")

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("reconstructed body is not valid JSON: %v", err)
	}

	if resp["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", resp["model"])
	}

	choices := resp["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello world" {
		t.Errorf("content = %v, want 'Hello world'", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
}

func TestFileEventStoreGetTimelineFromDisk(t *testing.T) {
	path := t.TempDir() + "/proxy.jsonl"
	s, err := NewFileEventStore(FileEventStoreConfig{Path: path})
	if err != nil {
		t.Fatalf("NewFileEventStore: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	s.Append(ctx, LogEvent{Timestamp: time.Now().UTC(), Type: EventStart, ID: "r-1", Model: "gpt-4o"})
	s.Append(ctx, LogEvent{Timestamp: time.Now().UTC(), Type: EventDone, ID: "r-1", StatusCode: 200})

	timeline, err := s.GetTimeline(ctx, "r-1")
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(timeline) != 2 {
		t.Fatalf("timeline len = %d, want 2", len(timeline))
	}
	if timeline[0].Type != EventStart {
		t.Errorf("event[0] = %q, want start", timeline[0].Type)
	}
	if timeline[0].Model != "gpt-4o" {
		t.Errorf("event[0] model = %q, want gpt-4o", timeline[0].Model)
	}

	_, err = s.GetTimeline(ctx, "nonexistent")
	if err != ErrNotFound {
		t.Fatalf("GetTimeline nonexistent = %v, want ErrNotFound", err)
	}
}

// TestProxyUpstreamClientHonorsEnvProxy: the recording proxy forwards to
// api.openai.com / api.anthropic.com through its own http.Transport, which — like
// every hand-built Transport — ignores HTTP_PROXY/HTTPS_PROXY unless Proxy is set.
// Without it, `deepai proxy` cannot reach the upstreams from behind a firewall
// even though direct model calls can.
func TestProxyUpstreamClientHonorsEnvProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	p, err := NewProxy(slog.Default(), Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	transport, ok := p.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("upstream Transport is %T, want *http.Transport", p.httpClient.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("upstream Transport.Proxy is nil; the recording proxy ignores the proxy environment")
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("Transport.Proxy() error = %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "127.0.0.1:7890" {
		t.Fatalf("Transport.Proxy() = %v, want 127.0.0.1:7890", proxyURL)
	}
}
