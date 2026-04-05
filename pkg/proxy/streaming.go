package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxStreamBufferSize = 10 << 20 // 10 MB

// streamingRecorder wraps http.ResponseWriter to simultaneously forward
// SSE chunks to the client and buffer them for post-stream analysis.
type streamingRecorder struct {
	http.ResponseWriter
	mu        sync.Mutex
	buf       bytes.Buffer
	firstByte time.Time
	started   time.Time
	truncated bool
}

func newStreamingRecorder(w http.ResponseWriter, started time.Time) *streamingRecorder {
	return &streamingRecorder{
		ResponseWriter: w,
		started:        started,
	}
}

func (s *streamingRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.firstByte.IsZero() {
		s.firstByte = time.Now()
	}
	if !s.truncated && s.buf.Len()+len(p) <= maxStreamBufferSize {
		s.buf.Write(p)
	} else if !s.truncated {
		s.truncated = true
	}
	s.mu.Unlock()
	return s.ResponseWriter.Write(p)
}

func (s *streamingRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// extractStreamEvents parses buffered SSE data into delta + usage events.
// apiFormat is "openai" or "anthropic" — determined from the request path, no guessing.
// Timestamps are NOT set on delta/usage events; the caller (handleStreamingProxy) sets
// them appropriately since we don't have real per-chunk timing from the buffered data.
func extractStreamEvents(data []byte, apiFormat, id, requestID string) ([]LogEvent, int, error) {
	var events []LogEvent
	var chunkCount int

	// Use a 1 MB scanner buffer so that individual SSE data lines up to 1 MB
	// are handled correctly. The default 64 KB limit would silently drop
	// chunks containing long tool-call arguments or large metadata payloads.
	const maxScanLineSize = 1 << 20 // 1 MB
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, maxScanLineSize), maxScanLineSize)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}

		raw := []byte(payload)

		switch apiFormat {
		case "openai":
			text, usage := parseOpenAIChunk(raw)
			if text != "" {
				chunkCount++
			}
			if usage != nil {
				events = append(events, LogEvent{
					Type:         EventUsage,
					ID:           id,
					InputTokens:  usage.promptTokens,
					OutputTokens: usage.completionTokens,
					TotalTokens:  usage.totalTokens,
				})
			}
		case "anthropic":
			text, usage := parseAnthropicChunk(raw)
			if text != "" {
				chunkCount++
			}
			if usage != nil {
				events = append(events, LogEvent{
					Type:         EventUsage,
					ID:           id,
					InputTokens:  usage.inputTokens,
					OutputTokens: usage.outputTokens,
				})
			}
		}
	}

	return events, chunkCount, scanner.Err()
}

type openAIUsage struct {
	promptTokens     int
	completionTokens int
	totalTokens      int
}

// parseOpenAIChunk extracts text delta and usage from an OpenAI SSE chunk.
func parseOpenAIChunk(raw []byte) (string, *openAIUsage) {
	var v struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return "", nil
	}

	var text string
	if len(v.Choices) > 0 {
		text = v.Choices[0].Delta.Content
	}

	var usage *openAIUsage
	if v.Usage != nil {
		usage = &openAIUsage{
			promptTokens:     v.Usage.PromptTokens,
			completionTokens: v.Usage.CompletionTokens,
			totalTokens:      v.Usage.TotalTokens,
		}
	}
	return text, usage
}

type anthropicUsage struct {
	inputTokens  int
	outputTokens int
}

// parseAnthropicChunk extracts text delta and usage from an Anthropic SSE chunk.
func parseAnthropicChunk(raw []byte) (string, *anthropicUsage) {
	var v struct {
		Type  string `json:"type"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Message struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return "", nil
	}

	var text string
	if v.Type == "content_block_delta" && (v.Delta.Text != "" || v.Delta.Thinking != "") {
		if v.Delta.Text != "" {
			text = v.Delta.Text
		} else {
			text = v.Delta.Thinking
		}
	}

	var usage *anthropicUsage
	if v.Usage.InputTokens > 0 || v.Usage.OutputTokens > 0 {
		usage = &anthropicUsage{
			inputTokens:  v.Usage.InputTokens,
			outputTokens: v.Usage.OutputTokens,
		}
	}
	if v.Message.Usage.OutputTokens > 0 {
		if usage == nil {
			usage = &anthropicUsage{}
		}
		if v.Message.Usage.OutputTokens > usage.outputTokens {
			usage.outputTokens = v.Message.Usage.OutputTokens
		}
	}

	return text, usage
}

// handleStreamingProxy forwards an SSE stream from upstream to the client,
// then emits delta + usage + done events after the stream completes.
func (p *Proxy) handleStreamingProxy(
	w http.ResponseWriter,
	upstreamReq *http.Request,
	id string,
	startEvt, reqBodyEvt LogEvent,
	started time.Time,
) {
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

	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	recorder := newStreamingRecorder(w, started)
	_, copyErr := io.Copy(recorder, resp.Body)

	recorder.mu.Lock()
	bufData := make([]byte, recorder.buf.Len())
	copy(bufData, recorder.buf.Bytes())
	firstByte := recorder.firstByte
	recorder.mu.Unlock()

	apiFormat := startEvt.APIFormat
	streamEvents, chunkCount, parseErr := extractStreamEvents(bufData, apiFormat, id, id)

	var ttfbStr string
	if !firstByte.IsZero() {
		ttfbStr = firstByte.Sub(started).Round(time.Millisecond).String()
	}

	doneEvt := LogEvent{
		Timestamp:  time.Now().UTC(),
		Type:       EventDone,
		ID:         id,
		StatusCode: resp.StatusCode,
		Duration:   time.Since(started).Round(time.Millisecond).String(),
		TTFB:       ttfbStr,
		ChunkCount: chunkCount,
	}
	if copyErr != nil {
		doneEvt.Error = copyErr.Error()
	}
	// Attach SSE parse error to done event for observability, but don't fail the request.
	if parseErr != nil && doneEvt.Error == "" {
		doneEvt.Error = "stream_parse: " + parseErr.Error()
	}

	// Reconstruct raw SSE into a single valid JSON for storage.
	reconstructed := reconstructStreamBody(bufData, apiFormat)

	// Build complete event batch: start + req_body + resp_body + deltas + usage + done.
	ts := time.Now().UTC()
	respBodyEvt := LogEvent{
		Timestamp: ts,
		Type:      EventRespBody,
		ID:        id,
		Body:      RawBody(reconstructed),
		BodySize:  len(reconstructed),
	}
	allEvents := make([]LogEvent, 0, 3+len(streamEvents)+1)
	allEvents = append(allEvents, startEvt, reqBodyEvt, respBodyEvt)
	for i := range streamEvents {
		streamEvents[i].Timestamp = ts
		allEvents = append(allEvents, streamEvents[i])
	}
	allEvents = append(allEvents, doneEvt)

	p.emitEvents(allEvents)
}
