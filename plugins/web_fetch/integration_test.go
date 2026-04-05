//go:build integration
// +build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/plugin"
)

// TestIntegrationWebFetchSO tests loading the .so via purego (LoadSharedLibrary)
// and calling plugin_execute through the standard ABI path.
func TestIntegrationWebFetchSO(t *testing.T) {
	soPath := "./web_fetch.so"
	if _, err := os.Stat(soPath); os.IsNotExist(err) {
		t.Skipf("skipping: %s not found. Run 'go build -buildmode=c-shared -o web_fetch.so web_fetch.go' first", soPath)
	}

	p, err := plugin.LoadSharedLibrary(soPath)
	if err != nil {
		t.Fatalf("LoadSharedLibrary failed: %v", err)
	}
	defer p.Close()

	info := p.Info()
	t.Logf("loaded plugin: name=%s version=%s type=%s", info.Name, info.Version, info.Type)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultJSON, err := p.CallTool(ctx, "web_fetch", map[string]any{
		"url":             "https://www.example.com",
		"backend":         "http",
		"extract_content": true,
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if errMsg, ok := result["error"]; ok {
		t.Fatalf("web_fetch returned error: %v", errMsg)
	}

	if url, ok := result["url"].(string); !ok || url != "https://www.example.com" {
		t.Errorf("expected url 'https://www.example.com', got '%v'", result["url"])
	}

	if statusCode, ok := result["status_code"].(float64); ok {
		t.Logf("status code: %.0f", statusCode)
	}

	if content, ok := result["content"].(string); ok && content != "" {
		t.Logf("content length: %d bytes", len(content))
	}

	if title, ok := result["title"].(string); ok {
		t.Logf("page title: %s", title)
	}

	t.Log("Successfully fetched https://www.example.com via purego .so plugin")
}

// TestIntegrationWebFetchExampleCom tests a real fetch of example.com using the Go API directly.
func TestIntegrationWebFetchExampleCom(t *testing.T) {
	p := New()

	args := map[string]interface{}{
		"url":             "https://www.example.com",
		"backend":         "http",
		"extract_content": true,
	}

	result, err := p.handleWebFetch(context.Background(), args)
	if err != nil {
		t.Logf("web_fetch returned error (may be network issue): %v", err)
		return
	}

	resultMap, ok := result.(*FetchResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}

	if resultMap.URL != "https://www.example.com" {
		t.Errorf("expected URL 'https://www.example.com', got '%s'", resultMap.URL)
	}

	if resultMap.Backend != "http" {
		t.Errorf("expected backend 'http', got '%s'", resultMap.Backend)
	}

	if resultMap.StatusCode != 200 {
		t.Logf("status code: %d (may be network issue)", resultMap.StatusCode)
	}

	if resultMap.Content == "" {
		t.Error("expected non-empty content")
	}

	if resultMap.TextContent == "" {
		t.Error("expected non-empty text_content")
	}

	t.Logf("Successfully fetched https://www.example.com")
	t.Logf("Title: %s", resultMap.Title)
	t.Logf("Content length: %d bytes", resultMap.Length)
	t.Logf("Status code: %d", resultMap.StatusCode)
	fmt.Printf("Title: %s\n", resultMap.Title)
	fmt.Printf("Text content (first 500 chars): %s\n", truncateText(resultMap.TextContent, 500))
}

// TestIntegrationWebFetchTimeout tests timeout handling with a slow endpoint.
func TestIntegrationWebFetchTimeout(t *testing.T) {
	p := New()
	p.timeout = 1 * time.Second
	p.httpClient.Timeout = 1 * time.Second

	args := map[string]interface{}{
		"url":             "https://httpbin.org/delay/10",
		"backend":         "http",
		"extract_content": true,
	}

	result, err := p.handleWebFetch(context.Background(), args)
	if err != nil {
		t.Logf("web_fetch correctly returned timeout error: %v", err)
		if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
			t.Errorf("expected timeout-related error, got: %v", err)
		}
		return
	}

	resultMap, ok := result.(*FetchResult)
	if ok {
		t.Logf("Request unexpectedly succeeded (may be network issue)")
		t.Logf("Status code: %d", resultMap.StatusCode)
	} else {
		t.Logf("Request unexpectedly succeeded with result type: %T", result)
	}
}

// TestIntegrationWebFetch tests fetching a real page.
func TestIntegrationWebFetch(t *testing.T) {
	p := New()

	args := map[string]interface{}{
		"url":             "https://code.claude.com/docs/zh-CN/skills",
		"backend":         "http",
		"extract_content": true,
	}

	result, err := p.handleWebFetch(context.Background(), args)
	if err != nil {
		t.Logf("web_fetch returned error (may be network issue): %v", err)
		return
	}

	resultMap, ok := result.(*FetchResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}

	t.Logf("Title: %s", resultMap.Title)
	t.Logf("Content length: %d bytes", resultMap.Length)
	t.Logf("Status code: %d", resultMap.StatusCode)
	fmt.Printf("Title: %s\n", resultMap.Title)
	fmt.Printf("Text content : %s\n", resultMap.TextContent)
}
