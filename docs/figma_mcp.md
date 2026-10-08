# Figma MCP 接入指南

> 让 deepai 读取 Figma 设计数据（布局、样式、组件、切图），用于设计还原与 UI 代码生成。
> 两条路径均在 macOS 实测通过：社区版（figma-developer-mcp v0.13.2，默认）、官方远程（2026-10，41 个工具，不受支持的变通）。

## 一、方案选型

| 方案 | 传输 | 鉴权 | 工具面 | 可用性 |
|---|---|---|---|---|
| 社区版 [figma-developer-mcp](https://github.com/GLips/Figma-Context-MCP)（**默认**） | stdio | Figma PAT | 2 个：`get_figma_data` + `download_figma_images`（只读，简化 + CSS 友好结构） | ✅ 即接即用，任何账号（见 §二） |
| 官方远程（`mcp.figma.com/mcp`） | http | OAuth，仅白名单客户端可**注册** | 41 个：读 + 写（生成设计/图片、建文件、传素材、搜设计系统、weave 系列） | ⚠️ 需借道白名单客户端铸 token，**不受支持的变通做法**（见 §三） |
| 官方本地 Dev Mode（`127.0.0.1:3845/mcp`） | http | 无需 token | 读为主 | ❌ 需 Figma Desktop + Dev/Full 付费席位，免费版菜单无开关 |

选择建议：**默认走社区版 PAT**——两步即通、无 OAuth、无客户端准入问题，覆盖设计→代码的读取场景。官方远程仅当确需写回 Figma / 截图比对 / 设计系统搜索等 41 工具能力时，再按 §三 自行承担变通风险启用。

## 二、默认接入：社区版（PAT，零依赖）

### 1. 生成令牌

figma.com → Settings → Security → **Personal access tokens** → Generate new token，勾选 **File content** 读取权限。

### 2. 配置令牌

令牌可写入 `~/.deepai/.env`（加一行 `FIGMA_API_KEY=<token>`），或 shell 导出——二者取其一。`~/.deepai/.env` 在 deepai 启动时由 `env.Load` 注入进程环境（不覆盖已有变量，shell 已导出时 `.env` 同名行不生效），MCP 配置里的 `${VAR}` 按 `os.Getenv` 展开；走 `.env` 的好处是 GUI 启动也生效、不污染 shell。

### 3. MCP 配置

`~/.deepai/mcp.json`（全局）或项目根 `.mcp.json`（项目级，同名条目覆盖全局）：

```json
{
  "mcpServers": {
    "figma": {
      "command": "npx",
      "args": ["-y", "figma-developer-mcp@latest", "--stdio"],
      "env": {
        "FIGMA_API_KEY": "${FIGMA_API_KEY}"
      }
    }
  }
}
```

### 4. 验证

```sh
# 令牌直测：200 + 账号信息即有效
curl -s -H "X-Figma-Token: $FIGMA_API_KEY" https://api.figma.com/v1/me
```

端到端：用假 fileKey 调 `get_figma_data`——404 = 鉴权已过、仅文件不存在；403 = 令牌无效或权限不足。

## 三、变通做法（不受支持）：官方远程借道

> **先读这段再决定**：Figma 官方仅允许白名单客户端（VS Code、Cursor、Claude Code、Codex、Xcode）注册接入官方远程 MCP，PAT / `X-Figma-Token` 一律无效。以下做法借白名单客户端替本机账号走完 OAuth，再把 token 复用给 deepai——属于绕过客户端准入控制（合规注记见 §六）。此外 `scripts/figma-mcp-token.py` 持久化的是 Claude Code OAuth 客户端的 `client_id` / `client_secret`，续期时以该客户端身份向 Figma 请求 token；且引导依赖 macOS 钥匙串，Linux 无 `security` 命令时不可用。Figma 若收紧校验，此路径随时失效——届时回到 §二。

### 1. 注册并授权（一次性，macOS）

```sh
claude mcp add --transport http figma https://mcp.figma.com/mcp --scope user
claude        # 启动后执行 /mcp → 选 figma → Authenticate → 浏览器点 Allow
```

浏览器 Allow 后，token 铸入 macOS 钥匙串（`Claude Code-credentials` 条目的 `mcpOAuth/figma|*` 路径，含 accessToken / refreshToken / clientId / clientSecret）。

### 2. 凭证托管与续期

`scripts/figma-mcp-token.py`（deepai 仓库内，路径按实际 checkout 位置调整）：

```sh
scripts/figma-mcp-token.py ensure
```

- 首次从钥匙串引导，凭证落盘 `~/.deepai/figma-mcp-token.json`；文件与临时文件自创建起 0600，原子替换
- access token 同步写入 `~/.deepai/.env` 的 `FIGMA_MCP_TOKEN` 行——deepai 启动时注入进程环境，`mcp.json` 的 `${FIGMA_MCP_TOKEN}` 即可解析，无需 shell 导出
- 剩余不足 7 天自动用 refresh_token 续期（实测 refresh_token 可重复使用，无需重新浏览器授权）
- 成功时静默；失败时向 stderr 报错并以非零码退出——`ensure` **不要做输出重定向**，放进 `~/.zshrc` 时保留一行裸调用，让续期失败可见：

```sh
/path/to/deepai/scripts/figma-mcp-token.py ensure
```

子命令：`ensure` / `refresh`（强制续期）/ `status`（查看剩余有效期）。

### 3. MCP 配置

```json
{
  "mcpServers": {
    "figma": {
      "type": "http",
      "url": "https://mcp.figma.com/mcp",
      "headers": {
        "Authorization": "Bearer ${FIGMA_MCP_TOKEN}"
      }
    }
  }
}
```

### 4. 验证

`loaded` 只代表握手成功，token 有效性需单独验证（该端点为无状态模式，不下发 `mcp-session-id`，`tools/list` 直接再发一次即可）：

```sh
curl -s -X POST https://mcp.figma.com/mcp \
  -H "Authorization: Bearer $FIGMA_MCP_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
```

token 存在 `.env` 时可先 `grep ^FIGMA_MCP_TOKEN ~/.deepai/.env` 取值验证。工具列表应出现 `figma__get_design_context`、`figma__get_screenshot`、`figma__generate_image` 等。token 有效期 90 天；仅当 refresh_token 被服务器吊销（refresh 报 400/401）时才需重跑 §三.1 重新授权。

## 四、使用

把 Figma 链接直接贴进对话即可，形如 `https://www.figma.com/design/<fileKey>/...`，可带 `node-id`。官方远程还支持 `get_screenshot`（截图比对）、`generate_figma_design`（写回设计）等。UI 开发工作流见 skill `figma-ui`。

## 五、故障排查

| 现象 | 原因与处理 |
|---|---|
| 社区版调用 403 | 令牌无效/过期，或未勾选 File content 权限，重新生成 |
| 社区版调用 404 | fileKey 或 node-id 不对；令牌可疑先用 `/v1/me` 直测 |
| 官方远程调用 401 | 运行中进程的 token 已过期：重启 shell/deepai（`ensure` 会自动续期）；仍 401 则 `refresh` 强制续期或重跑 §三.1 重新授权 |
| 官方远程启动 failed | `${FIGMA_MCP_TOKEN}` 未展开：跑 `scripts/figma-mcp-token.py ensure` 确认 `~/.deepai/.env` 有 `FIGMA_MCP_TOKEN=` 行；注意 `.env` 不覆盖已有变量，shell 里有旧同名导出时会优先生效 |
| `ensure` 报"无凭证" | 本机没做过 §三.1 授权（或非 macOS 无钥匙串）——按 §二 走 PAT，或先完成授权 |
| npx 首次启动慢 | 首次拉包耗时；追求稳定可固定版本号去掉 `@latest` |

## 六、合规注记

借道白名单客户端铸 token 属于绕过 Figma 的客户端准入控制：token 由本人账号亲自授权、只发往 figma 域名，但官方正道是进 [MCP 客户端 waitlist](https://developers.figma.com/docs/figma-mcp-server/remote-server-installation/)。Figma 若收紧校验（如客户端指纹），此路径随时可能失效——失效时社区版是稳定退路。
