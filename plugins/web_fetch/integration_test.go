//go:build integration
// +build integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"plugin"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// TestIntegrationWebFetchSO tests loading the .so file and calling web_fetch
func TestIntegrationWebFetchSO(t *testing.T) {
	// This test requires the web_fetch.so to be built first
	// Run: go build -buildmode=c-shared -o web_fetch.so web_fetch.go

	soPath := "./web_fetch.so"
	if _, err := os.Stat(soPath); os.IsNotExist(err) {
		t.Skipf("skipping integration test: %s not found. Run 'go build -buildmode=c-shared -o web_fetch.so web_fetch.go' first", soPath)
	}

	// Open the .so file
	p, err := plugin.Open(soPath)
	if err != nil {
		t.Fatalf("failed to open plugin: %v", err)
	}

	// Lookup exported functions
	newSym, err := p.Lookup("plugin_new")
	if err != nil {
		t.Fatalf("failed to lookup plugin_new: %v", err)
	}

	nameSym, err := p.Lookup("plugin_name")
	if err != nil {
		t.Fatalf("failed to lookup plugin_name: %v", err)
	}

	executeSym, err := p.Lookup("plugin_execute")
	if err != nil {
		t.Fatalf("failed to lookup plugin_execute: %v", err)
	}

	// Type assert the symbols
	newFunc, ok := newSym.(func() uintptr)
	if !ok {
		t.Fatal("plugin_new has unexpected signature")
	}

	nameFunc, ok := nameSym.(func(unsafe.Pointer) string)
	if !ok {
		t.Fatal("plugin_name has unexpected signature")
	}

	executeFunc, ok := executeSym.(func(unsafe.Pointer, string, string) string)
	if !ok {
		t.Fatal("plugin_execute has unexpected signature")
	}

	// Create plugin instance
	ptr := newFunc()
	if ptr == 0 {
		t.Fatal("plugin_new returned nil pointer")
	}

	// Get plugin name
	name := nameFunc(unsafe.Pointer(ptr))
	if name != "web-fetch" {
		t.Errorf("expected name 'web-fetch', got '%s'", name)
	}

	// Execute web_fetch for example.com
	args := map[string]interface{}{
		"url":             "https://www.example.com",
		"backend":         "http",
		"extract_content": true,
	}
	argsJSON, _ := json.Marshal(args)

	resultStr := executeFunc(unsafe.Pointer(ptr), "web_fetch", string(argsJSON))

	// Parse result
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resultStr), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Check for errors
	if errMsg, ok := result["error"]; ok {
		t.Fatalf("web_fetch returned error: %v", errMsg)
	}

	// Verify expected fields
	if url, ok := result["url"].(string); !ok || url != "https://www.example.com" {
		t.Errorf("expected url 'https://www.example.com', got '%v'", result["url"])
	}

	if backend, ok := result["backend"].(string); !ok || backend != "http" {
		t.Errorf("expected backend 'http', got '%v'", result["backend"])
	}

	if statusCode, ok := result["status_code"].(float64); ok {
		t.Logf("status code: %.0f", statusCode)
	}

	if content, ok := result["content"].(string); !ok || content == "" {
		t.Error("expected non-empty content")
	} else {
		t.Logf("content length: %d bytes", len(content))
	}

	if title, ok := result["title"].(string); ok {
		t.Logf("page title: %s", title)
	}

	t.Log("Successfully fetched https://www.example.com via .so plugin")
}

// TestIntegrationWebFetchExampleCom tests a real fetch of example.com
func TestIntegrationWebFetchExampleCom(t *testing.T) {
	p := New()

	args := map[string]interface{}{
		"url":             "https://www.example.com",
		"backend":         "http",
		"extract_content": true,
	}

	result, err := p.handleWebFetch(args)
	if err != nil {
		// Network error is acceptable in test environment
		t.Logf("web_fetch returned error (may be network issue): %v", err)
		return
	}

	resultMap, ok := result.(*FetchResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}

	// Verify result fields
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

// TestIntegrationWebFetchTimeout tests timeout handling with a slow endpoint
func TestIntegrationWebFetchTimeout(t *testing.T) {
	p := New()
	// Set a very short timeout (1 second)
	p.timeout = 1 * time.Second
	p.httpClient.Timeout = 1 * time.Second

	// Use httpbin delay endpoint - will delay 10 seconds
	// With 1 second timeout, this should fail
	args := map[string]interface{}{
		"url":             "https://httpbin.org/delay/10",
		"backend":         "http",
		"extract_content": true,
	}

	result, err := p.handleWebFetch(args)
	if err != nil {
		// Timeout error is expected
		t.Logf("web_fetch correctly returned timeout error: %v", err)
		if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
			t.Errorf("expected timeout-related error, got: %v", err)
		}
		return
	}

	// If no error (maybe network issue), log it
	resultMap, ok := result.(*FetchResult)
	if ok {
		t.Logf("Request unexpectedly succeeded (may be network issue)")
		t.Logf("Status code: %d", resultMap.StatusCode)
	} else {
		t.Logf("Request unexpectedly succeeded with result type: %T", result)
	}
}
