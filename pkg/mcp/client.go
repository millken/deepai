package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	"github.com/millken/deepai/pkg/models"
)

type Client struct {
	name   string
	client *mcpclient.Client
}

func ConnectStdio(ctx context.Context, name, command string, env []string, args ...string) (*Client, error) {
	transport := mcptransport.NewStdio(command, env, args...)
	client := mcpclient.NewClient(transport)
	return initializeClient(ctx, name, client)
}

func ConnectSSE(ctx context.Context, name, baseURL string, headers map[string]string, headerFunc func(context.Context) map[string]string) (*Client, error) {
	options := make([]mcptransport.ClientOption, 0, 1)
	if len(headers) > 0 {
		options = append(options, mcptransport.WithHeaders(cloneStringMap(headers)))
	}
	if headerFunc != nil {
		options = append(options, mcptransport.WithHeaderFunc(headerFunc))
	}

	client, err := mcpclient.NewSSEMCPClient(baseURL, options...)
	if err != nil {
		return nil, fmt.Errorf("create sse mcp client: %w", err)
	}
	return initializeClient(ctx, name, client)
}

func ConnectHTTP(ctx context.Context, name, baseURL string, headers map[string]string, headerFunc func(context.Context) map[string]string) (*Client, error) {
	options := []mcptransport.StreamableHTTPCOption{mcptransport.WithContinuousListening()}
	if len(headers) > 0 {
		options = append(options, mcptransport.WithHTTPHeaders(cloneStringMap(headers)))
	}
	if headerFunc != nil {
		options = append(options, mcptransport.WithHTTPHeaderFunc(headerFunc))
	}

	client, err := mcpclient.NewStreamableHttpClient(baseURL, options...)
	if err != nil {
		return nil, fmt.Errorf("create http mcp client: %w", err)
	}
	return initializeClient(ctx, name, client)
}

func initializeClient(ctx context.Context, name string, client *mcpclient.Client) (*Client, error) {
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("start mcp transport: %w", err)
	}
	if _, err := client.Initialize(ctx, mcpproto.InitializeRequest{
		Params: mcpproto.InitializeParams{
			ProtocolVersion: mcpproto.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcpproto.Implementation{
				Name:    "deepai-mcp-client",
				Version: "dev",
			},
			Capabilities: mcpproto.ClientCapabilities{},
		},
	}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize mcp client: %w", err)
	}
	return &Client{name: strings.TrimSpace(name), client: client}, nil
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func (c *Client) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

func (c *Client) Tools(ctx context.Context) ([]models.Tool, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("mcp client is not connected")
	}

	result, err := c.client.ListTools(ctx, mcpproto.ListToolsRequest{})
	if err != nil {
		return nil, err
	}

	tools := make([]models.Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		toolName := tool.Name
		if c.name != "" {
			toolName = c.name + "." + toolName
		}
		tools = append(tools, models.Tool{
			Name:        toolName,
			Description: strings.TrimSpace(tool.Description),
			InputSchema: rawSchema(tool),
			Groups:      []string{"mcp", c.name},
			Handler:     c.handler(tool.Name),
		})
	}
	return tools, nil
}

func (c *Client) handler(remoteName string) models.ToolHandler {
	return func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		result, err := c.client.CallTool(ctx, mcpproto.CallToolRequest{
			Params: mcpproto.CallToolParams{
				Name:      remoteName,
				Arguments: call.Arguments,
			},
		})
		if err != nil {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    err.Error(),
			}, err
		}

		content, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    marshalErr.Error(),
			}, marshalErr
		}

		toolResult := models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  string(content),
			Status:   models.CallStatusCompleted,
		}
		if result.IsError {
			toolResult.Status = models.CallStatusFailed
			toolResult.Error = string(content)
			return toolResult, fmt.Errorf("mcp tool %s returned an error", remoteName)
		}
		return toolResult, nil
	}
}

func rawSchema(tool mcpproto.Tool) map[string]any {
	if len(tool.RawInputSchema) == 0 {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil
		}
		var out map[string]any
		if json.Unmarshal(raw, &out) != nil {
			return nil
		}
		return out
	}
	var out map[string]any
	if json.Unmarshal(tool.RawInputSchema, &out) != nil {
		return nil
	}
	return out
}
