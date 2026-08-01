package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/millken/deepai/pkg/models"
)

const (
	defaultWebSearchMaxResults = 5
	defaultWebFetchMaxChars    = 4096
	defaultMaxContentLength    = 1 << 20 // 1MB
	maxWebFetchChars           = 100000  // 100KB upper bound
	maxBatchURLs               = 10
	maxBatchConcurrency        = 5
	defaultWebUserAgent        = "deepai/1.0"
)

var (
	// safeDialer resolves DNS at dial time and rejects private/loopback IPs,
	// preventing both DNS rebinding and direct private-host fetches.
	safeDialer = &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	safeTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %w", addr, err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("host %q is not allowed: %w", host, err)
			}
			for _, ip := range ips {
				if ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsPrivate() {
					return nil, fmt.Errorf("host %q resolved to private address %s", host, ip.IP)
				}
				if ip.IP.To4() != nil && ip.IP.Equal(net.IPv4(169, 254, 169, 254)) {
					return nil, fmt.Errorf("host %q resolved to metadata address", host)
				}
			}
			// Dial with the original addr (hostname) to preserve TLS SNI.
			return safeDialer.DialContext(ctx, network, addr)
		},
	}
	// webFetchClient rejects private hosts at the transport level (DialContext)
	// and blocks redirects to private hosts (CheckRedirect).
	webFetchClient = &http.Client{
		Timeout:   20 * time.Second,
		Transport: safeTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			host := req.URL.Host
			h, _, err := net.SplitHostPort(host)
			if err != nil {
				h = host
			}
			if h == "localhost" || h == "ip6-localhost" || h == "ip6-loopback" {
				return fmt.Errorf("redirect to private host %q is not allowed", host)
			}
			ips, err := net.LookupIP(h)
			if err != nil {
				return fmt.Errorf("redirect host %q is not allowed", host)
			}
			for _, ip := range ips {
				if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
					return fmt.Errorf("redirect to private host %q is not allowed", host)
				}
				if ip.To4() != nil && ip.Equal(net.IPv4(169, 254, 169, 254)) {
					return fmt.Errorf("redirect to metadata address %q is not allowed", host)
				}
			}
			return nil
		},
	}

	webClient               = &http.Client{Timeout: 20 * time.Second}
	duckDuckGoSearchBaseURL = "https://html.duckduckgo.com/html/"
	duckDuckGoPageBaseURL   = "https://duckduckgo.com/"
	duckDuckGoImageAPIURL   = "https://duckduckgo.com/i.js"

	ddgResultAnchorRE  = regexp.MustCompile(`(?is)<a[^>]+(?:class="[^"]*(?:result__a|result-link)[^"]*"|class='[^']*(?:result__a|result-link)[^']*')[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgResultAnchorRE2 = regexp.MustCompile(`(?is)<a[^>]+href="(https?://[^"]+)"[^>]+class="[^"]*(?:result__a|result-link)[^"]*"[^>]*>(.*?)</a>`)
	ddgSnippetRE       = regexp.MustCompile(`(?is)<(?:a|div|span)[^>]+class="[^"]*(?:result__snippet|result-snippet)[^"]*"[^>]*>(.*?)</(?:a|div|span)>`)
	ddgSnippetRE2      = regexp.MustCompile(`(?is)<td[^>]+class="[^"]*(?:result__snippet|result-snippet)[^"]*"[^>]*>(.*?)</td>`)
	ddgImageVQDREs     = []*regexp.Regexp{
		regexp.MustCompile(`vqd=([\w-]+)[&']`),
		regexp.MustCompile(`"vqd":"([^"]+)"`),
		regexp.MustCompile(`vqd='([^']+)'`),
		regexp.MustCompile(`vqd="([^"]+)"`),
	}
	titleTagRE            = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	articleTagRE          = regexp.MustCompile(`(?is)<article\b[^>]*>(.*?)</article>`)
	mainTagRE             = regexp.MustCompile(`(?is)<main\b[^>]*>(.*?)</main>`)
	bodyTagRE             = regexp.MustCompile(`(?is)<body\b[^>]*>(.*?)</body>`)
	sectionTagRE          = regexp.MustCompile(`(?is)<section\b([^>]*)>(.*?)</section>`)
	divTagRE              = regexp.MustCompile(`(?is)<div\b([^>]*)>(.*?)</div>`)
	scriptTagRE           = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleTagRE            = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	noiseTagRE            = regexp.MustCompile(`(?is)<(?:aside|button|dialog|footer|form|header|nav|noscript|script|style|svg)[^>]*>.*?</(?:aside|button|dialog|footer|form|header|nav|noscript|script|style|svg)>`)
	blockTagRE            = regexp.MustCompile(`(?is)</?(?:article|aside|blockquote|br|div|h[1-6]|header|footer|li|main|nav|p|pre|section|tr|table|ul|ol)[^>]*>`)
	anyTagRE              = regexp.MustCompile(`(?is)<[^>]+>`)
	anchorTagRE           = regexp.MustCompile(`(?is)<a\b([^>]*)href\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))([^>]*)>(.*?)</a>`)
	listItemTagRE         = regexp.MustCompile(`(?is)<li\b[^>]*>(.*?)</li>`)
	paragraphTagRE        = regexp.MustCompile(`(?is)<p\b[^>]*>(.*?)</p>`)
	headingTagRE          = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]>`)
	brTagRE               = regexp.MustCompile(`(?is)<br\s*/?>`)
	containerAttrRE       = regexp.MustCompile(`(?i)(?:class|id)\s*=\s*("([^"]*)"|'([^']*)')`)
	negativeContentHintRE = regexp.MustCompile(`(?i)\b(ad|banner|breadcrumb|comment|cookie|footer|header|hero|menu|modal|nav|newsletter|popup|promo|related|share|sidebar|social|subscribe|toolbar)\b`)
	positiveContentHintRE = regexp.MustCompile(`(?i)\b(article|body|content|entry|main|page|post|primary|prose|story|text)\b`)
	spaceRE               = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankLineRE           = regexp.MustCompile(`\n{3,}`)
)

type webSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

type webSearchResponse struct {
	Query        string            `json:"query"`
	TotalResults int               `json:"total_results"`
	Results      []webSearchResult `json:"results"`
}

type imageSearchResult struct {
	Title        string `json:"title"`
	SourceURL    string `json:"source_url"`
	ImageURL     string `json:"image_url"`
	ThumbnailURL string `json:"thumbnail_url"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

type imageSearchResponse struct {
	Query        string              `json:"query"`
	TotalResults int                 `json:"total_results"`
	Results      []imageSearchResult `json:"results"`
	UsageHint    string              `json:"usage_hint,omitempty"`
}

func WebSearchHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	query, ok := call.Arguments["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("query is required")
	}
	query = strings.TrimSpace(query)

	maxResults := defaultWebSearchMaxResults
	if raw, ok := call.Arguments["max_results"].(float64); ok && raw > 0 {
		maxResults = int(raw)
	}
	if maxResults <= 0 {
		maxResults = defaultWebSearchMaxResults
	}
	if maxResults > 10 {
		maxResults = 10
	}

	results, err := searchDuckDuckGo(ctx, query, maxResults)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("web search failed: %w", err)
	}

	body, err := json.Marshal(webSearchResponse{
		Query:        query,
		TotalResults: len(results),
		Results:      results,
	})
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("encode search results: %w", err)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  string(body),
	}, nil
}

func WebFetchHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	rawURL, ok := call.Arguments["url"].(string)
	if !ok || strings.TrimSpace(rawURL) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("url is required")
	}
	rawURL = strings.TrimSpace(rawURL)

	maxChars := defaultWebFetchMaxChars
	if raw, ok := call.Arguments["max_chars"].(float64); ok && raw > 0 {
		maxChars = int(raw)
	}
	if maxChars <= 0 {
		maxChars = defaultWebFetchMaxChars
	}
	if maxChars > maxWebFetchChars {
		maxChars = maxWebFetchChars
	}

	extractContent := true
	if ec, ok := call.Arguments["extract_content"].(bool); ok {
		extractContent = ec
	}

	content, err := fetchWebPage(ctx, rawURL, maxChars, extractContent)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("web fetch failed: %w", err)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  content,
	}, nil
}

func ImageSearchHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	query, ok := call.Arguments["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("query is required")
	}
	query = strings.TrimSpace(query)

	maxResults := defaultWebSearchMaxResults
	if raw, ok := call.Arguments["max_results"].(float64); ok && raw > 0 {
		maxResults = int(raw)
	}
	if maxResults <= 0 {
		maxResults = defaultWebSearchMaxResults
	}
	if maxResults > 10 {
		maxResults = 10
	}

	region := firstNonEmptyString(optionalStringArg(call.Arguments, "region"), "wt-wt")
	size := optionalStringArg(call.Arguments, "size")
	color := optionalStringArg(call.Arguments, "color")
	imageType := optionalStringArg(call.Arguments, "type_image")
	layout := optionalStringArg(call.Arguments, "layout")
	licenseImage := optionalStringArg(call.Arguments, "license_image")

	results, err := searchDuckDuckGoImages(ctx, query, maxResults, region, size, color, imageType, layout, licenseImage)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("image search failed: %w", err)
	}

	body, err := json.Marshal(imageSearchResponse{
		Query:        query,
		TotalResults: len(results),
		Results:      results,
		UsageHint:    "Use the image_url values as visual references before generating images.",
	})
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("encode image results: %w", err)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  string(body),
	}, nil
}

func WebSearchTool() models.Tool {
	return models.Tool{
		Name:         "web_search",
		Description:  "Search the web for current information and return relevant results.",
		Groups:       []string{"builtin", "web"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "Search query"},
				"max_results": map[string]any{"type": "number", "description": "Maximum number of results to return"},
			},
			"required": []any{"query"},
		},
		Handler: WebSearchHandler,
	}
}

func WebFetchTool() models.Tool {
	return models.Tool{
		Name:         "web_fetch",
		Description:  "Fetch the contents of a web page URL and return a readable text summary. Uses readability extraction to get clean article content.",
		Groups:       []string{"builtin", "web"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":             map[string]any{"type": "string", "description": "Exact URL to fetch"},
				"max_chars":       map[string]any{"type": "number", "description": "Maximum characters to return"},
				"extract_content": map[string]any{"type": "boolean", "description": "Whether to extract readable content using Readability (default: true)"},
			},
			"required": []any{"url"},
		},
		Handler: WebFetchHandler,
	}
}

func ImageSearchTool() models.Tool {
	return models.Tool{
		Name:         "image_search",
		Description:  "Search for reference images online and return image URLs plus thumbnails.",
		Groups:       []string{"builtin", "web"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":         map[string]any{"type": "string", "description": "Search keywords for the desired images"},
				"max_results":   map[string]any{"type": "number", "description": "Maximum number of images to return"},
				"region":        map[string]any{"type": "string", "description": "Optional DuckDuckGo region such as wt-wt, us-en, or cn-zh"},
				"size":          map[string]any{"type": "string", "description": "Optional size filter such as Small, Medium, Large, or Wallpaper"},
				"color":         map[string]any{"type": "string", "description": "Optional color filter such as color, monochrome, red, blue, or transparent"},
				"type_image":    map[string]any{"type": "string", "description": "Optional image type filter such as photo, clipart, gif, transparent, or line"},
				"layout":        map[string]any{"type": "string", "description": "Optional layout filter such as Square, Tall, or Wide"},
				"license_image": map[string]any{"type": "string", "description": "Optional license filter such as any, public, share, sharecommercially, modify, or modifycommercially"},
			},
			"required": []any{"query"},
		},
		Handler: ImageSearchHandler,
	}
}

type webFetchBatchResult struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type webFetchBatchResponse struct {
	Results []webFetchBatchResult `json:"results"`
	Count   int                   `json:"count"`
}

func WebFetchBatchHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	urlsRaw, ok := call.Arguments["urls"].([]interface{})
	if !ok {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("urls is required and must be an array")
	}

	var urls []string
	for _, u := range urlsRaw {
		if s, ok := u.(string); ok && strings.TrimSpace(s) != "" {
			urls = append(urls, strings.TrimSpace(s))
		}
	}
	if len(urls) == 0 {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("urls array cannot be empty")
	}
	if len(urls) > maxBatchURLs {
		urls = urls[:maxBatchURLs]
	}

	maxChars := defaultWebFetchMaxChars
	if raw, ok := call.Arguments["max_chars"].(float64); ok && raw > 0 {
		maxChars = int(raw)
	}
	if maxChars <= 0 {
		maxChars = defaultWebFetchMaxChars
	}
	if maxChars > maxWebFetchChars {
		maxChars = maxWebFetchChars
	}

	extractContent := true
	if ec, ok := call.Arguments["extract_content"].(bool); ok {
		extractContent = ec
	}

	results := make([]webFetchBatchResult, len(urls))
	sem := make(chan struct{}, maxBatchConcurrency)
	var wg sync.WaitGroup

	for i, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, urlStr string) {
			defer wg.Done()
			defer func() { <-sem }()
			content, err := fetchWebPage(ctx, urlStr, maxChars, extractContent)
			if err != nil {
				results[idx] = webFetchBatchResult{URL: urlStr, Error: err.Error()}
				return
			}
			results[idx] = webFetchBatchResult{URL: urlStr, Content: content}
		}(i, u)
	}
	wg.Wait()

	body, err := json.Marshal(webFetchBatchResponse{Results: results, Count: len(urls)})
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed}, fmt.Errorf("encode batch results: %w", err)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  string(body),
	}, nil
}

func WebFetchBatchTool() models.Tool {
	return models.Tool{
		Name:         "web_fetch_batch",
		Description:  "Fetches multiple web pages in parallel and returns results for all URLs.",
		Groups:       []string{"builtin", "web"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"urls":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of URLs to fetch"},
				"max_chars":       map[string]any{"type": "number", "description": "Maximum characters to return per URL"},
				"extract_content": map[string]any{"type": "boolean", "description": "Whether to extract readable content (default: true)"},
			},
			"required": []any{"urls"},
		},
		Handler: WebFetchBatchHandler,
	}
}

func WebTools() []models.Tool {
	return []models.Tool{
		WebSearchTool(),
		WebFetchTool(),
		WebFetchBatchTool(),
		ImageSearchTool(),
	}
}

func searchDuckDuckGo(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
	endpoint := duckDuckGoSearchBaseURL + "?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultWebUserAgent)

	resp, err := webClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	return parseDuckDuckGoResults(string(body), maxResults), nil
}

func parseDuckDuckGoResults(body string, maxResults int) []webSearchResult {
	anchors := ddgResultAnchorRE.FindAllStringSubmatch(body, -1)
	if len(anchors) == 0 {
		anchors = ddgResultAnchorRE2.FindAllStringSubmatch(body, -1)
	}
	snippets := ddgSnippetRE.FindAllStringSubmatch(body, -1)
	if len(snippets) == 0 {
		snippets = ddgSnippetRE2.FindAllStringSubmatch(body, -1)
	}
	results := make([]webSearchResult, 0, min(maxResults, len(anchors)))
	seen := make(map[string]struct{}, len(anchors))

	for idx, match := range anchors {
		if len(match) < 3 {
			continue
		}
		link := normalizeDuckDuckGoURL(match[1])
		title := cleanHTMLText(match[2])
		if link == "" || title == "" {
			continue
		}
		if _, ok := seen[link]; ok {
			continue
		}
		seen[link] = struct{}{}

		var snippet string
		if idx < len(snippets) && len(snippets[idx]) >= 2 {
			snippet = cleanHTMLText(snippets[idx][1])
		}
		results = append(results, webSearchResult{
			Title:   title,
			URL:     link,
			Snippet: snippet,
		})
		if len(results) >= maxResults {
			break
		}
	}
	return results
}

// isPrivateHost checks if a hostname is a well-known private name
// without performing DNS resolution. Used for early rejection; the full
// IP-level check is done by safeTransport.DialContext at connection time.
func isPrivateHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return h == "localhost" || h == "ip6-localhost" || h == "ip6-loopback"
}

func fetchWebPage(ctx context.Context, rawURL string, maxChars int, extractContent bool) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("url scheme must be http or https")
	}
	if parsed.Host == "" || isPrivateHost(parsed.Host) {
		return "", fmt.Errorf("url host is not allowed")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", defaultWebUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := webFetchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, defaultMaxContentLength))
	if err != nil {
		return "", err
	}
	htmlContent := string(body)

	if !extractContent {
		return formatPageContent(rawURL, htmlContent, maxChars), nil
	}

	// Try readability extraction first
	title, textContent := extractWithReadability(htmlContent)
	if title == "" {
		title = extractTitle(htmlContent)
	}
	if textContent == "" {
		// Fallback to regex-based extraction
		textContent = markdownifyReadableHTML(parsed.String(), extractPrimaryContent(htmlContent))
	}

	return formatExtractedContent(rawURL, title, textContent, maxChars), nil
}

func searchDuckDuckGoImages(ctx context.Context, query string, maxResults int, region, size, color, imageType, layout, licenseImage string) ([]imageSearchResult, error) {
	vqd, err := fetchDuckDuckGoImageToken(ctx, query)
	if err != nil {
		return nil, err
	}

	endpoint, err := url.Parse(duckDuckGoImageAPIURL)
	if err != nil {
		return nil, err
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("o", "json")
	params.Set("l", normalizedDuckDuckGoRegion(region))
	params.Set("p", "1")
	params.Set("vqd", vqd)
	if filters := duckDuckGoImageFilters(size, color, imageType, layout, licenseImage); filters != "" {
		params.Set("f", filters)
	}
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultWebUserAgent)
	req.Header.Set("Referer", duckDuckGoPageBaseURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := webClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var payload struct {
		Results []struct {
			Title     string `json:"title"`
			Image     string `json:"image"`
			Thumbnail string `json:"thumbnail"`
			URL       string `json:"url"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload); err != nil {
		return nil, err
	}

	results := make([]imageSearchResult, 0, min(maxResults, len(payload.Results)))
	seen := make(map[string]struct{}, len(payload.Results))
	for _, item := range payload.Results {
		imageURL := strings.TrimSpace(item.Image)
		if imageURL == "" {
			imageURL = strings.TrimSpace(item.Thumbnail)
		}
		if imageURL == "" {
			continue
		}
		if _, ok := seen[imageURL]; ok {
			continue
		}
		seen[imageURL] = struct{}{}
		results = append(results, imageSearchResult{
			Title:        cleanHTMLText(item.Title),
			SourceURL:    strings.TrimSpace(item.URL),
			ImageURL:     imageURL,
			ThumbnailURL: firstNonEmptyString(strings.TrimSpace(item.Thumbnail), imageURL),
			Width:        item.Width,
			Height:       item.Height,
		})
		if len(results) >= maxResults {
			break
		}
	}
	return results, nil
}

func fetchDuckDuckGoImageToken(ctx context.Context, query string) (string, error) {
	endpoint, err := url.Parse(duckDuckGoPageBaseURL)
	if err != nil {
		return "", err
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("iax", "images")
	params.Set("ia", "images")
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", defaultWebUserAgent)

	resp, err := webClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	vqd := extractDuckDuckGoImageToken(string(body))
	if vqd == "" {
		return "", fmt.Errorf("image search token not found")
	}
	return vqd, nil
}

func extractDuckDuckGoImageToken(body string) string {
	for _, re := range ddgImageVQDREs {
		match := re.FindStringSubmatch(body)
		if len(match) >= 2 && strings.TrimSpace(match[1]) != "" {
			return strings.TrimSpace(match[1])
		}
	}
	return ""
}

func duckDuckGoImageFilters(size, color, imageType, layout, licenseImage string) string {
	parts := make([]string, 0, 5)
	if value := strings.TrimSpace(size); value != "" {
		parts = append(parts, "size:"+strings.ToLower(value))
	}
	if value := strings.TrimSpace(color); value != "" {
		parts = append(parts, "color:"+strings.ToLower(value))
	}
	if value := strings.TrimSpace(imageType); value != "" {
		parts = append(parts, "type:"+strings.ToLower(value))
	}
	if value := strings.TrimSpace(layout); value != "" {
		parts = append(parts, "layout:"+strings.ToLower(value))
	}
	if value := strings.TrimSpace(licenseImage); value != "" {
		parts = append(parts, "license:"+strings.ToLower(value))
	}
	return strings.Join(parts, ",")
}

func normalizedDuckDuckGoRegion(region string) string {
	region = strings.TrimSpace(strings.ToLower(region))
	if region == "" {
		return "wt-wt"
	}
	return region
}

func optionalStringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func extractReadableContent(pageURL, body string, maxChars int) string {
	title := ""
	if match := titleTagRE.FindStringSubmatch(body); len(match) >= 2 {
		title = cleanHTMLText(match[1])
	}

	text := markdownifyReadableHTML(pageURL, extractPrimaryContent(body))

	var b strings.Builder
	if title != "" {
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
	}
	b.WriteString("Source: ")
	b.WriteString(pageURL)
	if text != "" {
		b.WriteString("\n\n")
		b.WriteString(text)
	}

	content := strings.TrimSpace(b.String())
	if maxChars > 0 && len(content) > maxChars {
		content = strings.TrimSpace(string([]rune(content)[:min(maxChars, len([]rune(content)))]))
	}
	return content
}

func extractWithReadability(htmlContent string) (title, text string) {
	article, err := readability.FromReader(strings.NewReader(htmlContent), nil)
	if err != nil {
		return "", ""
	}
	title = article.Title()
	var textBuf strings.Builder
	if err := article.RenderText(&textBuf); err != nil {
		return title, ""
	}
	return title, textBuf.String()
}

func extractTitle(htmlContent string) string {
	if match := titleTagRE.FindStringSubmatch(htmlContent); len(match) >= 2 {
		return cleanHTMLText(match[1])
	}
	return ""
}

func formatExtractedContent(pageURL, title, textContent string, maxChars int) string {
	var b strings.Builder
	if title != "" {
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
	}
	b.WriteString("Source: ")
	b.WriteString(pageURL)
	if textContent != "" {
		b.WriteString("\n\n")
		b.WriteString(textContent)
	}
	content := strings.TrimSpace(b.String())
	if maxChars > 0 && len([]rune(content)) > maxChars {
		content = strings.TrimSpace(string([]rune(content)[:maxChars]))
	}
	return content
}

func formatPageContent(pageURL, htmlContent string, maxChars int) string {
	text := htmlToText(htmlContent)
	title := extractTitle(htmlContent)
	return formatExtractedContent(pageURL, title, text, maxChars)
}

func htmlToText(htmlContent string) string {
	text := htmlContent
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
	text = anyTagRE.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = spaceRE.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

func extractPrimaryContent(body string) string {
	for _, re := range []*regexp.Regexp{articleTagRE, mainTagRE} {
		if match := re.FindStringSubmatch(body); len(match) >= 2 && strings.TrimSpace(match[1]) != "" {
			return match[1]
		}
	}
	bodyContent := body
	if match := bodyTagRE.FindStringSubmatch(body); len(match) >= 2 && strings.TrimSpace(match[1]) != "" {
		bodyContent = match[1]
	}
	bodyContent = scriptTagRE.ReplaceAllString(bodyContent, " ")
	bodyContent = styleTagRE.ReplaceAllString(bodyContent, " ")
	bodyContent = noiseTagRE.ReplaceAllString(bodyContent, " ")

	best := readableCandidate{html: bodyContent}
	for _, item := range collectContainerCandidates(sectionTagRE, bodyContent) {
		if item.score > best.score {
			best = item
		}
	}
	for _, item := range collectContainerCandidates(divTagRE, bodyContent) {
		if item.score > best.score {
			best = item
		}
	}
	if best.score > 0 && strings.TrimSpace(best.html) != "" {
		return best.html
	}
	return bodyContent
}

type readableCandidate struct {
	score int
	html  string
}

func collectContainerCandidates(re *regexp.Regexp, body string) []readableCandidate {
	matches := re.FindAllStringSubmatch(body, -1)
	out := make([]readableCandidate, 0, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		attrs := strings.TrimSpace(match[1])
		content := strings.TrimSpace(match[2])
		score := scoreReadableContainer(attrs, content)
		if score <= 0 {
			continue
		}
		out = append(out, readableCandidate{
			score: score,
			html:  content,
		})
	}
	return out
}

func scoreReadableContainer(attrs, content string) int {
	content = strings.TrimSpace(content)
	if content == "" {
		return 0
	}
	text := cleanHTMLText(content)
	if len(text) < 120 {
		return 0
	}
	score := len(text)
	score += strings.Count(strings.ToLower(content), "<p") * 160
	score += strings.Count(strings.ToLower(content), "<li") * 80
	score += strings.Count(strings.ToLower(content), "<h") * 40

	attrText := strings.Join(extractContainerHints(attrs), " ")
	if positiveContentHintRE.MatchString(attrText) {
		score += 500
	}
	if negativeContentHintRE.MatchString(attrText) {
		score -= 700
	}
	if negativeContentHintRE.MatchString(text) && len(text) < 320 {
		score -= 300
	}
	return score
}

func extractContainerHints(attrs string) []string {
	matches := containerAttrRE.FindAllStringSubmatch(attrs, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		switch {
		case len(match) >= 3 && strings.TrimSpace(match[2]) != "":
			values = append(values, strings.TrimSpace(match[2]))
		case len(match) >= 4 && strings.TrimSpace(match[3]) != "":
			values = append(values, strings.TrimSpace(match[3]))
		}
	}
	return values
}

func markdownifyReadableHTML(pageURL, rawHTML string) string {
	text := scriptTagRE.ReplaceAllString(rawHTML, " ")
	text = styleTagRE.ReplaceAllString(text, " ")
	text = noiseTagRE.ReplaceAllString(text, " ")
	text = stripNegativeHintContainers(text)
	text = anchorTagRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := anchorTagRE.FindStringSubmatch(match)
		if len(parts) < 8 {
			return cleanHTMLText(match)
		}
		href := firstNonEmptyString(parts[3], parts[4], parts[5])
		label := cleanHTMLText(parts[7])
		if href == "" {
			return label
		}
		if resolved := resolveRelativeURL(pageURL, href); resolved != "" {
			href = resolved
		}
		if label == "" {
			return href
		}
		return "[" + label + "](" + href + ")"
	})
	text = headingTagRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := headingTagRE.FindStringSubmatch(match)
		if len(parts) < 3 {
			return "\n"
		}
		level := 1
		if parsed, err := strconv.Atoi(parts[1]); err == nil && parsed > 0 && parsed <= 6 {
			level = parsed
		}
		label := cleanHTMLText(parts[2])
		if label == "" {
			return "\n"
		}
		return "\n\n" + strings.Repeat("#", level) + " " + label + "\n\n"
	})
	text = paragraphTagRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := paragraphTagRE.FindStringSubmatch(match)
		if len(parts) < 2 {
			return "\n"
		}
		label := cleanHTMLText(parts[1])
		if label == "" {
			return "\n"
		}
		return "\n\n" + label + "\n\n"
	})
	text = listItemTagRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := listItemTagRE.FindStringSubmatch(match)
		if len(parts) < 2 {
			return "\n"
		}
		label := cleanHTMLText(parts[1])
		if label == "" {
			return "\n"
		}
		return "\n- " + label
	})
	text = brTagRE.ReplaceAllString(text, "\n")
	text = blockTagRE.ReplaceAllString(text, "\n")
	text = anyTagRE.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)

	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(spaceRE.ReplaceAllString(line, " "))
		if line == "" {
			continue
		}
		if negativeContentHintRE.MatchString(line) && len(line) <= 80 {
			continue
		}
		filtered = append(filtered, line)
	}
	text = strings.Join(filtered, "\n\n")
	return blankLineRE.ReplaceAllString(strings.TrimSpace(text), "\n\n")
}

func stripNegativeHintContainers(content string) string {
	for _, re := range []*regexp.Regexp{sectionTagRE, divTagRE} {
		content = re.ReplaceAllStringFunc(content, func(match string) string {
			parts := re.FindStringSubmatch(match)
			if len(parts) < 3 {
				return match
			}
			if negativeContentHintRE.MatchString(strings.Join(extractContainerHints(parts[1]), " ")) {
				return " "
			}
			return match
		})
	}
	return content
}

func resolveRelativeURL(pageURL, raw string) string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" {
		return ""
	}
	target, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if target.IsAbs() {
		return target.String()
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return raw
	}
	return base.ResolveReference(target).String()
}

func normalizeDuckDuckGoURL(raw string) string {
	raw = html.UnescapeString(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.Path == "/l/" || parsed.Path == "/l" {
		if uddg := parsed.Query().Get("uddg"); uddg != "" {
			decoded, err := url.QueryUnescape(uddg)
			if err == nil {
				return decoded
			}
			return uddg
		}
	}
	return raw
}

func cleanHTMLText(value string) string {
	value = anyTagRE.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	value = strings.TrimSpace(spaceRE.ReplaceAllString(value, " "))
	return value
}
