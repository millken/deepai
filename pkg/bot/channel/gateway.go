package channel

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/schedule"
)

// GatewayOptions configures the IM Channel Gateway.
type GatewayOptions struct {
	Manager   *bot.Manager
	Schedules *schedule.Service
	Bindings  BindingStore
	Audit     *bot.AuditLogger
	Logger    *slog.Logger
}

type registeredAdapter struct {
	adapter Adapter
	policy  AccessPolicy
}

// Gateway bridges registered IM Adapters onto a single bot.Manager.
type Gateway struct {
	m         *bot.Manager
	schedules *schedule.Service
	bindings  BindingStore
	audit     *bot.AuditLogger
	log       *slog.Logger

	mu       sync.Mutex
	adapters map[string]registeredAdapter
	streams  map[string]struct{} // active sessionID -> streaming goroutine exists
	wg       sync.WaitGroup
}

func NewGateway(opts GatewayOptions) *Gateway {
	bindings := opts.Bindings
	if bindings == nil {
		bindings = NewMemoryBindingStore()
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		m:         opts.Manager,
		schedules: opts.Schedules,
		bindings:  bindings,
		audit:     opts.Audit,
		log:       log,
		adapters:  map[string]registeredAdapter{},
		streams:   map[string]struct{}{},
	}
}

// Register attaches an IM Adapter with its AccessPolicy.
func (g *Gateway) Register(a Adapter, policy AccessPolicy) {
	if a == nil {
		return
	}
	g.mu.Lock()
	g.adapters[a.Name()] = registeredAdapter{adapter: a, policy: policy}
	g.mu.Unlock()
}

// Start launches all registered adapters in background goroutines until ctx is
// cancelled. Call Wait() on shutdown to drain active message handlers.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	list := make([]registeredAdapter, 0, len(g.adapters))
	for _, ra := range g.adapters {
		list = append(list, ra)
	}
	g.mu.Unlock()

	for _, ra := range list {
		ra := ra
		if len(ra.policy.AllowUsers) == 0 && len(ra.policy.AllowChats) == 0 {
			g.log.Warn("channel: allowlist is empty; all inbound messages will be rejected", "channel", ra.adapter.Name())
		}
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			if err := ra.adapter.Start(ctx, func(msg InboundMessage) {
				g.HandleInbound(ctx, msg)
			}); err != nil && ctx.Err() == nil {
				g.log.Error("channel: adapter stopped with error", "channel", ra.adapter.Name(), "err", err)
			}
		}()
	}
	return nil
}

// Wait blocks until all adapter and streaming goroutines exit.
func (g *Gateway) Wait() { g.wg.Wait() }

// HandleInbound processes one normalized IM message (exported so tests and
// webhook-based adapters can inject messages directly).
func (g *Gateway) HandleInbound(ctx context.Context, msg InboundMessage) {
	g.mu.Lock()
	ra, ok := g.adapters[msg.Channel]
	g.mu.Unlock()
	if !ok {
		return
	}
	if !ra.policy.Allowed(msg.UserID, msg.ChatID) {
		g.log.Warn("channel: rejected unauthorized message",
			"channel", msg.Channel, "user_id", msg.UserID, "chat_id", msg.ChatID)
		g.audit.Record(bot.AuditEntry{
			Action:  "channel_rejected",
			Actor:   msg.Channel + ":" + msg.UserID,
			Target:  msg.ChatID,
			Outcome: "denied",
			Detail:  "not in allowlist",
		})
		return
	}
	// Group chats only respond when explicitly mentioned or on button callbacks.
	if msg.IsGroup && !msg.Mentioned && msg.CallbackData == "" {
		return
	}

	dst := Destination{ChatID: msg.ChatID, ThreadID: msg.ThreadID, ReplyToID: msg.MessageID}
	text := strings.TrimSpace(msg.Text)

	// 1. Handle inline button callback for ask_clarification ("ans:<qid>:<answer>").
	if strings.HasPrefix(msg.CallbackData, "ans:") {
		g.handleQuestionCallback(ctx, ra, dst, msg)
		return
	}

	// 2. Built-in slash commands (docs/BOT_NEXT_DESIGN.md §5.3).
	if strings.HasPrefix(text, "/") {
		if g.handleSlashCommand(ctx, ra, dst, msg, text) {
			return
		}
	}

	// 3. Resolve or create bound (bot_id, session_id).
	binding, err := g.ensureBinding(ctx, ra.policy, msg.Channel, msg.ChatID, msg.ThreadID)
	if err != nil {
		_, _ = ra.adapter.SendText(ctx, dst, "会话初始化失败："+err.Error())
		return
	}

	// If session currently has a pending question and user sent plain text, treat as answer.
	if info, _, err := g.m.GetSessionForBot(binding.BotID, binding.SessionID); err == nil && len(info.Pending) > 0 && text != "" && len(msg.Attachments) == 0 {
		q := info.Pending[0]
		if err := g.m.AnswerForBot(binding.BotID, binding.SessionID, q.ID, text); err == nil {
			_, _ = ra.adapter.SendText(ctx, dst, "已提交回答："+text)
			return
		}
	}

	// 4. Lazy-download (after auth/mention) and upload any inbound attachments via M3 pipeline.
	var attIDs []string
	for _, att := range msg.Attachments {
		data := att.Data
		if len(data) == 0 && att.Download != nil {
			var dlErr error
			data, dlErr = att.Download(ctx)
			if dlErr != nil {
				g.log.Warn("channel: download inbound attachment failed", "channel", msg.Channel, "name", att.Name, "err", dlErr)
				_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("附件 %s 下载失败：%v", att.Name, dlErr))
				return
			}
		}
		uploaded, err := g.m.UploadAttachmentForBot(binding.BotID, binding.SessionID, att.Name, bytes.NewReader(data))
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("附件 %s 上传失败：%v", att.Name, err))
			return
		}
		attIDs = append(attIDs, uploaded.ID)
	}
	if text == "" && len(attIDs) == 0 {
		return
	}

	// Subscribe BEFORE SubmitForBot so we never miss early deltas on fast turns.
	sub, _, _, subErr := g.m.SubscribeForBot(binding.BotID, binding.SessionID, -1)

	res, err := g.m.SubmitForBot(binding.BotID, binding.SessionID, bot.SendRequest{
		Content:       text,
		Model:         binding.Model,
		Mode:          bot.SendModeQueue,
		AttachmentIDs: attIDs,
	}, bot.SendOptions{DisableSelfTrain: !ra.policy.CanSelfTrain(msg.UserID, msg.IsGroup)})
	if err != nil {
		if sub != nil {
			sub.Close()
		}
		_, _ = ra.adapter.SendText(ctx, dst, "发送失败："+err.Error())
		return
	}
	if res.Queued {
		_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("已加入队列（第 %d 位），发送 /steer <内容> 可立即插话，/cancel 可中止。", res.Position))
		// If the running turn originated from Web/Scheduler (no IM forwarder
		// attached yet), attach one so this queued follow-up reply is delivered.
		if subErr == nil && sub != nil && !g.hasActiveStream(binding.SessionID) {
			g.startStreamForwarder(ctx, ra, dst, binding, sub)
			return
		}
		if sub != nil {
			sub.Close()
		}
		return
	}
	if subErr != nil || sub == nil {
		return
	}
	g.startStreamForwarder(ctx, ra, dst, binding, sub)
}

func (g *Gateway) hasActiveStream(sessionID string) bool {
	g.mu.Lock()
	_, ok := g.streams[sessionID]
	g.mu.Unlock()
	return ok
}

func (g *Gateway) handleQuestionCallback(ctx context.Context, ra registeredAdapter, dst Destination, msg InboundMessage) {
	rest := strings.TrimPrefix(msg.CallbackData, "ans:")
	sep := strings.Index(rest, ":")
	if sep <= 0 {
		return
	}
	qID, answer := rest[:sep], rest[sep+1:]
	binding, ok, err := g.bindings.GetBinding(ctx, msg.Channel, msg.ChatID, msg.ThreadID)
	if err != nil || !ok {
		_, _ = ra.adapter.SendText(ctx, dst, "当前没有活跃会话。")
		return
	}
	// Resolve index-encoded callback payloads ("ans:<qid>:#<idx>") used when
	// Telegram's 64-byte callback_data limit prevents embedding full option text.
	if strings.HasPrefix(answer, "#") {
		if idx, err := strconv.Atoi(strings.TrimPrefix(answer, "#")); err == nil && idx >= 0 {
			if info, _, err := g.m.GetSessionForBot(binding.BotID, binding.SessionID); err == nil {
				for _, pq := range info.Pending {
					if pq.ID == qID && idx < len(pq.Options) {
						answer = pq.Options[idx]
						break
					}
				}
			}
		}
	}
	if err := g.m.AnswerForBot(binding.BotID, binding.SessionID, qID, answer); err != nil {
		_, _ = ra.adapter.SendText(ctx, dst, "提交回答失败："+err.Error())
		return
	}
	_, _ = ra.adapter.SendText(ctx, dst, "已选择："+answer)
}

func (g *Gateway) handleSlashCommand(ctx context.Context, ra registeredAdapter, dst Destination, msg InboundMessage, text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	// Strip optional @botname suffix from Telegram commands (e.g. "/new@my_bot").
	cmd := strings.ToLower(fields[0])
	if at := strings.Index(cmd, "@"); at > 0 {
		cmd = cmd[:at]
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))

	switch cmd {
	case "/new":
		botID := ra.policy.defaultBot()
		model := ""
		if cur, ok, _ := g.bindings.GetBinding(ctx, msg.Channel, msg.ChatID, msg.ThreadID); ok && cur.BotID != "" {
			if b, err := g.m.GetBot(ctx, cur.BotID); err == nil && b.Enabled && ra.policy.AllowsBot(msg.UserID, b.ID) {
				botID = b.ID
				model = cur.Model
			}
		}
		info, err := g.m.CreateSessionForBot(botID, bot.CreateSessionRequest{Model: model})
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "新建会话失败："+err.Error())
			return true
		}
		_ = g.bindings.PutBinding(ctx, Binding{
			Channel: msg.Channel, ChatID: msg.ChatID, ThreadID: msg.ThreadID,
			BotID: botID, SessionID: info.ID, Model: model,
		})
		_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("已开启新会话（bot=%s，session=%s）。", botID, info.ID))
		return true

	case "/cancel":
		cur, ok, _ := g.bindings.GetBinding(ctx, msg.Channel, msg.ChatID, msg.ThreadID)
		if !ok {
			_, _ = ra.adapter.SendText(ctx, dst, "当前没有活跃会话。")
			return true
		}
		if err := g.m.CancelForBot(cur.BotID, cur.SessionID); err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "中止失败："+err.Error())
			return true
		}
		_, _ = ra.adapter.SendText(ctx, dst, "已发送中止信号。")
		return true

	case "/steer":
		if arg == "" {
			_, _ = ra.adapter.SendText(ctx, dst, "用法：/steer <插话内容>")
			return true
		}
		cur, err := g.ensureBinding(ctx, ra.policy, msg.Channel, msg.ChatID, msg.ThreadID)
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "会话初始化失败："+err.Error())
			return true
		}
		sub, _, _, subErr := g.m.SubscribeForBot(cur.BotID, cur.SessionID, -1)
		res, err := g.m.SubmitForBot(cur.BotID, cur.SessionID, bot.SendRequest{
			Content: arg,
			Model:   cur.Model,
			Mode:    bot.SendModeSteer,
		}, bot.SendOptions{DisableSelfTrain: !ra.policy.CanSelfTrain(msg.UserID, msg.IsGroup)})
		if err != nil {
			if sub != nil {
				sub.Close()
			}
			_, _ = ra.adapter.SendText(ctx, dst, "插话失败："+err.Error())
			return true
		}
		if res.Steered {
			_, _ = ra.adapter.SendText(ctx, dst, "↪ 已提交插话，将在当前步骤完成后生效。")
			if subErr == nil && sub != nil && !g.hasActiveStream(cur.SessionID) {
				g.startStreamForwarder(ctx, ra, dst, cur, sub)
				return true
			}
			if sub != nil {
				sub.Close()
			}
		} else if res.Queued {
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("当前回合正在收尾，已转入队列（第 %d 位）。", res.Position))
			if subErr == nil && sub != nil && !g.hasActiveStream(cur.SessionID) {
				g.startStreamForwarder(ctx, ra, dst, cur, sub)
				return true
			}
			if sub != nil {
				sub.Close()
			}
		} else {
			_, _ = ra.adapter.SendText(ctx, dst, "已直接开始新回合。")
			if subErr == nil && sub != nil {
				g.startStreamForwarder(ctx, ra, dst, cur, sub)
			}
		}
		return true

	case "/bot":
		if arg == "" {
			curBot := ra.policy.defaultBot()
			if cur, ok, _ := g.bindings.GetBinding(ctx, msg.Channel, msg.ChatID, msg.ThreadID); ok && cur.BotID != "" {
				curBot = cur.BotID
			}
			bots, _ := g.m.ListBots(ctx)
			var names []string
			for _, b := range bots {
				if b.Enabled && ra.policy.AllowsBot(msg.UserID, b.ID) {
					names = append(names, b.ID)
				}
			}
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("当前 Bot：%s\n可用 Bot：%s\n用法：/bot <id>", curBot, strings.Join(names, ", ")))
			return true
		}
		if !ra.policy.AllowsBot(msg.UserID, arg) {
			g.audit.Record(bot.AuditEntry{
				Action:  "channel_rejected",
				Actor:   msg.Channel + ":" + msg.UserID,
				BotID:   arg,
				Target:  msg.ChatID,
				Outcome: "denied",
				Detail:  "bot not allowed by channel policy",
			})
			_, _ = ra.adapter.SendText(ctx, dst, "切换失败：当前频道策略不允许使用该 Bot。")
			return true
		}
		b, err := g.m.GetBot(ctx, arg)
		if err != nil || !b.Enabled {
			_, _ = ra.adapter.SendText(ctx, dst, "切换失败：Bot 不存在或已停用。")
			return true
		}
		info, err := g.m.CreateSessionForBot(b.ID, bot.CreateSessionRequest{})
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "切换失败："+err.Error())
			return true
		}
		_ = g.bindings.PutBinding(ctx, Binding{
			Channel: msg.Channel, ChatID: msg.ChatID, ThreadID: msg.ThreadID,
			BotID: b.ID, SessionID: info.ID,
		})
		_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("已切换至 Bot「%s」(%s)，并开启新会话。", b.Name, b.ID))
		return true

	case "/model":
		cur, err := g.ensureBinding(ctx, ra.policy, msg.Channel, msg.ChatID, msg.ThreadID)
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "会话初始化失败："+err.Error())
			return true
		}
		if arg == "" {
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("当前模型：%s\n用法：/model <alias>", cur.Model))
			return true
		}
		if !g.hasModel(arg) {
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("切换失败：未知模型 %q（可用：%s）。", arg, strings.Join(g.modelNames(), ", ")))
			return true
		}
		cur.Model = arg
		_ = g.bindings.PutBinding(ctx, cur)
		_, _ = ra.adapter.SendText(ctx, dst, "已切换模型为："+arg)
		return true

	case "/status":
		cur, ok, _ := g.bindings.GetBinding(ctx, msg.Channel, msg.ChatID, msg.ThreadID)
		if !ok {
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("当前无活跃会话（默认 Bot：%s）。", ra.policy.defaultBot()))
			return true
		}
		info, _, err := g.m.GetSessionForBot(cur.BotID, cur.SessionID)
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "会话已失效，发送消息将自动创建新会话。")
			return true
		}
		_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("Bot: %s\nSession: %s\n状态: %s · 排队: %d · 消息数: %d",
			cur.BotID, info.ID, info.Status, len(info.Queue), info.MsgCount))
		return true

	case "/approve":
		if g.schedules == nil {
			_, _ = ra.adapter.SendText(ctx, dst, "定时任务功能未启用。")
			return true
		}
		if arg == "" {
			_, _ = ra.adapter.SendText(ctx, dst, "用法：/approve <schedule_id>")
			return true
		}
		targetSch, err := g.schedules.Get(ctx, arg)
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "批准失败："+err.Error())
			return true
		}
		if !ra.policy.CanApprove(msg.UserID, targetSch.BotID) {
			g.audit.Record(bot.AuditEntry{
				Action:  "channel_rejected",
				Actor:   msg.Channel + ":" + msg.UserID,
				BotID:   targetSch.BotID,
				Target:  targetSch.ID,
				Outcome: "denied",
				Detail:  "approve not allowed by channel policy",
			})
			_, _ = ra.adapter.SendText(ctx, dst, "批准失败：无权批准该 Bot 的定时任务。")
			return true
		}
		sch, err := g.schedules.Approve(ctx, arg)
		if err != nil {
			_, _ = ra.adapter.SendText(ctx, dst, "批准失败："+err.Error())
			return true
		}
		g.audit.Record(bot.AuditEntry{
			Action:  "schedule_approved",
			Actor:   msg.Channel + ":" + msg.UserID,
			BotID:   sch.BotID,
			Target:  sch.ID,
			Outcome: "ok",
		})
		_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("已批准任务「%s」(%s)。", sch.Name, sch.ID))
		return true

	case "/secret":
		if msg.IsGroup {
			_, _ = ra.adapter.SendText(ctx, dst, "出于安全原因，仅限私聊管理 Bot 密钥。")
			return true
		}
		if !ra.policy.IsAllowedUser(msg.UserID) {
			g.audit.Record(bot.AuditEntry{
				Action:  "channel_rejected",
				Actor:   msg.Channel + ":" + msg.UserID,
				Target:  msg.ChatID,
				Outcome: "denied",
				Detail:  "secret management requires allow_users membership",
			})
			_, _ = ra.adapter.SendText(ctx, dst, "无权管理 Bot 密钥。")
			return true
		}
		curBot := ra.policy.defaultBot()
		if cur, ok, _ := g.bindings.GetBinding(ctx, msg.Channel, msg.ChatID, msg.ThreadID); ok && cur.BotID != "" {
			if b, err := g.m.GetBot(ctx, cur.BotID); err == nil && b.Enabled {
				curBot = b.ID
			}
		}
		if !ra.policy.AllowsBot(msg.UserID, curBot) {
			_, _ = ra.adapter.SendText(ctx, dst, "当前频道策略不允许操作该 Bot 的密钥。")
			return true
		}
		parts := strings.Fields(arg)
		subCmd := "list"
		if len(parts) > 0 {
			subCmd = strings.ToLower(parts[0])
		}
		switch subCmd {
		case "list":
			list, err := g.m.ListSecrets(ctx, curBot)
			if err != nil {
				_, _ = ra.adapter.SendText(ctx, dst, "查询密钥失败："+err.Error())
				return true
			}
			if len(list) == 0 {
				_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("Bot「%s」尚未配置密钥。\n推荐本地安全录入：deepai-bot secret set --bot %s <NAME>\n或私聊：/secret set <NAME> <VALUE>", curBot, curBot))
				return true
			}
			names := make([]string, 0, len(list))
			for _, m := range list {
				names = append(names, m.Name)
			}
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("Bot「%s」已配置密钥：%s\n可在命名 connector 的 http_request 中使用 {{secret:<NAME>}} 引用。", curBot, strings.Join(names, ", ")))
			return true
		case "set":
			deleted := false
			if msg.MessageID != "" {
				if del, ok := ra.adapter.(MessageDeleter); ok {
					if err := del.DeleteMessage(ctx, dst, msg.MessageID); err == nil {
						deleted = true
					}
				}
			}
			// Split into at most 3 tokens ("set", NAME, VALUE) preserving spaces inside VALUE if any.
			rest := strings.TrimSpace(strings.TrimPrefix(arg, parts[0]))
			nameEnd := strings.IndexAny(rest, " \t")
			if nameEnd <= 0 {
				_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("用法：/secret set <NAME> <VALUE>（更推荐使用 CLI：deepai-bot secret set --bot %s <NAME>）", curBot))
				return true
			}
			secName := strings.TrimSpace(rest[:nameEnd])
			secVal := strings.TrimSpace(rest[nameEnd+1:])
			meta, err := g.m.SetSecret(ctx, curBot, secName, secVal)
			if err != nil {
				_, _ = ra.adapter.SendText(ctx, dst, "保存密钥失败："+err.Error())
				return true
			}
			g.audit.Record(bot.AuditEntry{
				Action:  "secret_set",
				Actor:   msg.Channel + ":" + msg.UserID,
				BotID:   meta.BotID,
				Target:  meta.Name,
				Outcome: "ok",
			})
			historyNote := fmt.Sprintf("请手动删除上方含明文的聊天记录；生产环境推荐使用 CLI：deepai-bot secret set --bot %s %s", meta.BotID, meta.Name)
			if deleted {
				historyNote = fmt.Sprintf("已自动请求撤回含明文的命令消息；更高安全要求请优先使用 CLI：deepai-bot secret set --bot %s %s", meta.BotID, meta.Name)
			}
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("已为 Bot「%s」加密保存密钥 %s（未写入模型上下文，可通过 {{secret:%s}} 安全调用）。\n%s", meta.BotID, meta.Name, meta.Name, historyNote))
			return true
		case "delete", "del", "rm":
			if len(parts) < 2 {
				_, _ = ra.adapter.SendText(ctx, dst, "用法：/secret delete <NAME>")
				return true
			}
			secName := strings.TrimSpace(parts[1])
			if err := g.m.DeleteSecret(ctx, curBot, secName); err != nil {
				_, _ = ra.adapter.SendText(ctx, dst, "删除密钥失败："+err.Error())
				return true
			}
			g.audit.Record(bot.AuditEntry{
				Action:  "secret_deleted",
				Actor:   msg.Channel + ":" + msg.UserID,
				BotID:   curBot,
				Target:  secName,
				Outcome: "ok",
			})
			_, _ = ra.adapter.SendText(ctx, dst, fmt.Sprintf("已删除 Bot「%s」的密钥 %s。", curBot, secName))
			return true
		default:
			_, _ = ra.adapter.SendText(ctx, dst, "用法：/secret list | /secret set <NAME> <VALUE> | /secret delete <NAME>")
			return true
		}
	}
	return false
}

func (g *Gateway) ensureBinding(ctx context.Context, policy AccessPolicy, ch, chatID, threadID string) (Binding, error) {
	if b, ok, err := g.bindings.GetBinding(ctx, ch, chatID, threadID); err == nil && ok {
		if prof, bErr := g.m.GetBot(ctx, b.BotID); bErr == nil && prof.Enabled {
			if _, _, err := g.m.GetSessionForBot(b.BotID, b.SessionID); err == nil {
				return b, nil
			}
		}
	}
	botID := policy.defaultBot()
	info, err := g.m.CreateSessionForBot(botID, bot.CreateSessionRequest{
		Title: fmt.Sprintf("%s:%s", ch, chatID),
	})
	if err != nil {
		return Binding{}, err
	}
	b := Binding{
		Channel:   ch,
		ChatID:    chatID,
		ThreadID:  threadID,
		BotID:     botID,
		SessionID: info.ID,
	}
	_ = g.bindings.PutBinding(ctx, b)
	return b, nil
}

func (g *Gateway) hasModel(alias string) bool {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return false
	}
	info := g.m.Info()
	for _, m := range info.Models {
		if m.Name == alias {
			return true
		}
	}
	return info.DefaultModel != "" && info.DefaultModel == alias
}

func (g *Gateway) modelNames() []string {
	info := g.m.Info()
	names := make([]string, 0, len(info.Models))
	for _, m := range info.Models {
		names = append(names, m.Name)
	}
	return names
}

// startStreamForwarder streams a session's events to the IM adapter with
// throttled message editing, tool status indicators, question cards, and
// artifact attachments until the session goes idle (including any auto-dequeued
// follow-up turns).
func (g *Gateway) startStreamForwarder(ctx context.Context, ra registeredAdapter, dst Destination, b Binding, sub *bot.Subscription) {
	g.mu.Lock()
	if _, active := g.streams[b.SessionID]; active {
		g.mu.Unlock()
		sub.Close()
		return
	}
	g.streams[b.SessionID] = struct{}{}
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		curSub := sub
		defer func() {
			if curSub != nil {
				curSub.Close()
			}
		}()
		defer func() {
			g.mu.Lock()
			delete(g.streams, b.SessionID)
			g.mu.Unlock()
		}()

		interval := ra.policy.editInterval()
		maxRunes := ra.policy.maxRunes()
		tick := time.NewTicker(interval)
		defer tick.Stop()

		var textBuf strings.Builder
		var activeTool string
		var chunkMsgIDs []string
		var chunkTexts []string
		var lastSeq int64
		dirty := false

		flush := func(final bool) {
			rawText := strings.TrimSpace(textBuf.String())
			var chunks []string
			if rawText != "" {
				chunks = splitRunes(rawText, maxRunes)
			}
			// Keep the transient tool indicator inside the existing last chunk
			// (or chunk 0 when no text arrived yet) so it never spawns an extra
			// overflow message that becomes orphaned once the tool finishes.
			if !final && activeTool != "" {
				indicator := "🔧 运行 " + activeTool + "..."
				if len(chunks) == 0 {
					chunks = []string{indicator}
				} else {
					last := chunks[len(chunks)-1]
					cand := last + "\n\n" + indicator
					if maxRunes <= 0 || len([]rune(cand)) <= maxRunes {
						chunks[len(chunks)-1] = cand
					}
				}
			}
			if len(chunks) == 0 {
				dirty = false
				return
			}
			for i, ch := range chunks {
				if i < len(chunkMsgIDs) {
					if chunkTexts[i] == ch {
						continue
					}
					if err := ra.adapter.EditText(ctx, dst, chunkMsgIDs[i], ch); err != nil {
						g.log.Warn("channel: EditText failed", "channel", ra.adapter.Name(), "chat", dst.ChatID, "msg_id", chunkMsgIDs[i], "final", final, "err", err)
						if final {
							if id, sendErr := ra.adapter.SendText(ctx, dst, ch); sendErr != nil {
								g.log.Warn("channel: fallback SendText failed", "channel", ra.adapter.Name(), "chat", dst.ChatID, "err", sendErr)
							} else {
								chunkMsgIDs[i] = id
								chunkTexts[i] = ch
							}
						}
					} else {
						chunkTexts[i] = ch
					}
				} else {
					id, err := ra.adapter.SendText(ctx, dst, ch)
					if err != nil {
						g.log.Warn("channel: SendText failed", "channel", ra.adapter.Name(), "chat", dst.ChatID, "err", err)
						break
					}
					chunkMsgIDs = append(chunkMsgIDs, id)
					chunkTexts = append(chunkTexts, ch)
				}
			}
			dirty = false
		}

		handleEvent := func(e bot.Event) (done bool) {
			if e.Seq > lastSeq {
				lastSeq = e.Seq
			}
			switch e.Type {
			case bot.EventTextDelta:
				textBuf.WriteString(e.Text)
				dirty = true
			case bot.EventToolCallStart:
				if e.Tool != nil {
					activeTool = e.Tool.Name
					dirty = true
				}
			case bot.EventToolCallEnd:
				activeTool = ""
				dirty = true
			case bot.EventQuestion:
				if e.Question != nil {
					flush(false)
					if _, err := ra.adapter.SendQuestion(ctx, dst, *e.Question); err != nil {
						g.log.Warn("channel: SendQuestion failed", "channel", ra.adapter.Name(), "chat", dst.ChatID, "question", e.Question.ID, "err", err)
					}
				}
			case bot.EventArtifact:
				if e.Artifact != nil && e.Artifact.DownloadPath != "" {
					if rf, err := g.m.ResolveSessionFileForBot(b.BotID, b.SessionID, e.Artifact.DownloadPath); err != nil {
						g.log.Warn("channel: resolve artifact failed", "session", b.SessionID, "path", e.Artifact.DownloadPath, "err", err)
					} else if err := ra.adapter.SendAttachment(ctx, dst, rf.AbsPath, e.Artifact.Description); err != nil {
						g.log.Warn("channel: SendAttachment failed", "channel", ra.adapter.Name(), "chat", dst.ChatID, "file", rf.Name, "err", err)
					}
				}
			case bot.EventTurnEnd:
				activeTool = ""
				if e.Turn != nil && textBuf.Len() == 0 && e.Turn.FinalOutput != "" {
					textBuf.WriteString(e.Turn.FinalOutput)
				}
				if e.Turn != nil && (e.Turn.Status == bot.TurnFailed || e.Turn.Status == bot.TurnTimedOut) && textBuf.Len() == 0 {
					textBuf.WriteString("执行失败：" + e.Turn.Error)
				}
				flush(true)
				// Exit when the session is idle with no newer events beyond lastSeq
				// and either the queue is empty OR the turn did not complete (failed,
				// timed_out, cancelled — those preserve the queue without auto-dequeuing).
				info, _, err := g.m.GetSessionForBot(b.BotID, b.SessionID)
				nonCompleted := e.Turn != nil && e.Turn.Status != bot.TurnCompleted
				if err != nil || (info.Status == bot.SessionIdle && info.LastSeq <= lastSeq && (len(info.Queue) == 0 || nonCompleted)) {
					return true
				}
				// Reset chunk buffer for next queued turn.
				textBuf.Reset()
				chunkMsgIDs = nil
				chunkTexts = nil
				dirty = false
			}
			return false
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if dirty {
					flush(false)
				}
			case e, ok := <-curSub.C:
				if !ok {
					if curSub.Lagged() {
						flush(false)
						nextSub, backlog, _, err := g.m.SubscribeForBot(b.BotID, b.SessionID, lastSeq)
						if err != nil {
							g.log.Warn("channel: resubscribe after lag failed", "session", b.SessionID, "err", err)
							flush(true)
							return
						}
						curSub = nextSub
						for _, be := range backlog {
							if handleEvent(be) {
								return
							}
						}
						continue
					}
					flush(true)
					return
				}
				if handleEvent(e) {
					return
				}
			}
		}
	}()
}

func splitRunes(s string, max int) []string {
	if max <= 0 {
		return []string{s}
	}
	r := []rune(s)
	if len(r) <= max {
		return []string{s}
	}
	var out []string
	for len(r) > 0 {
		n := min(max, len(r))
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return out
}
