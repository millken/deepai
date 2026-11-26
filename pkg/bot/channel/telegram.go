package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/bot"
)

// TelegramConfig configures the Telegram Long Polling adapter (no public IP required).
type TelegramConfig struct {
	Token      string
	BotName    string // optional @username for group mention detection
	APIBaseURL string // default "https://api.telegram.org"
	HTTPClient *http.Client
}

type TelegramAdapter struct {
	token   string
	botName string
	baseURL string
	client  *http.Client
}

func NewTelegramAdapter(cfg TelegramConfig) *TelegramAdapter {
	base := strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 45 * time.Second}
	}
	return &TelegramAdapter{
		token:   strings.TrimSpace(cfg.Token),
		botName: strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cfg.BotName)), "@"),
		baseURL: base,
		client:  c,
	}
}

func (t *TelegramAdapter) Name() string { return "telegram" }

func (t *TelegramAdapter) apiURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", t.baseURL, t.token, method)
}

// redactErr strips the Telegram bot token from transport errors (such as
// *url.Error carrying the full request URL) before returning or logging.
func (t *TelegramAdapter) redactErr(err error) error {
	if err == nil || t.token == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, t.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, t.token, "[REDACTED]"))
}

func (t *TelegramAdapter) Start(ctx context.Context, onInbound func(InboundMessage)) error {
	if t.token == "" {
		return fmt.Errorf("telegram: bot token is required")
	}
	var offset int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := t.getUpdates(ctx, offset, 25)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("channel: telegram getUpdates failed", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if msg, ok := t.toInbound(ctx, u); ok {
				onInbound(msg)
			}
		}
	}
}

type tgUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message,omitempty"`
	CallbackQuery *tgCallbackQuery `json:"callback_query,omitempty"`
}

type tgMessage struct {
	MessageID       int64       `json:"message_id"`
	MessageThreadID int64       `json:"message_thread_id,omitempty"`
	From            *tgUser     `json:"from,omitempty"`
	Chat            tgChat      `json:"chat"`
	Text            string      `json:"text,omitempty"`
	Caption         string      `json:"caption,omitempty"`
	Document        *tgDocument `json:"document,omitempty"`
	Photo           []tgPhoto   `json:"photo,omitempty"`
}

type tgUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private" | "group" | "supergroup"
}

type tgDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

type tgPhoto struct {
	FileID string `json:"file_id"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message,omitempty"`
	Data    string     `json:"data,omitempty"`
}

func (t *TelegramAdapter) getUpdates(ctx context.Context, offset int64, timeoutSec int) ([]tgUpdate, error) {
	url := fmt.Sprintf("%s?timeout=%d&offset=%d", t.apiURL("getUpdates"), timeoutSec, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, t.redactErr(err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.redactErr(err)
	}
	defer resp.Body.Close()
	var env struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if !env.OK {
		return nil, fmt.Errorf("telegram getUpdates returned ok=false (status %d)", resp.StatusCode)
	}
	return env.Result, nil
}

func (t *TelegramAdapter) toInbound(ctx context.Context, u tgUpdate) (InboundMessage, bool) {
	if u.CallbackQuery != nil && u.CallbackQuery.Message != nil {
		// Best-effort ack to dismiss Telegram's loading spinner.
		_ = t.postJSON(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": u.CallbackQuery.ID}, nil)
		m := u.CallbackQuery.Message
		return InboundMessage{
			Channel:      "telegram",
			UserID:       strconv.FormatInt(u.CallbackQuery.From.ID, 10),
			ChatID:       strconv.FormatInt(m.Chat.ID, 10),
			ThreadID:     threadStr(m.MessageThreadID),
			MessageID:    strconv.FormatInt(m.MessageID, 10),
			IsGroup:      m.Chat.Type != "private",
			Mentioned:    true,
			CallbackData: u.CallbackQuery.Data,
		}, true
	}
	if u.Message == nil {
		return InboundMessage{}, false
	}
	m := u.Message
	uid := ""
	if m.From != nil {
		uid = strconv.FormatInt(m.From.ID, 10)
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		text = strings.TrimSpace(m.Caption)
	}
	isGroup := m.Chat.Type != "private"
	mentioned := !isGroup
	if isGroup {
		lower := strings.ToLower(text)
		if t.botName != "" && strings.Contains(lower, "@"+t.botName) {
			mentioned = true
		} else if fields := strings.Fields(text); len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
			cmdTok := strings.ToLower(fields[0])
			if at := strings.Index(cmdTok, "@"); at > 0 {
				targetBot := cmdTok[at+1:]
				if t.botName != "" && targetBot == t.botName && isKnownSlashCommand(cmdTok[:at]) {
					mentioned = true
				}
			} else if isKnownSlashCommand(cmdTok) {
				mentioned = true
			}
		}
	}

	// Defer file downloads until Gateway has checked AccessPolicy and group mention.
	var atts []InboundAttachment
	if m.Document != nil && m.Document.FileID != "" {
		fileID := m.Document.FileID
		name := m.Document.FileName
		if name == "" {
			name = "document"
		}
		atts = append(atts, InboundAttachment{
			Name:     name,
			MimeType: m.Document.MimeType,
			Download: func(dlCtx context.Context) ([]byte, error) {
				return t.downloadFile(dlCtx, fileID)
			},
		})
	} else if len(m.Photo) > 0 {
		fileID := m.Photo[len(m.Photo)-1].FileID
		atts = append(atts, InboundAttachment{
			Name:     "photo.jpg",
			MimeType: "image/jpeg",
			Download: func(dlCtx context.Context) ([]byte, error) {
				return t.downloadFile(dlCtx, fileID)
			},
		})
	}

	return InboundMessage{
		Channel:     "telegram",
		UserID:      uid,
		ChatID:      strconv.FormatInt(m.Chat.ID, 10),
		ThreadID:    threadStr(m.MessageThreadID),
		MessageID:   strconv.FormatInt(m.MessageID, 10),
		IsGroup:     isGroup,
		Mentioned:   mentioned,
		Text:        text,
		Attachments: atts,
	}, true
}

func (t *TelegramAdapter) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	var res struct {
		FilePath string `json:"file_path"`
	}
	if err := t.postJSON(ctx, "getFile", map[string]any{"file_id": fileID}, &res); err != nil {
		return nil, err
	}
	if res.FilePath == "" {
		return nil, fmt.Errorf("empty file_path")
	}
	url := fmt.Sprintf("%s/file/bot%s/%s", t.baseURL, t.token, strings.TrimPrefix(res.FilePath, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, t.redactErr(err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.redactErr(err)
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, bot.MaxAttachmentBytes+1))
}

func (t *TelegramAdapter) SendText(ctx context.Context, dst Destination, text string) (string, error) {
	payload := map[string]any{"chat_id": dst.ChatID, "text": text}
	if dst.ThreadID != "" {
		payload["message_thread_id"] = dst.ThreadID
	}
	var out struct {
		MessageID int64 `json:"message_id"`
	}
	if err := t.postJSON(ctx, "sendMessage", payload, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.MessageID, 10), nil
}

func (t *TelegramAdapter) EditText(ctx context.Context, dst Destination, msgID, text string) error {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	return t.postJSON(ctx, "editMessageText", map[string]any{
		"chat_id":    dst.ChatID,
		"message_id": id,
		"text":       text,
	}, nil)
}

func (t *TelegramAdapter) DeleteMessage(ctx context.Context, dst Destination, msgID string) error {
	id, err := strconv.ParseInt(msgID, 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("invalid telegram message_id %q", msgID)
	}
	return t.postJSON(ctx, "deleteMessage", map[string]any{
		"chat_id":    dst.ChatID,
		"message_id": id,
	}, nil)
}

func (t *TelegramAdapter) SendQuestion(ctx context.Context, dst Destination, q bot.Question) (string, error) {
	payload := map[string]any{
		"chat_id": dst.ChatID,
		"text":    "❓ " + q.Text,
	}
	if dst.ThreadID != "" {
		payload["message_thread_id"] = dst.ThreadID
	}
	if len(q.Options) > 0 {
		var rows [][]map[string]string
		for idx, opt := range q.Options {
			cb := "ans:" + q.ID + ":" + opt
			if len(cb) > 64 {
				// Use option index to stay within Telegram's 64-byte callback_data
				// limit without truncating multi-byte UTF-8 option text.
				cb = fmt.Sprintf("ans:%s:#%d", q.ID, idx)
			}
			rows = append(rows, []map[string]string{{"text": opt, "callback_data": cb}})
		}
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	var out struct {
		MessageID int64 `json:"message_id"`
	}
	if err := t.postJSON(ctx, "sendMessage", payload, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.MessageID, 10), nil
}

func (t *TelegramAdapter) SendAttachment(ctx context.Context, dst Destination, filePath, caption string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chat_id", dst.ChatID)
	if dst.ThreadID != "" {
		_ = mw.WriteField("message_thread_id", dst.ThreadID)
	}
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiURL("sendDocument"), &body)
	if err != nil {
		return t.redactErr(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := t.client.Do(req)
	if err != nil {
		return t.redactErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram sendDocument status %d", resp.StatusCode)
	}
	return nil
}

func (t *TelegramAdapter) postJSON(ctx context.Context, method string, payload any, dst any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiURL(method), bytes.NewReader(raw))
	if err != nil {
		return t.redactErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return t.redactErr(err)
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description,omitempty"`
		Result      json.RawMessage `json:"result,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if dst != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, dst)
	}
	return nil
}

func threadStr(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func isKnownSlashCommand(cmd string) bool {
	switch cmd {
	case "/new", "/cancel", "/steer", "/bot", "/model", "/status", "/approve", "/secret":
		return true
	}
	return false
}
