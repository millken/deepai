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
deepai-bot                         # 前台运行（等同 deepai-bot serve）
deepai-bot start -w ~/code/proj    # 后台守护运行
deepai-bot status                  # 查看状态（--json 供脚本/GUI 使用）
deepai-bot stop                    # 优雅停止：取消运行中的任务并保存
deepai-bot token [--rotate]        # 查看/轮换访问令牌
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

文件位置：`~/.deepai/bot/bot.db`（会话与长期记忆）、`bot.json`（运行状态）、`bot.log`（`start` 模式日志）、`token`、`sandbox/`。

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

bot 能执行 shell 命令和修改文件。默认只监听本机并强制 token；需要远程访问时请放在 TLS 反向代理、SSH 隧道或 Tailscale 之后，不要直接暴露 `0.0.0.0`。
