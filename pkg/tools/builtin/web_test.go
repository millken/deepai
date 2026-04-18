package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func stubHTTPClient(t *testing.T, fn roundTripFunc) func() {
	t.Helper()
	oldClient := webClient
	oldFetchClient := webFetchClient
	webClient = &http.Client{Transport: fn}
	webFetchClient = &http.Client{
		Timeout:   oldFetchClient.Timeout,
		Transport: fn,
	}
	return func() {
		webClient = oldClient
		webFetchClient = oldFetchClient
	}
}

func newHTTPResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header(textproto.MIMEHeader{"Content-Type": {"text/html; charset=utf-8"}}),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestParseDuckDuckGoResults(t *testing.T) {
	body := `
<html><body>
  <a class="result__a" href="https://example.com/alpha">Alpha Result</a>
  <a class="result__snippet">First snippet</a>
  <a class="result-link" href="https://example.com/beta">Beta Result</a>
  <div class="result-snippet">Second snippet</div>
</body></html>`

	results := parseDuckDuckGoResults(body, 5)
	if len(results) != 2 {
		t.Fatalf("len(results)=%d want 2", len(results))
	}
	if results[0].Title != "Alpha Result" || results[0].URL != "https://example.com/alpha" {
		t.Fatalf("first result=%#v", results[0])
	}
	if results[0].Snippet != "First snippet" {
		t.Fatalf("first snippet=%q want %q", results[0].Snippet, "First snippet")
	}
	if results[1].Title != "Beta Result" || results[1].Snippet != "Second snippet" {
		t.Fatalf("second result=%#v", results[1])
	}
}

func TestWebSearchHandler(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		if got := r.URL.Query().Get("q"); got != "golang" {
			t.Fatalf("query=%q want %q", got, "golang")
		}
		return newHTTPResponse(r, `
<a class="result__a" href="https://example.com/one">Result One</a>
<a class="result__snippet">Snippet One</a>
<a class="result__a" href="https://example.com/two">Result Two</a>
<a class="result__snippet">Snippet Two</a>`), nil
	})
	defer restore()

	oldBaseURL := duckDuckGoSearchBaseURL
	duckDuckGoSearchBaseURL = "https://search.local/html/"
	defer func() { duckDuckGoSearchBaseURL = oldBaseURL }()

	result, err := WebSearchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-search-1",
		Name: "web_search",
		Arguments: map[string]any{
			"query":       "golang",
			"max_results": float64(2),
		},
	})
	if err != nil {
		t.Fatalf("WebSearchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, `"query":"golang"`) {
		t.Fatalf("content=%q missing query", result.Content)
	}
	if !strings.Contains(result.Content, `"total_results":2`) {
		t.Fatalf("content=%q missing total_results", result.Content)
	}
	if !strings.Contains(result.Content, `"results":[`) {
		t.Fatalf("content=%q missing results array", result.Content)
	}
	if !strings.Contains(result.Content, `"title":"Result One"`) || !strings.Contains(result.Content, `"title":"Result Two"`) {
		t.Fatalf("content=%q missing result", result.Content)
	}
	if !strings.Contains(result.Content, `"snippet":"Snippet One"`) || !strings.Contains(result.Content, `"snippet":"Snippet Two"`) {
		t.Fatalf("content=%q missing snippet field", result.Content)
	}
	if !strings.Contains(result.Content, `"snippet":"Snippet One"`) || !strings.Contains(result.Content, `"snippet":"Snippet Two"`) {
		t.Fatalf("content=%q missing snippet field", result.Content)
	}
}

func TestIssue2139WebSearchHandlerReturnsDiagnosableError(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup html.duckduckgo.com: no such host")
	})
	defer restore()

	oldBaseURL := duckDuckGoSearchBaseURL
	duckDuckGoSearchBaseURL = "https://search.local/html/"
	defer func() { duckDuckGoSearchBaseURL = oldBaseURL }()

	_, err := WebSearchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-search-error",
		Name: "web_search",
		Arguments: map[string]any{
			"query": "today openai api pricing",
		},
	})
	if err == nil {
		t.Fatal("WebSearchHandler() error = nil want diagnostic failure")
	}
	if !strings.Contains(err.Error(), "web search failed:") {
		t.Fatalf("error=%q want wrapped search failure prefix", err.Error())
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("error=%q want underlying provider/network detail", err.Error())
	}
}

func TestLiveIssue2139WebSearchHandlerReachesNetwork(t *testing.T) {
	if os.Getenv("DEERFLOW_LIVE_BEHAVIOR") == "" {
		t.Skip("set DEERFLOW_LIVE_BEHAVIOR=1 to run live web search smoke")
	}

	result, err := WebSearchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-search-live",
		Name: "web_search",
		Arguments: map[string]any{
			"query":       "site:openai.com OpenAI API pricing",
			"max_results": float64(5),
		},
	})
	if err != nil {
		t.Fatalf("WebSearchHandler() live error = %v", err)
	}

	var payload webSearchResponse
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode live search payload: %v", err)
	}
	if payload.TotalResults == 0 || len(payload.Results) == 0 {
		t.Fatalf("live search returned no results: %#v", payload)
	}

	foundOfficial := false
	for _, item := range payload.Results {
		if strings.Contains(strings.ToLower(item.URL), "openai.com") {
			foundOfficial = true
			break
		}
	}
	if !foundOfficial {
		t.Fatalf("live search results=%#v want at least one openai.com result", payload.Results)
	}
}

func TestWebFetchHandler(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return newHTTPResponse(r, `<!doctype html>
<html>
  <head>
    <title>Sample Page</title>
    <style>.hidden { display: none; }</style>
  </head>
  <body>
    <article>
      <h1>Headline</h1>
      <p>Important content.</p>
      <script>console.log("ignore");</script>
    </article>
  </body>
</html>`), nil
	})
	defer restore()

	result, err := WebFetchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-fetch-1",
		Name: "web_fetch",
		Arguments: map[string]any{
			"url":       "https://example.com/article",
			"max_chars": float64(200),
		},
	})
	if err != nil {
		t.Fatalf("WebFetchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "# Sample Page") {
		t.Fatalf("content=%q missing title", result.Content)
	}
	if strings.Contains(result.Content, "Site navigation") {
		t.Fatalf("content=%q should prefer primary article content", result.Content)
	}
	if !strings.Contains(result.Content, "Important content.") {
		t.Fatalf("content=%q missing article text", result.Content)
	}
	if strings.Contains(result.Content, "console.log") {
		t.Fatalf("content=%q should not include script", result.Content)
	}
}

func TestWebFetchPrefersMainContent(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return newHTTPResponse(r, `<!doctype html>
<html>
  <head><title>Long Page</title></head>
  <body>
    <nav>Global navigation</nav>
    <main>
      <h1>Main Story</h1>
      <p>Key facts only.</p>
    </main>
    <footer>Footer links</footer>
  </body>
</html>`), nil
	})
	defer restore()

	result, err := WebFetchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-fetch-main",
		Name: "web_fetch",
		Arguments: map[string]any{
			"url": "https://example.com/main",
		},
	})
	if err != nil {
		t.Fatalf("WebFetchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "Main Story") || !strings.Contains(result.Content, "Key facts only.") {
		t.Fatalf("content=%q missing main content", result.Content)
	}
}

func TestWebFetchScoresReadableContainerOverCookieBanner(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return newHTTPResponse(r, `<!doctype html>
<html>
  <head><title>Launch Notes</title></head>
  <body>
    <div class="cookie-banner">
      <p>Accept cookies to continue browsing this website.</p>
    </div>
    <div class="page-shell">
      <div class="sidebar">Share this story</div>
      <div class="article-content">
        <h2>Launch plan</h2>
        <p>Ship the migration in three phases.</p>
        <p>Validate metrics after each phase before continuing.</p>
      </div>
    </div>
  </body>
</html>`), nil
	})
	defer restore()

	result, err := WebFetchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-fetch-readable-container",
		Name: "web_fetch",
		Arguments: map[string]any{
			"url": "https://example.com/launch",
		},
	})
	if err != nil {
		t.Fatalf("WebFetchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "Launch plan") || !strings.Contains(result.Content, "Ship the migration in three phases.") {
		t.Fatalf("content=%q missing article body", result.Content)
	}
	if strings.Contains(result.Content, "Accept cookies") || strings.Contains(result.Content, "Share this story") {
		t.Fatalf("content=%q should exclude banner/sidebar noise", result.Content)
	}
}

func TestWebFetchExtractsReadableContent(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return newHTTPResponse(r, `<!doctype html>
<html>
  <head><title>Guide</title></head>
  <body>
    <article>
      <h2>Checklist</h2>
      <p>Read the <a href="/docs/start">quickstart guide</a> first.</p>
      <ul>
        <li>Review requirements</li>
        <li>Run validation</li>
      </ul>
    </article>
  </body>
</html>`), nil
	})
	defer restore()

	result, err := WebFetchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-fetch-readable",
		Name: "web_fetch",
		Arguments: map[string]any{
			"url": "https://example.com/guide",
		},
	})
	if err != nil {
		t.Fatalf("WebFetchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "Checklist") {
		t.Fatalf("content=%q missing heading", result.Content)
	}
	if !strings.Contains(result.Content, "quickstart guide") {
		t.Fatalf("content=%q missing link text", result.Content)
	}
	if !strings.Contains(result.Content, "Review requirements") || !strings.Contains(result.Content, "Run validation") {
		t.Fatalf("content=%q missing list items", result.Content)
	}
}

func TestSearchDuckDuckGoReturnsParsedResults(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return newHTTPResponse(r, `<a class="result__a" href="https://example.com/article">Example Article</a><a class="result__snippet">Useful summary</a>`), nil
	})
	defer restore()

	oldBaseURL := duckDuckGoSearchBaseURL
	duckDuckGoSearchBaseURL = "https://search.local/html/"
	defer func() { duckDuckGoSearchBaseURL = oldBaseURL }()

	results, err := searchDuckDuckGo(context.Background(), "test", 5)
	if err != nil {
		t.Fatalf("searchDuckDuckGo() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results)=%d want 1", len(results))
	}
	if results[0].URL != "https://example.com/article" {
		t.Fatalf("url=%q want %q", results[0].URL, "https://example.com/article")
	}
}

func TestWebSearchUsesUserAgent(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("User-Agent"); got != defaultWebUserAgent {
			t.Fatalf("user-agent=%q want %q", got, defaultWebUserAgent)
		}
		return newHTTPResponse(r, `<a class="result__a" href="https://example.com/x">X</a>`), nil
	})
	defer restore()

	oldBaseURL := duckDuckGoSearchBaseURL
	duckDuckGoSearchBaseURL = "https://search.local/html/"
	defer func() { duckDuckGoSearchBaseURL = oldBaseURL }()

	if _, err := searchDuckDuckGo(context.Background(), "query", 1); err != nil {
		t.Fatalf("searchDuckDuckGo() error = %v", err)
	}
}

func TestExtractDuckDuckGoImageToken(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "json field",
			body: `{"vqd":"3-12345678901234567890123456789012"}`,
			want: "3-12345678901234567890123456789012",
		},
		{
			name: "query parameter",
			body: `https://duckduckgo.com/i.js?vqd=4-abcdef&foo=bar`,
			want: "4-abcdef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractDuckDuckGoImageToken(tt.body); got != tt.want {
				t.Fatalf("extractDuckDuckGoImageToken()=%q want %q", got, tt.want)
			}
		})
	}
}

func TestImageSearchHandler(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.String(), duckDuckGoImageAPIURL):
			if got := r.URL.Query().Get("vqd"); got != "3-abc123" {
				t.Fatalf("image token=%q want %q", got, "3-abc123")
			}
			if got := r.URL.Query().Get("l"); got != "us-en" {
				t.Fatalf("region=%q want %q", got, "us-en")
			}
			if got := r.URL.Query().Get("f"); got != "size:large,color:monochrome,type:photo,layout:wide,license:share" {
				t.Fatalf("filters=%q want %q", got, "size:large,color:monochrome,type:photo,layout:wide,license:share")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header(textproto.MIMEHeader{"Content-Type": {"application/json"}}),
				Body: io.NopCloser(strings.NewReader(`{
					"results": [
						{
							"title": "Retro Robot",
							"image": "https://img.example.com/full.jpg",
							"thumbnail": "https://img.example.com/thumb.jpg",
							"url": "https://source.example.com/robot",
							"width": 1024,
							"height": 768
						}
					]
				}`)),
				Request: r,
			}, nil
		case strings.HasPrefix(r.URL.String(), duckDuckGoPageBaseURL):
			if got := r.URL.Query().Get("q"); got != "retro robot illustration" {
				t.Fatalf("page query=%q want %q", got, "retro robot illustration")
			}
			return newHTTPResponse(r, `<html><script>var cfg={"vqd":"3-abc123"}</script></html>`), nil
		default:
			t.Fatalf("unexpected request URL %q", r.URL.String())
			return nil, nil
		}
	})
	defer restore()

	result, err := ImageSearchHandler(context.Background(), models.ToolCall{
		ID:   "call-image-search-1",
		Name: "image_search",
		Arguments: map[string]any{
			"query":         "retro robot illustration",
			"max_results":   float64(1),
			"region":        "us-en",
			"size":          "Large",
			"color":         "Monochrome",
			"type_image":    "photo",
			"layout":        "Wide",
			"license_image": "share",
		},
	})
	if err != nil {
		t.Fatalf("ImageSearchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, `"query":"retro robot illustration"`) {
		t.Fatalf("content=%q missing query", result.Content)
	}
	if !strings.Contains(result.Content, `"image_url":"https://img.example.com/full.jpg"`) {
		t.Fatalf("content=%q missing image_url", result.Content)
	}
	if !strings.Contains(result.Content, `"thumbnail_url":"https://img.example.com/thumb.jpg"`) {
		t.Fatalf("content=%q missing thumbnail_url", result.Content)
	}
	if !strings.Contains(result.Content, `"source_url":"https://source.example.com/robot"`) {
		t.Fatalf("content=%q missing source_url", result.Content)
	}
}

func TestDuckDuckGoImageFilters(t *testing.T) {
	got := duckDuckGoImageFilters("Large", "Monochrome", "photo", "Wide", "share")
	want := "size:large,color:monochrome,type:photo,layout:wide,license:share"
	if got != want {
		t.Fatalf("duckDuckGoImageFilters()=%q want %q", got, want)
	}
}

func TestWebFetchBatchHandler(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return newHTTPResponse(r, `<!doctype html><html><head><title>Batch Page</title></head><body><p>Batch content for `+r.URL.String()+`</p></body></html>`), nil
	})
	defer restore()

	result, err := WebFetchBatchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-fetch-batch-1",
		Name: "web_fetch_batch",
		Arguments: map[string]any{
			"urls": []interface{}{
				"https://example.com/page1",
				"https://example.com/page2",
			},
		},
	})
	if err != nil {
		t.Fatalf("WebFetchBatchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, `"count":2`) {
		t.Fatalf("content=%q missing count", result.Content)
	}
	if !strings.Contains(result.Content, `"results":[`) {
		t.Fatalf("content=%q missing results array", result.Content)
	}
	if !strings.Contains(result.Content, "Batch Page") {
		t.Fatalf("content=%q missing page content", result.Content)
	}
}

func TestWebFetchBatchHandlerEmptyURLs(t *testing.T) {
	_, err := WebFetchBatchHandler(context.Background(), models.ToolCall{
		ID:   "call-web-fetch-batch-empty",
		Name: "web_fetch_batch",
		Arguments: map[string]any{
			"urls": []interface{}{},
		},
	})
	if err == nil {
		t.Fatal("WebFetchBatchHandler() error = nil want empty urls error")
	}
}

func TestWebFetchRejectsPrivateHost(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"localhost", "http://localhost/path"},
		{"localhost with port", "http://localhost:8080/path"},
		{"127.0.0.1", "http://127.0.0.1/path"},
		{"10.x", "http://10.0.0.1/path"},
		{"192.168.x", "http://192.168.1.1/path"},
		{"172.16.x", "http://172.16.0.1/path"},
		{"169.254 metadata", "http://169.254.169.254/latest/meta-data/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := WebFetchHandler(context.Background(), models.ToolCall{
				ID:   "call-ssrf",
				Name: "web_fetch",
				Arguments: map[string]any{
					"url": tt.url,
				},
			})
			if err == nil {
				t.Fatalf("expected error for %s", tt.url)
			}
			// Error message depends on whether caught by pre-check or transport
			errMsg := err.Error()
			if !strings.Contains(errMsg, "not allowed") && !strings.Contains(errMsg, "private address") && !strings.Contains(errMsg, "metadata address") {
				t.Fatalf("error=%q want SSRF rejection", errMsg)
			}
		})
	}
}

func TestWebFetchBatchTruncatesURLs(t *testing.T) {
	restore := stubHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		return newHTTPResponse(r, `<!doctype html><html><head><title>OK</title></head><body><p>content</p></body></html>`), nil
	})
	defer restore()

	urls := make([]interface{}, 20)
	for i := range urls {
		urls[i] = "https://example.com/page"
	}
	result, err := WebFetchBatchHandler(context.Background(), models.ToolCall{
		ID:   "call-batch-truncate",
		Name: "web_fetch_batch",
		Arguments: map[string]any{
			"urls": urls,
		},
	})
	if err != nil {
		t.Fatalf("WebFetchBatchHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, `"count":10`) {
		t.Fatalf("content=%q want count=10 (truncated from 20)", result.Content)
	}
}

func TestIsPrivateHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost:8080", true},
		{"ip6-localhost", true},
		{"ip6-loopback", true},
		{"127.0.0.1", false}, // IP literals are handled by safeTransport, not isPrivateHost
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"example.com", false},
		{"google.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := isPrivateHost(tt.host)
			if got != tt.want {
				t.Fatalf("isPrivateHost(%q)=%v want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestNormalizedDuckDuckGoRegionDefaults(t *testing.T) {
	if got := normalizedDuckDuckGoRegion(""); got != "wt-wt" {
		t.Fatalf("normalizedDuckDuckGoRegion(\"\")=%q want %q", got, "wt-wt")
	}
	if got := normalizedDuckDuckGoRegion(" US-EN "); got != "us-en" {
		t.Fatalf("normalizedDuckDuckGoRegion(\" US-EN \")=%q want %q", got, "us-en")
	}
}
