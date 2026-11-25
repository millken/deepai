package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"golang.org/x/net/websocket"
)

// FeishuConfig configures the Feishu (Lark) adapter (docs/BOT_NEXT_DESIGN.md §5.4).
type FeishuConfig struct {
	AppID          string
	AppSecret      string
	APIBaseURL     string // default "https://open.feishu.cn"
	EventStreamURL string // optional WS endpoint (default resolved via callback/ws/endpoint)
	HTTPClient     *http.Client
}

type FeishuAdapter struct {
	appID      string
	appSecret  string
	baseURL    string
	streamURL  string
	client     *http.Client
	mu         sync.Mutex
	token      string
	tokenUntil time.Time
}

func NewFeishuAdapter(cfg FeishuConfig) *FeishuAdapter {
	base := strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if base == "" {
		base = "https://open.feishu.cn"
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &FeishuAdapter{
		appID:     strings.TrimSpace(cfg.AppID),
		appSecret: strings.TrimSpace(cfg.AppSecret),
		baseURL:   base,
		streamURL: strings.TrimSpace(cfg.EventStreamURL),
		client:    c,
	}
}

func (f *FeishuAdapter) Name() string { return "feishu" }

// Start connects to the configured Feishu WS event stream (or waits for ctx
// cancellation if only outbound/webhook mode is used).
func (f *FeishuAdapter) Start(ctx context.Context, onInbound func(InboundMessage)) error {
	if f.appID == "" || f.appSecret == "" {
		return fmt.Errorf("feishu: app_id and app_secret are required")
	}
	wsURL := f.streamURL
	if wsURL == "" {
		var err error
		wsURL, err = f.resolveWSEndpoint(ctx)
		if err != nil {
			return err
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := f.runWSOnce(ctx, wsURL, onInbound); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
}

func (f *FeishuAdapter) resolveWSEndpoint(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"AppID": f.appID, "AppSecret": f.appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/callback/ws/endpoint", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			URL string `json:"URL"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", err
	}
	if env.Code != 0 || env.Data.URL == "" {
		return "", fmt.Errorf("feishu ws endpoint error (%d): %s", env.Code, env.Msg)
	}
	return env.Data.URL, nil
}

type feishuEventEnvelope struct {
	Header struct {
		EventType string `json:"event_type"`
	} `json:"header"`
	Event struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
		Message struct {
			MessageID   string `json:"message_id"`
			ChatID      string `json:"chat_id"`
			ThreadID    string `json:"thread_id"`
			ChatType    string `json:"chat_type"` // "p2p" | "group"
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
			Mentions    []any  `json:"mentions"`
		} `json:"message"`
		Action *struct {
			Value map[string]string `json:"value"`
		} `json:"action,omitempty"`
	} `json:"event"`
}

func (f *FeishuAdapter) runWSOnce(ctx context.Context, wsURL string, onInbound func(InboundMessage)) error {
	conn, err := websocket.Dial(wsURL, "", f.baseURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	for {
		var raw []byte
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			return err
		}
		if msg, ok := ParseFeishuEvent(raw); ok {
			onInbound(msg)
		}
	}
}

// ParseFeishuEvent normalizes a Feishu v2 message or card-action JSON event.
func ParseFeishuEvent(raw []byte) (InboundMessage, bool) {
	var env feishuEventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return InboundMessage{}, false
	}
	if env.Event.Action != nil {
		if cb := env.Event.Action.Value["callback_data"]; cb != "" {
			return InboundMessage{
				Channel:      "feishu",
				UserID:       env.Event.Sender.SenderID.OpenID,
				ChatID:       env.Event.Message.ChatID,
				ThreadID:     env.Event.Message.ThreadID,
				Mentioned:    true,
				CallbackData: cb,
			}, true
		}
	}
	if env.Header.EventType != "im.message.receive_v1" {
		return InboundMessage{}, false
	}
	m := env.Event.Message
	var contentObj struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(m.Content), &contentObj)
	isGroup := m.ChatType != "" && m.ChatType != "p2p"
	return InboundMessage{
		Channel:   "feishu",
		UserID:    env.Event.Sender.SenderID.OpenID,
		ChatID:    m.ChatID,
		ThreadID:  m.ThreadID,
		MessageID: m.MessageID,
		IsGroup:   isGroup,
		Mentioned: !isGroup || len(m.Mentions) > 0,
		Text:      strings.TrimSpace(contentObj.Text),
	}, true
}

func (f *FeishuAdapter) tenantToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	if f.token != "" && time.Now().Before(f.tokenUntil) {
		tok := f.token
		f.mu.Unlock()
		return tok, nil
	}
	f.mu.Unlock()

	raw, _ := json.Marshal(map[string]string{"app_id": f.appID, "app_secret": f.appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var res struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.Code != 0 || res.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu auth (%d): %s", res.Code, res.Msg)
	}
	ttl := time.Duration(max(60, res.Expire-120)) * time.Second
	f.mu.Lock()
	f.token = res.TenantAccessToken
	f.tokenUntil = time.Now().Add(ttl)
	f.mu.Unlock()
	return res.TenantAccessToken, nil
}

func (f *FeishuAdapter) SendText(ctx context.Context, dst Destination, text string) (string, error) {
	content, _ := json.Marshal(map[string]string{"text": text})
	return f.sendMessage(ctx, dst.ChatID, "text", string(content))
}

func (f *FeishuAdapter) EditText(ctx context.Context, _ Destination, msgID, text string) error {
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return err
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]string{"msg_type": "text", "content": string(content)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, f.baseURL+"/open-apis/im/v1/messages/"+msgID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("feishu edit status %d", resp.StatusCode)
	}
	return nil
}

func (f *FeishuAdapter) SendQuestion(ctx context.Context, dst Destination, q bot.Question) (string, error) {
	actions := make([]map[string]any, 0, len(q.Options))
	for _, opt := range q.Options {
		actions = append(actions, map[string]any{
			"tag":  "button",
			"text": map[string]string{"tag": "plain_text", "content": opt},
			"type": "default",
			"value": map[string]string{
				"callback_data": "ans:" + q.ID + ":" + opt,
			},
		})
	}
	card := map[string]any{
		"header": map[string]any{
			"title": map[string]string{"tag": "plain_text", "content": "DeepAI 需要确认"},
		},
		"elements": []any{
			map[string]any{"tag": "markdown", "content": q.Text},
			map[string]any{"tag": "action", "actions": actions},
		},
	}
	rawCard, _ := json.Marshal(card)
	return f.sendMessage(ctx, dst.ChatID, "interactive", string(rawCard))
}

func (f *FeishuAdapter) SendAttachment(ctx context.Context, dst Destination, filePath, _ string) error {
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return err
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("file_type", "stream")
	_ = mw.WriteField("file_name", filepath.Base(filePath))
	fw, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, file); err != nil {
		return err
	}
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/open-apis/im/v1/files", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var up struct {
		Code int `json:"code"`
		Data struct {
			FileKey string `json:"file_key"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil {
		return err
	}
	if up.Code != 0 || up.Data.FileKey == "" {
		return fmt.Errorf("feishu file upload failed (code=%d)", up.Code)
	}
	content, _ := json.Marshal(map[string]string{"file_key": up.Data.FileKey})
	_, err = f.sendMessage(ctx, dst.ChatID, "file", string(content))
	return err
}

func (f *FeishuAdapter) sendMessage(ctx context.Context, chatID, msgType, content string) (string, error) {
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{
		"receive_id": chatID,
		"msg_type":   msgType,
		"content":    content,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/open-apis/im/v1/messages?receive_id_type=chat_id", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("feishu sendMessage (%d): %s", res.Code, res.Msg)
	}
	return res.Data.MessageID, nil
}
