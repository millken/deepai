# deepai-bot

把 deepai agent 作为**常驻后台服务**运行：会话与任务常驻在 bot 进程中，Web / GUI / App / IM 适配器等客户端通过 HTTP、SSE、NDJSON、WebSocket 控制它。设计与完整协议见 [`docs/BOT_DESIGN.md`](../../docs/BOT_DESIGN.md)。

配置与 `deepai` 共用：`~/.deepai/config.yaml`（模型）、`~/.deepai/.env`（API key）、`DEEPAI.md`、技能、插件、`.mcp.json`。会话和长期记忆存放在 bot 自己的 `~/.deepai/bot/bot.db`，与 CLI 的会话库分开（`deepai -r` 不能续聊 bot 会话）。先用 `deepai setup` 配好模型即可。

## 构建

```bash
make build-bot          # ./bin/deepai-bot
make install-bot        # 安装到 ~/.local/bin
```

## 运行

```bash
deepai-bot                                      # 前台运行（等同 deepai-bot serve）
deepai-bot start -w ~/code/proj                 # 后台守护运行（支持同机多 workdir 并行）
deepai-bot status [-w ~/code/proj] [--json]     # 查看状态
deepai-bot stop [-w ~/code/proj]                # 优雅停止：取消运行中的任务并保存
deepai-bot token [--rotate]                     # 查看/轮换主访问令牌
deepai-bot token create --name app --role user  # 签发受限令牌（admin | user | readonly）
deepai-bot secret set <NAME> <VAL> [--bot <id>] # 为指定 Bot 加密保存第三方 API 密钥
```

启动后打印 Web 控制台地址（`http://127.0.0.1:8787/#token=…`），浏览器打开即可使用。

| 参数 | 默认 | 说明 |
|------|------|------|
| `--addr` | `127.0.0.1:8787`（`$DEEPAI_BOT_ADDR`） | 监听地址 |
| `--token` | `$DEEPAI_BOT_TOKEN` → `~/.deepai/bot/token`（首次自动生成） | API 令牌 |
| `--no-auth` | off | 关闭鉴权，仅限回环地址 |
| `-w, --workdir` | 当前目录 | agent 的工作目录 |
| `-m, --model` | 配置中的默认模型 | 默认模型别名 |
| `--max-tool-calls` | 0（不限） | 每轮工具调用上限 |
| `--turn-timeout` | 0（不限） | 单轮超时；超时的 turn 以 `turn_end` status `timed_out` 结束 |
| `--idle-timeout` | 30m | 空闲会话卸载（释放沙箱） |
| `--ephemeral` | off | 会话只存内存，不写 `bot.db`、不用长期记忆 |
| `--cors-origin` | — | 允许跨域访问的前端 origin（可重复，`*` 为任意） |
| `--no-web` | off | 不提供内置 Web 控制台 |
| `--no-schedule` | off | 关闭定时任务（不注册 `schedule` 工具、不启动调度器，接口返回 404） |

文件位置：`~/.deepai/bot/bot.db`（多 Bot 配置、加密密钥 `bot_secrets`、会话、长期记忆、定时任务、渠道绑定与受限令牌）、`bots/<bot_id>/skills/`（各 Bot 对话沉淀的专属技能）、`instances/` 与 `bot.json`（运行实例状态）、`bot.log`（守护日志）、`audit.jsonl`（安全审计日志）、`token`、`sandbox/`。

## 快速调用

```bash
TOKEN=$(deepai-bot token); B=http://127.0.0.1:8787; H="Authorization: Bearer $TOKEN"

SID=$(curl -s -H "$H" -H 'Content-Type: application/json' -d '{}' $B/v1/sessions | jq -r .id)

# 流式（SSE）
curl -N -H "$H" -H 'Content-Type: application/json' -H 'Accept: text/event-stream' \
  -d '{"content":"总结一下这个仓库的结构"}' $B/v1/sessions/$SID/messages

# 阻塞直到完成
curl -s -H "$H" -H 'Content-Type: application/json' \
  -d '{"content":"再详细一点"}' "$B/v1/sessions/$SID/messages?wait=1" | jq .final_output

# 断线后续传
curl -N -H "$H" "$B/v1/sessions/$SID/events?since=42"
```

## 定时任务

`deepai-bot` 常驻时会注册一个 `schedule` 工具，模型可以自己创建一次性 / 周期任务；每个触发就是往某个会话里发一条消息、跑一个普通 turn，执行过程用现有的会话事件流观看。设计见 [`docs/BOT_SCHEDULER_DESIGN.md`](../../docs/BOT_SCHEDULER_DESIGN.md)。

```yaml
# ~/.deepai/config.yaml（缺省值即下列值）
schedule:
  enabled: true
  timezone: Asia/Taipei     # 缺省 = 系统时区
  confirm: ask              # 交互轮次里创建周期任务需用户确认；auto = 直接生效
  min_interval: 5m
  max_concurrent_runs: 2
  max_schedules: 50
  default_timeout: 30m            # 单次执行的时长上限（等待回答时不计时）
  default_answer_timeout: 30m     # 无人值守时每个提问的等待上限
  default_max_auto_answers: 3     # 单次运行最多自动回答几次
```

```bash
# 用户直接创建（不走确认）
curl -s -H "$H" -H 'Content-Type: application/json' \
  -d '{"name":"检查 CI","prompt":"查看最近一次 CI 结果并总结","trigger":{"type":"once","in":"2h"},"timezone":"UTC"}' \
  $B/v1/schedules | jq .

curl -s -H "$H" "$B/v1/schedules?state=active" | jq .        # 列表
curl -s -X POST -H "$H" $B/v1/schedules/sch_xxx/run | jq .    # 立即跑一次
curl -N -H "$H" "$B/v1/events?since=0"                        # 全局 schedule_* 事件
```

WS ops 为 `list_schedules` / `create_schedule` / `pause_schedule` / `run_schedule` / `cancel_run` 等，参数放在 `params`，id 放 `schedule_id` / `run_id`。拿到 `run.session_id` 后订阅该会话即可看执行过程。支持 `once`、`interval` 与 5 字段 `cron`（含 `@hourly`/`@daily`/`@weekly`/`@monthly`/`@yearly` 与 `L`），内置 Web 控制台提供「任务」页签，并复用 `~/.deepai/config.yaml` 的 `notifications` 配置推送 `schedule_run_end` / `schedule_ask` / `schedule_approval` 通知。

任务可带 `answer_timeout`、`on_answer_timeout`（`assume` 默认 / `fail`）、`max_auto_answers`、`max_tool_calls`、`timeout`。定时 turn 里模型调用 `ask_clarification` 时：

- Run 转为 `waiting_input`，全局流发 `schedule_run_updated`，**执行时钟暂停**（不会把等人的时间算成 `timed_out`）；会话流照常发 `question`，可用现有的 `/answer` 或 WS `answer` 回答。
- `answer_timeout` 内无人回答且策略为 `assume`、自动回答次数没用完时，bot 自动回答「请按最合理的假设继续……」，`question_answered` 事件带 `auto: true`，Run 的 `auto_answers` 加一并回到 `running`。
- 策略为 `fail` 或次数用尽时，以 `cancel_source=scheduler` 取消 turn，Run 记 `failed`（`error_code=answer_timeout`）。客户端主动 `cancel_run` 则记 `cancelled`。

`turn_end` 现在带 `cancel_source`（`client` / `shutdown` / `scheduler`），调度器据此区分主动取消、bot 关闭与超时失败。

## 多 Bot 对话训练与接入第三方业务 API

所有 Bot 共享同一套模型与运行时（「出生一致」），通过对话逐步演化出独立的长期记忆（`bot:<id>`）、专属技能与外部业务 API 连接器（完整设计见 [`docs/BOT_MULTI_DESIGN.md`](../../docs/BOT_MULTI_DESIGN.md)）：

```bash
# 1. 创建一个仅允许受控 API/技能与定时任务（剥离 bash）的自定义业务 Bot
curl -s -H "$H" -H 'Content-Type: application/json' \
  -d '{"id":"assistant","name":"业务助手","tool_allowlist":["http_request","skill","save_bot_skill","update_bot_persona","schedule","ask_clarification"]}' \
  $B/v1/bots | jq .

# 2. 加密录入第三方 API 密钥（机器指纹 AES-GCM 落库，明文不进上下文；也可在 Telegram/飞书私聊发 /secret set）
deepai-bot secret set NOTION_TOKEN "ntn_xxx" --bot assistant

# 3. 创建会话后直接在对话中训练它：
#    Bot 会调用 update_bot_persona（开启 isolated_prompt 并绑定带 {{secret:NOTION_TOKEN}} 的连接器）
#    与 save_bot_skill（沉淀专属 ~/.deepai/bot/bots/assistant/skills/<name>/SKILL.md），随后通过 http_request 安全调用外部 API
```

## IM 渠道与 OpenAI 兼容接口

- **Telegram / 飞书网关**：在 `~/.deepai/config.yaml` 中配置 `channels.telegram` 或 `channels.feishu`（默认白名单拒绝一切未授权用户，支持 `/new`、`/steer`、`/cancel`、`/bot`、`/model`、`/approve` 与私聊 `/secret`）；详见 [`docs/BOT_NEXT_DESIGN.md`](../../docs/BOT_NEXT_DESIGN.md) §5。
- **OpenAI 兼容接口**：`POST /v1/chat/completions` 支持 `model: "bot:<bot_id>"` 或 `"bot:<bot_id>/<model_alias>"` 及 `X-DeepAI-Session` 会话复用，可直接接入 Open WebUI、Cherry Studio、Raycast 等客户端。

## 交给 systemd 管理

```ini
# ~/.config/systemd/user/deepai-bot.service
[Unit]
Description=deepai bot

[Service]
ExecStart=%h/.local/bin/deepai-bot serve -w %h/code/proj
Restart=on-failure

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now deepai-bot
```

## 安全提示

bot 能执行 shell 命令和修改文件。默认只监听本机并强制 token；需要远程访问时请放在 TLS 反向代理、SSH 隧道或 Tailscale 之后，不要直接暴露 `0.0.0.0`。对于面向业务或外部 IM 渠道的自定义 Bot，建议通过 `tool_allowlist` 移除 `bash`，改用受控的 `http_request` + `{{secret:NAME}}` 访问第三方 API。
