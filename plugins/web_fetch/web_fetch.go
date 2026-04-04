// Package main provides a web fetch tool plugin with multiple backends.
// This plugin can be compiled with: go build -buildmode=c-shared -o web_fetch.so web_fetch.go
//
// Features:
// - HTTP backend for static pages
// - Chromedp backend for dynamic/JavaScript-rendered pages
// - Readability extraction for clean article content
package main

/*
#cgo CFLAGS: -I.
#cgo LDFLAGS: -lstdc++ -lm -ldl
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unsafe"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/chromedp/chromedp"
)

// WebFetchPlugin implements the web fetch tool with multiple backends.
type WebFetchPlugin struct {
	mu               sync.RWMutex
	defaultBackend   string
	timeout          time.Duration
	maxContentLength int64
	tools            []ToolDef
	httpClient       *http.Client
}

// ToolDef represents a tool definition.
type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// FetchResult represents the result of a web fetch operation.
type FetchResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	TextContent string `json:"text_content"`
	Excerpt     string `json:"excerpt"`
	Backend     string `json:"backend"`
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type"`
	Length      int    `json:"length"`
}

// Global plugin instance registry (for callbacks)
var plugins = make(map[uintptr]*WebFetchPlugin)
var pluginsMu sync.RWMutex

// New creates a new WebFetchPlugin instance.
func New() *WebFetchPlugin {
	return &WebFetchPlugin{
		defaultBackend:   "http",
		timeout:          30 * time.Second,
		maxContentLength: 1000000,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		tools: []ToolDef{
			{
				Name:        "web_fetch",
				Description: "Fetches web page content. Supports two backends: 'http' for static pages and 'chromedp' for dynamic JavaScript-rendered pages. Uses readability extraction to get clean article content.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url": map[string]interface{}{
							"type":        "string",
							"description": "The URL to fetch",
						},
						"backend": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"http", "chromedp"},
							"description": "Backend to use: 'http' for static pages, 'chromedp' for dynamic JS-rendered pages",
							"default":     "http",
						},
						"extract_content": map[string]interface{}{
							"type":        "boolean",
							"description": "Whether to extract readable content using Readability (default: true)",
							"default":     true,
						},
						"return_html": map[string]interface{}{
							"type":        "boolean",
							"description": "Whether to return raw HTML instead of extracted content (default: false)",
							"default":     false,
						},
					},
					"required": []string{"url"},
				},
			},
			{
				Name:        "web_fetch_batch",
				Description: "Fetches multiple web pages in parallel. Returns results for all URLs.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"urls": map[string]interface{}{
							"type":        "array",
							"items":       map[string]interface{}{"type": "string"},
							"description": "List of URLs to fetch",
						},
						"backend": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"http", "chromedp"},
							"description": "Backend to use for all URLs",
							"default":     "http",
						},
						"extract_content": map[string]interface{}{
							"type":        "boolean",
							"description": "Whether to extract readable content",
							"default":     true,
						},
					},
					"required": []string{"urls"},
				},
			},
		},
	}
}

// ============== Exported C Functions ==============

//export plugin_new
func plugin_new() uintptr {
	p := New()
	pluginsMu.Lock()
	ptr := uintptr(unsafe.Pointer(p))
	plugins[ptr] = p
	pluginsMu.Unlock()
	return ptr
}

//export plugin_name
func plugin_name(ptr unsafe.Pointer) *C.char {
	p := getPlugin(uintptr(ptr))
	if p == nil {
		return C.CString("unknown")
	}
	return C.CString("web-fetch")
}

//export plugin_version
func plugin_version(ptr unsafe.Pointer) *C.char {
	return C.CString("1.0.0")
}

//export plugin_description
func plugin_description(ptr unsafe.Pointer) *C.char {
	return C.CString("Web page fetcher with HTTP and headless browser support, plus readability extraction")
}

//export plugin_type
func plugin_type(ptr unsafe.Pointer) *C.char {
	return C.CString("tool")
}

//export plugin_init
func plugin_init(ptr unsafe.Pointer, configJSON *C.char) {
	p := getPlugin(uintptr(ptr))
	if p == nil || configJSON == nil {
		return
	}

	// Parse JSON config
	config := make(map[string]interface{})
	if goStr := C.GoString(configJSON); goStr != "" && goStr != "{}" {
		if err := json.Unmarshal([]byte(goStr), &config); err != nil {
			return
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Apply configuration
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
}

//export plugin_start
func plugin_start(ptr unsafe.Pointer) {
	// Start is optional - no-op for this plugin
}

//export plugin_stop
func plugin_stop(ptr unsafe.Pointer) {
	// Stop is optional - no-op for this plugin
}

//export plugin_close
func plugin_close(ptr unsafe.Pointer) {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	delete(plugins, uintptr(ptr))
}

//export plugin_tools
func plugin_tools(ptr unsafe.Pointer) *C.char {
	p := getPlugin(uintptr(ptr))
	if p == nil {
		return C.CString("[]")
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	data, err := json.Marshal(p.tools)
	if err != nil {
		return C.CString("[]")
	}
	return C.CString(string(data))
}

//export plugin_execute
func plugin_execute(ptr unsafe.Pointer, toolName *C.char, argsJSON *C.char) *C.char {
	p := getPlugin(uintptr(ptr))
	if p == nil {
		return C.CString(`{"error": "plugin not found"}`)
	}

	tool := C.GoString(toolName)
	argsStr := C.GoString(argsJSON)

	args := make(map[string]interface{})
	if argsStr != "" && argsStr != "{}" {
		if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
			return C.CString(`{"error": "invalid args JSON"}`)
		}
	}

	result, err := p.executeTool(tool, args)
	if err != nil {
		return C.CString(fmt.Sprintf(`{"error": %q}`, err.Error()))
	}

	resultJSON, _ := json.Marshal(result)
	return C.CString(string(resultJSON))
}

// ============== Internal Implementation ==============

func getPlugin(ptr uintptr) *WebFetchPlugin {
	pluginsMu.RLock()
	defer pluginsMu.RUnlock()
	return plugins[ptr]
}

func (p *WebFetchPlugin) executeTool(name string, args map[string]interface{}) (interface{}, error) {
	switch name {
	case "web_fetch":
		return p.handleWebFetch(args)
	case "web_fetch_batch":
		return p.handleWebFetchBatch(args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (p *WebFetchPlugin) handleWebFetch(args map[string]interface{}) (interface{}, error) {
	urlStr, ok := args["url"].(string)
	if !ok || urlStr == "" {
		return nil, fmt.Errorf("url argument is required and must be a string")
	}

	p.mu.RLock()
	backend := p.defaultBackend
	timeout := p.timeout
	maxLen := p.maxContentLength
	p.mu.RUnlock()

	// Override backend if specified
	if b, ok := args["backend"].(string); ok && (b == "http" || b == "chromedp") {
		backend = b
	}

	extractContent := true
	if ec, ok := args["extract_content"].(bool); ok {
		extractContent = ec
	}

	returnHTML := false
	if rh, ok := args["return_html"].(bool); ok {
		returnHTML = rh
	}

	// Fetch the page
	var htmlContent string
	var statusCode int
	var contentType string
	var err error

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	switch backend {
	case "http":
		htmlContent, statusCode, contentType, err = p.fetchWithHTTP(ctx, urlStr)
	case "chromedp":
		htmlContent, statusCode, contentType, err = p.fetchWithChromedp(ctx, urlStr)
	default:
		return nil, fmt.Errorf("unknown backend: %s", backend)
	}

	if err != nil {
		return map[string]interface{}{
			"error":   err.Error(),
			"url":     urlStr,
			"backend": backend,
		}, nil
	}

	// Check content length
	if int64(len(htmlContent)) > maxLen {
		htmlContent = htmlContent[:maxLen]
	}

	result := &FetchResult{
		URL:         urlStr,
		Backend:     backend,
		StatusCode:  statusCode,
		ContentType: contentType,
		Length:      len(htmlContent),
	}

	if returnHTML {
		result.Content = htmlContent
		result.TextContent = htmlContent
		return result, nil
	}

	// Extract content using readability
	if extractContent {
		// Use go-readability for proper content extraction
		reader := strings.NewReader(htmlContent)
		article, err := readability.FromReader(reader, nil)
		if err != nil {
			// Fallback to basic extraction if readability fails
			result.Title = extractTitle(htmlContent)
			result.Content = htmlContent
			result.TextContent = htmlToText(htmlContent)
			result.Excerpt = truncateText(result.TextContent, 200)
		} else {
			// Use readability extracted content
			// In v2, methods return strings and we use RenderHTML/RenderText
			title := article.Title()
			if title != "" {
				result.Title = title
			} else {
				result.Title = extractTitle(htmlContent)
			}
			// Use RenderHTML and RenderText to get the content
			var htmlBuf strings.Builder
			if err := article.RenderHTML(&htmlBuf); err == nil {
				result.Content = htmlBuf.String()
			} else {
				result.Content = htmlContent
			}
			var textBuf strings.Builder
			if err := article.RenderText(&textBuf); err == nil {
				result.TextContent = textBuf.String()
			} else {
				result.TextContent = htmlToText(htmlContent)
			}
			result.Excerpt = article.Excerpt()
			if result.Excerpt == "" {
				result.Excerpt = truncateText(result.TextContent, 200)
			}
		}
	} else {
		result.Content = htmlContent
		result.TextContent = htmlContent
	}

	return result, nil
}

func (p *WebFetchPlugin) handleWebFetchBatch(args map[string]interface{}) (interface{}, error) {
	urlsInterface, ok := args["urls"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("urls argument is required and must be an array")
	}

	var urls []string
	for _, u := range urlsInterface {
		if urlStr, ok := u.(string); ok {
			urls = append(urls, urlStr)
		}
	}

	if len(urls) == 0 {
		return nil, fmt.Errorf("urls array cannot be empty")
	}

	p.mu.RLock()
	backend := p.defaultBackend
	p.mu.RUnlock()

	if b, ok := args["backend"].(string); ok && (b == "http" || b == "chromedp") {
		backend = b
	}

	extractContent := true
	if ec, ok := args["extract_content"].(bool); ok {
		extractContent = ec
	}

	// Fetch in parallel
	results := make([]interface{}, len(urls))
	var wg sync.WaitGroup

	for i, url := range urls {
		wg.Add(1)
		go func(idx int, u string) {
			defer wg.Done()
			fetchArgs := map[string]interface{}{
				"url":             u,
				"backend":         backend,
				"extract_content": extractContent,
				"return_html":     false,
			}
			result, err := p.handleWebFetch(fetchArgs)
			if err != nil {
				results[idx] = map[string]interface{}{
					"error": err.Error(),
					"url":   u,
				}
			} else {
				results[idx] = result
			}
		}(i, url)
	}

	wg.Wait()

	return map[string]interface{}{
		"results": results,
		"count":   len(urls),
	}, nil
}

// fetchWithHTTP fetches a page using standard HTTP client
func (p *WebFetchPlugin) fetchWithHTTP(ctx context.Context, url string) (string, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set a common user agent
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, resp.Header.Get("Content-Type"), fmt.Errorf("failed to read response body: %w", err)
	}

	return string(body), resp.StatusCode, resp.Header.Get("Content-Type"), nil
}

// fetchWithChromedp fetches a page using headless Chrome browser
func (p *WebFetchPlugin) fetchWithChromedp(ctx context.Context, url string) (string, int, string, error) {
	// Create chromedp context
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	var htmlContent string
	var statusCode int

	// Navigate and wait for page to load
	err := chromedp.Run(taskCtx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body"),
		chromedp.OuterHTML("html", &htmlContent),
	)
	if err != nil {
		return "", 0, "", fmt.Errorf("chromedp failed to fetch: %w", err)
	}

	// Get status code by evaluating JavaScript
	var statusJS interface{}
	err = chromedp.Run(taskCtx,
		chromedp.Evaluate(`window.performance.getEntriesByType('navigation')[0]?.responseStatus || 200`, &statusJS),
	)
	if err == nil {
		if status, ok := statusJS.(float64); ok {
			statusCode = int(status)
		}
	}
	if statusCode == 0 {
		statusCode = 200 // Default to 200 if we can't get it
	}

	return htmlContent, statusCode, "text/html; charset=utf-8", nil
}

// extractTitle extracts title from HTML
func extractTitle(html string) string {
	// Simple title extraction
	start := strings.Index(html, "<title>")
	if start == -1 {
		return "Untitled"
	}
	start += 7
	end := strings.Index(html[start:], "</title>")
	if end == -1 {
		return "Untitled"
	}
	return strings.TrimSpace(html[start : start+end])
}

// htmlToText converts HTML to plain text (simplified)
func htmlToText(html string) string {
	// Remove script and style tags
	text := html
	for {
		start := strings.Index(text, "<script")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "</script>")
		if end == -1 {
			break
		}
		text = text[:start] + text[start+end+9:]
	}

	for {
		start := strings.Index(text, "<style")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "</style>")
		if end == -1 {
			break
		}
		text = text[:start] + text[start+end+8:]
	}

	// Remove all HTML tags
	text = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(text, " ")

	// Decode common HTML entities
	text = strings.ReplaceAll(text, "\u0026nbsp;", " ")
	text = strings.ReplaceAll(text, "\u0026amp;", "&")
	text = strings.ReplaceAll(text, "\u0026lt;", "<")
	text = strings.ReplaceAll(text, "\u0026gt;", ">")
	text = strings.ReplaceAll(text, "\u0026quot;", "\"")
	text = strings.ReplaceAll(text, "\u0026#39;", "'")

	// Collapse multiple whitespace
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}

// truncateText truncates text to maxLen characters
func truncateText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// Required for c-shared build
func main() {}
