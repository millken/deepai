package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// handleHealth returns a simple health check response.
func (p *Proxy) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		p.logger.Printf("health encode error: %v", err)
	}
}

// readBodyLimited reads body with truncation detection.
// Uses LimitReader(N+1): if we read more than maxBytes, the body was truncated.
// Returns the (possibly trimmed) body, the original size read, and whether truncation occurred.
func readBodyLimited(r io.Reader, maxBytes int64) (body []byte, totalRead int, truncated bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, 0, false, err
	}
	totalRead = len(body)
	if int64(totalRead) > maxBytes {
		truncated = true
		body = body[:maxBytes]
	}
	return body, totalRead, truncated, nil
}

// handleProxy is the core reverse-proxy handler that logs all traffic as events.
func (p *Proxy) handleProxy(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	id := requestIDFromCtx(r.Context())
	apiFormat := detectAPIFormat(r)

	// Read request body with truncation detection.
	defer r.Body.Close()
	body, reqBodySize, reqTruncated, err := readBodyLimited(r.Body, p.cfg.MaxRequestBody)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	parsed := parseRequestBody(body)
	upstreamURL := p.upstreamURL(apiFormat, r.URL.Path)
	sessionID := r.Header.Get("X-Claude-Code-Session-Id")

	now := started.UTC()

	// Emit start event.
	startEvt := LogEvent{
		Timestamp:    now,
		Type:         EventStart,
		ID:           id,
		Method:       r.Method,
		Path:         r.URL.Path,
		APIFormat:    apiFormat,
		Model:        parsed.model,
		Streaming:    parsed.stream,
		UpstreamAddr: upstreamURL,
		ClientID:     sessionID,
	}

	// Emit request body event.
	reqBodyEvt := LogEvent{
		Timestamp: now,
		Type:      EventReqBody,
		ID:        id,
		Body:      RawBody(body),
		BodySize:  reqBodySize,
		Truncated: reqTruncated,
	}

	// Build upstream request.
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		reqBodyEvt.Timestamp = time.Now().UTC()
		p.emitEvents([]LogEvent{startEvt, reqBodyEvt, {
			Timestamp: time.Now().UTC(),
			Type:      EventDone,
			ID:        id,
			Error:     err.Error(),
		}})
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	copyRequestHeaders(upstreamReq, r)
	p.injectAuth(upstreamReq, apiFormat)

	if parsed.stream {
		p.handleStreamingProxy(w, upstreamReq, id, startEvt, reqBodyEvt, started)
		return
	}

	// Non-streaming: forward and capture response.
	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		p.emitEvents([]LogEvent{startEvt, reqBodyEvt, {
			Timestamp: time.Now().UTC(),
			Type:      EventDone,
			ID:        id,
			Duration:  time.Since(started).Round(time.Millisecond).String(),
			Error:     err.Error(),
		}})
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, respBodySize, respTruncated, err := readBodyLimited(resp.Body, p.cfg.MaxRequestBody)
	if err != nil {
		p.emitEvents([]LogEvent{startEvt, reqBodyEvt, {
			Timestamp:  time.Now().UTC(),
			Type:       EventDone,
			ID:         id,
			StatusCode: resp.StatusCode,
			Duration:   time.Since(started).Round(time.Millisecond).String(),
			Error:      err.Error(),
		}})
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	// Copy response to client.
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
		p.logger.Printf("write response error request_id=%s err=%v", id, err)
	}

	// Emit response body + done events.
	p.emitEvents([]LogEvent{
		startEvt,
		reqBodyEvt,
		{
			Timestamp: time.Now().UTC(),
			Type:      EventRespBody,
			ID:        id,
			Body:      RawBody(respBody),
			BodySize:  respBodySize,
			Truncated: respTruncated,
		},
		{
			Timestamp:  time.Now().UTC(),
			Type:       EventDone,
			ID:         id,
			StatusCode: resp.StatusCode,
			Duration:   time.Since(started).Round(time.Millisecond).String(),
		},
	})
}

// detectAPIFormat determines whether the request targets OpenAI or Anthropic API.
func detectAPIFormat(r *http.Request) string {
	path := r.URL.Path
	if strings.Contains(path, "/v1/messages") {
		return "anthropic"
	}
	return "openai"
}

// requestBodyParams holds parsed fields from the request JSON.
type requestBodyParams struct {
	model  string
	stream bool
}

// parseRequestBody extracts model and stream flag in a single JSON parse.
func parseRequestBody(body []byte) requestBodyParams {
	var v struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	json.Unmarshal(body, &v)
	return requestBodyParams{model: v.Model, stream: v.Stream}
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"TE":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Content-Encoding":    true, // body is decompressed by Go http client
	"Content-Length":      true, // length may differ after decompression/reconstruction
}

func (p *Proxy) injectAuth(req *http.Request, apiFormat string) {
	key := p.upstreamAPIKey(apiFormat)
	if key == "" {
		return
	}
	switch apiFormat {
	case "anthropic":
		req.Header.Set("x-api-key", key)
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func copyRequestHeaders(dst *http.Request, src *http.Request) {
	for _, h := range []string{
		"Content-Type",
		"Accept",
		"User-Agent",
		"anthropic-version",
		"anthropic-beta",
	} {
		if v := src.Header.Get(h); v != "" {
			dst.Header.Set(h, v)
		}
	}
}

func copyResponseHeaders(dst http.ResponseWriter, src *http.Response) {
	for k, vv := range src.Header {
		if hopByHopHeaders[k] {
			continue
		}
		for _, v := range vv {
			dst.Header().Add(k, v)
		}
	}
}
