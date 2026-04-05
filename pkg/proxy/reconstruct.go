package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// reconstructStreamBody converts raw SSE bytes into a single valid JSON response.
// For non-streaming or unrecognized data, returns the input unchanged.
func reconstructStreamBody(data []byte, apiFormat string) []byte {
	switch apiFormat {
	case "anthropic":
		return reconstructAnthropicStream(data)
	case "openai":
		return reconstructOpenAIStream(data)
	}
	return data
}

// reconstructAnthropicStream merges Anthropic SSE events into a single message object.
func reconstructAnthropicStream(data []byte) []byte {
	msg := &anthropicMessage{
		Type:    "message",
		Role:    "assistant",
		Content: []json.RawMessage{},
	}

	var currentBlock *anthropicContentBlock

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := []byte(strings.TrimPrefix(line, "data: "))
		if string(payload) == "[DONE]" {
			break
		}

		var evt struct {
			Type  string          `json:"type"`
			Index int             `json:"index"`
			Delta json.RawMessage `json:"delta"`
			Msg   struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Role  string `json:"role"`
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			ContentBlock json.RawMessage `json:"content_block"`
		}
		if json.Unmarshal(payload, &evt) != nil {
			continue
		}

		switch evt.Type {
		case "message_start":
			msg.ID = evt.Msg.ID
			msg.Model = evt.Msg.Model
			msg.Role = evt.Msg.Role
			if evt.Msg.Usage.InputTokens > 0 {
				msg.Usage.InputTokens = evt.Msg.Usage.InputTokens
			}
			if evt.Msg.Usage.OutputTokens > 0 {
				msg.Usage.OutputTokens = evt.Msg.Usage.OutputTokens
			}

		case "content_block_start":
			currentBlock = &anthropicContentBlock{Index: evt.Index}
			var cb struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Thinking  string `json:"thinking"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Input     any    `json:"input"`
				Signature string `json:"signature"`
			}
			if json.Unmarshal(evt.ContentBlock, &cb) == nil {
				currentBlock.Type = cb.Type
				currentBlock.Text = cb.Text
				currentBlock.Thinking = cb.Thinking
				currentBlock.ID = cb.ID
				currentBlock.Name = cb.Name
				currentBlock.Signature = cb.Signature
			}

		case "content_block_delta":
			if currentBlock == nil {
				continue
			}
			var delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			}
			if json.Unmarshal(evt.Delta, &delta) != nil {
				continue
			}
			switch delta.Type {
			case "text_delta":
				currentBlock.Text += delta.Text
			case "thinking_delta":
				currentBlock.Thinking += delta.Thinking
			case "input_json_delta":
				currentBlock.PartialInput += delta.PartialJSON
			case "signature_delta":
				currentBlock.Signature += delta.Signature
			}

		case "content_block_stop":
			if currentBlock != nil {
				msg.Content = append(msg.Content, currentBlock.ToJSON())
				currentBlock = nil
			}

		case "message_delta":
			var md struct {
				StopReason   string `json:"stop_reason"`
				StopSequence string `json:"stop_sequence"`
			}
			if json.Unmarshal(evt.Delta, &md) == nil {
				if md.StopReason != "" {
					msg.StopReason = md.StopReason
				}
				if md.StopSequence != "" {
					msg.StopSequence = md.StopSequence
				}
			}
			if evt.Usage.OutputTokens > msg.Usage.OutputTokens {
				msg.Usage.OutputTokens = evt.Usage.OutputTokens
			}
			if evt.Usage.InputTokens > msg.Usage.InputTokens {
				msg.Usage.InputTokens = evt.Usage.InputTokens
			}

		case "message_stop":
			// end of stream
		}
	}

	// Flush any unclosed block.
	if currentBlock != nil {
		msg.Content = append(msg.Content, currentBlock.ToJSON())
	}

	out, err := json.Marshal(msg)
	if err != nil {
		return data
	}
	return out
}

type anthropicMessage struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   string            `json:"stop_reason,omitempty"`
	StopSequence string            `json:"stop_sequence,omitempty"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicContentBlock struct {
	Index        int
	Type         string
	Text         string
	Thinking     string
	Signature    string
	ID           string // tool_use id
	Name         string // tool_use name
	PartialInput string
}

func (b *anthropicContentBlock) ToJSON() json.RawMessage {
	switch b.Type {
	case "thinking":
		v, _ := json.Marshal(map[string]string{
			"type":      "thinking",
			"thinking":  b.Thinking,
			"signature": b.Signature,
		})
		return v
	case "text":
		v, _ := json.Marshal(map[string]string{
			"type": "text",
			"text": b.Text,
		})
		return v
	case "tool_use":
		m := map[string]any{
			"type":  "tool_use",
			"id":    b.ID,
			"name":  b.Name,
			"input": map[string]any{},
		}
		if b.PartialInput != "" {
			var input any
			if json.Unmarshal([]byte(b.PartialInput), &input) == nil {
				m["input"] = input
			} else {
				m["input"] = json.RawMessage(b.PartialInput)
			}
		}
		v, _ := json.Marshal(m)
		return v
	default:
		v, _ := json.Marshal(map[string]string{"type": b.Type})
		return v
	}
}

// reconstructOpenAIStream merges OpenAI SSE events into a single response object.
func reconstructOpenAIStream(data []byte) []byte {
	resp := &openAIMessage{
		Role: "assistant",
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := []byte(strings.TrimPrefix(line, "data: "))
		if string(payload) == "[DONE]" {
			break
		}

		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Index        int             `json:"index"`
				Delta        json.RawMessage `json:"delta"`
				FinishReason *string         `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &chunk) != nil {
			continue
		}

		if chunk.ID != "" {
			resp.ID = chunk.ID
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if len(chunk.Choices) > 0 {
			c := &chunk.Choices[0]
			var delta struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			}
			if json.Unmarshal(c.Delta, &delta) == nil {
				if delta.Role != "" {
					resp.Role = delta.Role
				}
				resp.Content += delta.Content
				if delta.Refusal != "" {
					resp.Refusal = delta.Refusal
				}
				for _, tc := range delta.ToolCalls {
					for len(resp.ToolCalls) <= tc.Index {
						resp.ToolCalls = append(resp.ToolCalls, openAIToolCall{})
					}
					entry := &resp.ToolCalls[tc.Index]
					if tc.ID != "" {
						entry.ID = tc.ID
					}
					if tc.Type != "" {
						entry.Type = tc.Type
					}
					if tc.Function.Name != "" {
						entry.Function.Name = tc.Function.Name
					}
					entry.Function.Arguments += tc.Function.Arguments
				}
			}
			if c.FinishReason != nil {
				resp.FinishReason = *c.FinishReason
			}
		}
		if chunk.Usage != nil {
			resp.Usage.PromptTokens = chunk.Usage.PromptTokens
			resp.Usage.CompletionTokens = chunk.Usage.CompletionTokens
			resp.Usage.TotalTokens = chunk.Usage.TotalTokens
		}
	}

	msg := map[string]any{
		"role":    resp.Role,
		"content": resp.Content,
	}
	if len(resp.ToolCalls) > 0 {
		msg["tool_calls"] = resp.ToolCalls
	}
	if resp.Refusal != "" {
		msg["refusal"] = resp.Refusal
	}

	out, err := json.Marshal(map[string]any{
		"id":     resp.ID,
		"object": "chat.completion",
		"model":  resp.Model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": resp.FinishReason,
			},
		},
		"usage": resp.Usage,
	})
	if err != nil {
		return data
	}
	return out
}

type openAIMessage struct {
	ID           string
	Model        string
	Role         string
	Content      string
	Refusal      string
	FinishReason string
	ToolCalls    []openAIToolCall
	Usage        struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
