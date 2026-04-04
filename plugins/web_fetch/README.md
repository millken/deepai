# Web Fetch Plugin

A powerful web page fetcher plugin with multiple backends and readability extraction, designed as an MCP (Model Context Protocol) compatible tool.

## Features

- **HTTP Backend**: Fast fetching for static pages using standard Go HTTP client
- **Chromedp Backend**: Full headless Chrome browser for JavaScript-rendered pages
- **Readability Extraction**: Clean article content extraction using go-readability v2
- **Batch Fetching**: Fetch multiple URLs in parallel for efficiency
- **MCP Compatible**: Can be used as an MCP tool in AI agent workflows

## Installation

Build the plugin as a shared library:

```bash
cd plugins/web_fetch
go mod tidy
go build -buildmode=c-shared -o web_fetch.so web_fetch.go
```

## Tools Provided

### web_fetch

Fetch a single web page with customizable options.

**Input Schema:**
```json
{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The URL to fetch"
    },
    "backend": {
      "type": "string",
      "enum": ["http", "chromedp"],
      "description": "Backend to use: 'http' for static pages, 'chromedp' for dynamic JS-rendered pages",
      "default": "http"
    },
    "extract_content": {
      "type": "boolean",
      "description": "Whether to extract readable content using Readability (default: true)",
      "default": true
    },
    "return_html": {
      "type": "boolean",
      "description": "Whether to return raw HTML instead of extracted content (default: false)",
      "default": false
    }
  },
  "required": ["url"]
}
```

**Example Usage:**
```json
{
  "url": "https://example.com/article",
  "backend": "http",
  "extract_content": true
}
```

**Response:**
```json
{
  "url": "https://example.com/article",
  "title": "Article Title",
  "content": "<p>Clean HTML content...</p>",
  "text_content": "Plain text content...",
  "excerpt": "Short excerpt...",
  "backend": "http",
  "status_code": 200,
  "content_type": "text/html; charset=utf-8",
  "length": 12345
}
```

### web_fetch_batch

Fetch multiple web pages in parallel.

**Input Schema:**
```json
{
  "type": "object",
  "properties": {
    "urls": {
      "type": "array",
      "items": {"type": "string"},
      "description": "List of URLs to fetch"
    },
    "backend": {
      "type": "string",
      "enum": ["http", "chromedp"],
      "description": "Backend to use for all URLs",
      "default": "http"
    },
    "extract_content": {
      "type": "boolean",
      "description": "Whether to extract readable content",
      "default": true
    }
  },
  "required": ["urls"]
}
```

**Example Usage:**
```json
{
  "urls": [
    "https://example.com/article1",
    "https://example.com/article2"
  ],
  "backend": "http",
  "extract_content": true
}
```

## Configuration

The plugin accepts the following configuration options when initialized:

```json
{
  "default_backend": "http",
  "timeout": 30,
  "max_content_length": 1000000
}
```

- `default_backend`: Default backend to use ("http" or "chromedp")
- `timeout`: Request timeout in seconds (default: 30)
- `max_content_length`: Maximum content length in bytes (default: 1MB)

## Backends

### HTTP Backend

- Fast and lightweight
- Uses standard Go HTTP client
- Sets common browser User-Agent headers
- Suitable for static HTML pages

### Chromedp Backend

- Full headless Chrome browser
- Executes JavaScript
- Renders dynamic content
- Captures JavaScript-rendered HTML
- Note: Requires Chrome/Chromium installed on the system

## Content Extraction

The plugin uses [go-readability v2](https://codeberg.org/readeck/go-readability) for intelligent content extraction:

- Removes navigation, ads, and clutter
- Extracts main article content
- Provides clean HTML and plain text versions
- Generates article excerpts
- Falls back to basic extraction if readability fails

## Integration with DeepAI

This plugin can be used as a tool in the DeepAI agent system. When loaded, it provides:

1. `web_fetch` tool for single page fetching
2. `web_fetch_batch` tool for parallel batch fetching

These tools can be invoked by AI agents to retrieve web content for analysis, summarization, or information extraction tasks.

## Dependencies

- `codeberg.org/readeck/go-readability/v2` v2.1.1
- `github.com/chromedp/chromedp` v0.13.6
- `golang.org/x/net` (transitive)

## License

Part of the DeepAI project.