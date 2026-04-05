package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWebFetchPlugin(t *testing.T) {
	// Test plugin creation using internal functions
	p := New()
	if p == nil {
		t.Fatal("New returned nil plugin")
	}

	// Test default values
	if p.defaultBackend != "http" {
		t.Errorf("expected default backend 'http', got '%s'", p.defaultBackend)
	}
	if p.timeout != 30*time.Second {
		t.Errorf("expected timeout 30s, got %v", p.timeout)
	}
	if p.maxContentLength != 1000000 {
		t.Errorf("expected max content length 1000000, got %d", p.maxContentLength)
	}

	// Test plugin tools
	tools := p.tools
	if len(tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(tools))
	}

	// Check tool names
	toolNames := make(map[string]bool)
	for _, tool := range tools {
		toolNames[tool.Name] = true
	}

	if !toolNames["web_fetch"] {
		t.Error("web_fetch tool not found")
	}
	if !toolNames["web_fetch_batch"] {
		t.Error("web_fetch_batch tool not found")
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		html     string
		expected string
	}{
		{
			html:     `<html><head><title>Test Title</title></head><body></body></html>`,
			expected: "Test Title",
		},
		{
			html:     `<html><head><title>  Spaced Title  </title></head><body></body></html>`,
			expected: "Spaced Title",
		},
		{
			html:     `<html><head></head><body></body></html>`,
			expected: "Untitled",
		},
		{
			html:     `<html><head><title>Unterminated Title`,
			expected: "Untitled",
		},
	}

	for _, test := range tests {
		result := extractTitle(test.html)
		if result != test.expected {
			t.Errorf("extractTitle(%q) = %q, want %q", test.html, result, test.expected)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		html     string
		contains string
	}{
		{
			html:     `<p>Hello World</p>`,
			contains: "Hello World",
		},
		{
			html:     `<script>alert('test');</script><p>Content</p>`,
			contains: "Content",
		},
		{
			html:     `<style>.class { color: red; }</style><p>Styled</p>`,
			contains: "Styled",
		},
		{
			html:     `<p>Multiple   Spaces</p>`,
			contains: "Multiple Spaces",
		},
		{
			html:     `<p>&nbsp;&<></p>`,
			contains: "&",
		},
	}

	for _, test := range tests {
		result := htmlToText(test.html)
		if !strings.Contains(result, test.contains) {
			t.Errorf("htmlToText(%q) = %q, should contain %q", test.html, result, test.contains)
		}
	}
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		text     string
		maxLen   int
		expected string
	}{
		{
			text:     "Short text",
			maxLen:   20,
			expected: "Short text",
		},
		{
			text:     "This is a very long text that should be truncated",
			maxLen:   20,
			expected: "This is a very long ...",
		},
		{
			text:     "",
			maxLen:   10,
			expected: "",
		},
	}

	for _, test := range tests {
		result := truncateText(test.text, test.maxLen)
		if result != test.expected {
			t.Errorf("truncateText(%q, %d) = %q, want %q", test.text, test.maxLen, result, test.expected)
		}
	}
}

func TestHandleWebFetchErrorHandling(t *testing.T) {
	p := New()

	// Test missing URL
	args := map[string]interface{}{}
	_, err := p.handleWebFetch(context.Background(), args)
	if err == nil {
		t.Error("expected error for missing URL")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error should mention 'url', got: %v", err)
	}

	// Test invalid URL type
	args = map[string]interface{}{
		"url": 123,
	}
	_, err = p.handleWebFetch(context.Background(), args)
	if err == nil {
		t.Error("expected error for invalid URL type")
	}
}

func TestHandleWebFetchBatchErrorHandling(t *testing.T) {
	p := New()

	// Test missing URLs
	args := map[string]interface{}{}
	_, err := p.handleWebFetchBatch(context.Background(), args)
	if err == nil {
		t.Error("expected error for missing urls")
	}

	// Test empty URLs array
	args = map[string]interface{}{
		"urls": []interface{}{},
	}
	_, err = p.handleWebFetchBatch(context.Background(), args)
	if err == nil {
		t.Error("expected error for empty urls array")
	}

	// Test invalid URLs type
	args = map[string]interface{}{
		"urls": "not-an-array",
	}
	_, err = p.handleWebFetchBatch(context.Background(), args)
	if err == nil {
		t.Error("expected error for invalid urls type")
	}
}

func TestExecuteToolUnknownTool(t *testing.T) {
	p := New()

	_, err := p.executeTool(context.Background(), "unknown_tool", map[string]interface{}{})
	if err == nil {
		t.Error("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error should mention 'unknown tool', got: %v", err)
	}
}

func TestPluginConfiguration(t *testing.T) {
	p := New()

	// Test configuration
	configJSON := `{"default_backend": "chromedp", "timeout": 60, "max_content_length": 500000}`
	config := make(map[string]interface{})
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	// Apply configuration manually (simulating plugin_init)
	p.mu.Lock()
	if backend, ok := config["default_backend"].(string); ok {
		p.defaultBackend = backend
	}
	if timeout, ok := config["timeout"].(float64); ok {
		p.timeout = time.Duration(timeout) * time.Second
		p.httpClient.Timeout = p.timeout
	}
	if maxLen, ok := config["max_content_length"].(float64); ok {
		p.maxContentLength = int64(maxLen)
	}
	p.mu.Unlock()

	// Verify configuration was applied
	p.mu.RLock()
	if p.defaultBackend != "chromedp" {
		t.Errorf("expected backend 'chromedp', got '%s'", p.defaultBackend)
	}
	if p.timeout != 60*time.Second {
		t.Errorf("expected timeout 60s, got %v", p.timeout)
	}
	if p.maxContentLength != 500000 {
		t.Errorf("expected max content length 500000, got %d", p.maxContentLength)
	}
	p.mu.RUnlock()
}

func TestToolDefinitions(t *testing.T) {
	p := New()

	// Verify web_fetch tool definition
	webFetchTool := p.tools[0]
	if webFetchTool.Name != "web_fetch" {
		t.Errorf("expected tool name 'web_fetch', got '%s'", webFetchTool.Name)
	}
	if webFetchTool.Description == "" {
		t.Error("web_fetch tool should have a description")
	}
	if webFetchTool.InputSchema == nil {
		t.Error("web_fetch tool should have an input schema")
	}

	// Check required fields
	schema := webFetchTool.InputSchema
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		if _, ok := props["url"]; !ok {
			t.Error("web_fetch should have 'url' property")
		}
		if _, ok := props["backend"]; !ok {
			t.Error("web_fetch should have 'backend' property")
		}
	}

	// Verify web_fetch_batch tool definition
	batchTool := p.tools[1]
	if batchTool.Name != "web_fetch_batch" {
		t.Errorf("expected tool name 'web_fetch_batch', got '%s'", batchTool.Name)
	}
	if batchTool.Description == "" {
		t.Error("web_fetch_batch tool should have a description")
	}
}
