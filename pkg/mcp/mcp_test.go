package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	mcpproto "github.com/mark3labs/mcp-go/mcp"
	"github.com/millken/deepai/pkg/models"
)

func TestRawSchema_FromRawInputSchema(t *testing.T) {
	raw := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}

	tool := mcpproto.Tool{RawInputSchema: b}
	out := rawSchema(tool)
	if out == nil {
		t.Fatalf("expected non-nil schema")
	}
	if out["type"] != "object" {
		t.Fatalf("unexpected type: %v", out["type"])
	}
}

func TestTools_NotConnected(t *testing.T) {
	c := &Client{}
	_, err := c.Tools(context.Background())
	if err == nil {
		t.Fatalf("expected error when client is not connected")
	}
}

func TestClose_NilReceiver(t *testing.T) {
	var c *Client = nil
	if err := c.Close(); err != nil {
		t.Fatalf("expected nil error when closing nil client, got: %v", err)
	}
}

func TestConnectStdio_HandshakeCtxDoesNotLeak(t *testing.T) {
	// The handshake ctx is cancelled the instant initializeClient returns
	// (via defer cancel()). If Start had been given that handshake ctx, the
	// transport's stored ctx would already be cancelled here and Tools()
	// would fail with ctx.Err(). A successful Tools() proves Start bound the
	// long-lived session ctx instead of the short-lived handshake ctx.
	ctx := context.Background()
	client, err := ConnectStdio(ctx, "mock", "go", nil, "run", "../../cmd/mcp-example/main.go")
	if err != nil {
		t.Fatalf("ConnectStdio: %v", err)
	}
	defer client.Close()
	time.Sleep(100 * time.Millisecond)
	if _, err := client.Tools(ctx); err != nil {
		t.Fatalf("Tools after connect failed — handshake ctx likely leaked into Start: %v", err)
	}
}

func TestConnectStdio_WithExampleServer(t *testing.T) {
	ctx := context.Background()

	// Start example server using `go run cmd/mcp-example/main.go` via ConnectStdio
	// Use a small timeout to avoid hanging tests.
	client, err := ConnectStdio(ctx, "mock", "go", nil, "run", "../../cmd/mcp-example/main.go")
	if err != nil {
		t.Fatalf("ConnectStdio failed: %v", err)
	}
	defer client.Close()

	// Allow server a short moment to be ready for requests
	time.Sleep(100 * time.Millisecond)

	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools failed: %v", err)
	}

	var toolFound bool
	var handlerFound func(ctx context.Context, call models.ToolCall) (models.ToolResult, error)
	for _, tt := range tools {
		if strings.HasSuffix(tt.Name, "test-tool") {
			toolFound = true
			handlerFound = tt.Handler
			break
		}
	}
	if !toolFound {
		t.Fatalf("expected to find test-tool in tools: %+v", tools)
	}

	// Call the tool via the Handler and expect the mock server's text result
	call := models.ToolCall{ID: "1", Name: "test-tool", Arguments: map[string]any{}, Status: models.CallStatusPending}
	res, err := handlerFound(ctx, call)
	if err != nil {
		t.Fatalf("handler call failed: %v", err)
	}
	if res.Status != models.CallStatusCompleted && res.Status != models.CallStatusFailed {
		t.Fatalf("unexpected result status: %v", res.Status)
	}
	if !strings.Contains(res.Content, "tool result") {
		t.Fatalf("expected content to contain 'tool result', got: %s", res.Content)
	}
	// basic JSON validity check
	var tmp any
	if json.Unmarshal([]byte(res.Content), &tmp) != nil {
		t.Fatalf("result content is not valid json: %s", res.Content)
	}
	_ = os.Stdout
}
