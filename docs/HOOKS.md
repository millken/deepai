# deepai hooks：事件通知与远程控制

`~/.deepai/config.yaml` 的两个可选块驱动整个 hook 系统。两者都不配置时，系统完全关闭，行为与此功能存在之前一字不差。

```yaml
notifications:
  - events: [ask, idle, pr_awaiting_merge, mission_end]   # 省略 = 全部事件
    webhook_url: https://example.com/deepai-events          # 必须接收 hook.Event JSON（见下）
  - events: [turn_end]
    command: ["~/.deepai/hooks/log-turn.sh"]               # 企业微信/TG 用 command 脚本（见参考脚本节）

control:
  command: ["~/.deepai/hooks/tg-control.sh"]   # 二选一；command 优先
  # url: http://127.0.0.1:8080/commands
  poll_seconds: 3                               # 默认 3
```

## 事件（outbound）

| kind | 含义 | Message 内容 |
|---|---|---|
| `turn_start` | 一个 turn 开始 | 用户输入原文 |
| `turn_end` | 一个 turn 结束 | token 用量摘要 |
| `ask` | agent 提问、等待用户回答（ask_clarification / 计划确认） | 问题原文 |
| `idle` | REPL 回到输入提示符，等待下一条输入 | "waiting for input" |
| `mission_end` | mission 到达终态（done / handed_over / design_failed / aborted） | mission id + 状态 |
| `pr_awaiting_merge` | PR review+CI 全绿，停在 awaiting_merge 等指令 | PR 号（`pr_auto_merge: true` 时不发） |
| `session_end` | 会话结束 | turn 计数 |

### webhook sink

POST JSON（即 `hook.Event`）：`{"kind":"ask","session_id":"…","work_dir":"…","message":"…","time":"…"}`。
每个投递 5 秒超时，失败只记日志，绝不阻塞或打断 turn。

### command sink（bash hook）

argv 形式，支持 `~` 展开。每次事件执行一次：

- **stdin**：与 webhook 相同的完整 JSON；
- **环境变量**：`DEEPAI_EVENT`、`DEEPAI_SESSION_ID`、`DEEPAI_WORKDIR`、`DEEPAI_MESSAGE` —— 不用 jq 也能写 hook。

```sh
#!/bin/sh
# ~/.deepai/hooks/log-turn.sh
printf '%s %s\n' "$DEEPAI_EVENT" "$DEEPAI_MESSAGE" >> "$HOME/.deepai/hooks/events.log"
cat >> "$HOME/.deepai/hooks/events.jsonl"   # 完整 JSON
```

## 控制（inbound）

`control.command` 每 `poll_seconds` 执行一次（或 `control.url` 被 GET 一次）。stdout / 响应体按行解析，每行一条指令：

| 行 | 指令 |
|---|---|
| `reply <text>`（或不以关键字开头的任意行） | 把 text 注入为当前提问的回答 / 下一条用户输入（无 prompt 等待时 FIFO 排队） |
| `interrupt` | 等价 Ctrl+C：取消正在运行的 turn |
| `cancel-task <id>` | 取消单个 subagent（等价 Ctrl+X） |
| 空行 / `noop` | 忽略（裸 `/` 开头行也被解析器忽略；slash 命令由参考脚本加 `reply ` 前缀转发） |

语义要点：

- **reply 在 prompt/ask 可见时立即提交；其余情况排队**。一次 poll 取回多条消息时，第一条入当前 prompt/ask，其余按 FIFO 排队；turn 进行中到达的 reply 同样排队。队列只在下一个 REPL prompt 喂入（一次一条）——不会拿去回答 agent 的提问：提问只能由提问期间到达的 reply 回答。被脚本确认过的远程消息不会静默丢失。
- **interrupt 只对进行中的 turn / CI 等待有效**。空闲期到达的 interrupt 会被丢弃（turn 启动与 CI 等待入口都会清空残留信号），不会误杀你的下一个正常 turn。
- **去重是 control 脚本自己的责任**（见下面 TG 脚本的 offset 机制）：每次 poll 返回的行都会被当作新指令执行。

每次 poll 时脚本能拿到当前状态的环境变量：

- `DEEPAI_STATE`：`asking`（agent 正在等回答）或 `idle`；
- `DEEPAI_ASKING`：`true` / `false`；
- `DEEPAI_QUESTION`：正在等待回答的问题原文（asking 时）。

## Telegram 参考脚本

### 通知：notify.sh

```sh
#!/bin/sh
# ~/.deepai/hooks/notify.sh — config.yaml notifications[].command 指向它
BOT_TOKEN="123456:ABC..."
CHAT_ID="123456789"
MSG="$DEEPAI_EVENT: $DEEPAI_MESSAGE"
# -f：TG API 返回非 2xx 时 curl 非零退出，投递失败才会进 deepai 日志
curl -fsS -X POST "https://api.telegram.org/bot${BOT_TOKEN}/sendMessage" \
  -d chat_id="$CHAT_ID" -d text="$MSG" >/dev/null
```

### 控制：tg-control.sh

脚本必须自己记住消费进度（offset），否则每轮 poll 都会重复执行同一条历史消息。Telegram 的规则：消费了 `update_id:N` 之后，下次请求用 `offset=N+1`。

```sh
#!/bin/sh
# ~/.deepai/hooks/tg-control.sh — config.yaml control.command 指向它
BOT_TOKEN="123456:ABC..."
OFFSET_FILE="$HOME/.deepai/hooks/tg-offset"
mkdir -p "$(dirname "$OFFSET_FILE")"
OFFSET=$(cat "$OFFSET_FILE" 2>/dev/null || echo 0)

# 只看文本消息；输出行格式 = 上面表格的行协议：
#   回复消息（含 /pr merge 等 slash 命令）→ "reply <原文>"
#   interrupt → "interrupt"
#   cancel-task <id> → 原样转发
# 局限：按逗号切行，消息文本内含逗号会被截断；要更稳请自行换 jq。
curl -sS "https://api.telegram.org/bot${BOT_TOKEN}/getUpdates?offset=${OFFSET}&timeout=0&allowed_updates=%5B%22message%22%5D" \
| tr ',' '\n' | awk '
  /"update_id"/ {
    line=$0; sub(/^.*"update_id":/, "", line); sub(/[^0-9].*$/, "", line);
    if (line != "" && line+1 > max) max=line+1;
  }
  /"text"/ {
    text=$0; gsub(/^.*"text":"/, "", text); gsub(/".*$/, "", text);
    if (text == "interrupt" || text ~ /^cancel-task /) print text;
    else print "reply " text;
  }
  END { if (max) print max > "'"$OFFSET_FILE"'.tmp" }
'
if [ -s "$OFFSET_FILE.tmp" ]; then mv "$OFFSET_FILE.tmp" "$OFFSET_FILE"; fi
```

脚本输出的行（`reply hello from telegram`、`interrupt`、`cancel-task <id>`）与解析器语法由 `pkg/hook/control_test.go` 钉版；整个脚本（含 offset 提取与 rename 落盘）由同一测试对 docs 原文直接运行两轮断言去重：改任何一边而不改另一边，测试会红。

## 微信（企业微信）参考

企业微信**群机器人 webhook 是单向的**——只能通知，不能接收指令。要双向需自建应用（回调服务器），本版不做。

```sh
#!/bin/sh
# ~/.deepai/hooks/wecom.sh
# 两个硬要求：消息含引号/反斜杠/换行也必须拼出合法 JSON；WeCom 失败也返回
# HTTP 200 + errcode!=0，不显式检查 errcode 投递失败就无迹可查。
KEY="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
ESCAPED=$(printf '[%s] %s' "$DEEPAI_EVENT" "$DEEPAI_MESSAGE" | awk 'BEGIN{ORS=""}
  {gsub(/\\/,"\\\\"); gsub(/"/,"\\\""); gsub(/\t/,"\\t"); print $0 "\\n"}' | tr -d '\r\n')
PAYLOAD=$(printf '{"msgtype":"text","text":{"content":"%s"}}' "$ESCAPED")
RESP=$(curl -sS -X POST "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=${KEY}" \
  -H 'Content-Type: application/json' -d "$PAYLOAD")
case "$RESP" in
  *'"errcode":0'*) exit 0 ;;
esac
printf '%s\n' "$RESP" >&2
exit 1
```

微信侧的"控制"用 `control.command` 指向一个轮询你自己中转服务（例如企业微信自建应用的消息回调落地到一个本地文件/接口）的脚本即可，行协议不变。

## 信任边界

- `control.url` 的返回文本等价于"别人替你打字"：它能注入任意 prompt、取消 turn 与 subagent。URL 只应指向你自己的本地服务；不要把 control 暴露给不可信的网络来源。
- `notifications[].webhook_url` 会把 session 内摘要（问题原文、PR 号、用量）发到该地址——企业微信机器人 URL 本身就是凭证，视同密码保管。
- 两块配置都留在 `~/.deepai/config.yaml`（本机私有），不进版本库。
