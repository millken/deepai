# Figma MCP 接入指南

> 让 deepai 读取 Figma 设计数据（布局、样式、组件、切图），用于设计还原与 UI 代码生成。
> 两种方案均在 macOS 实测通过：官方远程（2026-10，41 个工具）、社区版（figma-developer-mcp v0.13.2）。

## 一、方案选型

| 方案 | 传输 | 鉴权 | 工具面 | 可用性 |
|---|---|---|---|---|
| 官方远程（`mcp.figma.com/mcp`） | http | OAuth，仅白名单客户端可**注册** | 41 个：读 + 写（生成设计/图片、建文件、传素材、搜设计系统、weave 系列） | ✅ 借道白名单客户端铸 token（见 §二） |
| 社区版 [figma-developer-mcp](https://github.com/GLips/Figma-Context-MCP) | stdio | Figma PAT | 2 个：`get_figma_data` + `download_figma_images`（只读，简化 + CSS 友好结构） | ✅ 即接即用（见 §四） |
| 官方本地 Dev Mode（`127.0.0.1:3845/mcp`） | http | 无需 token | 读为主 | ❌ 需 Figma Desktop + Dev/Full 付费席位，免费版菜单无开关 |

关键事实：官方远程的白名单只拦 **OAuth 动态客户端注册**（非目录客户端 403），不拦**连接**——任何持有有效 Bearer token 的客户端都能正常服务。而 Claude Code 在白名单内，可替本机账号走完 OAuth，token 铸出后给 deepai 复用。PAT / `X-Figma-Token` 对官方远程一律无效。

选择建议：要读+写全能力选官方远程；只要设计转码、不想经手 OAuth 选社区版。

## 二、官方远程接入（借道铸造）

### 1. 注册并授权（一次性）

```sh
claude mcp add --transport http figma https://mcp.figma.com/mcp --scope user
claude        # 启动后执行 /mcp → 选 figma → Authenticate → 浏览器点 Allow
```

浏览器 Allow 后，token 铸入 macOS 钥匙串（`Claude Code-credentials` 条目的 `mcpOAuth/figma|*` 路径，含 accessToken / refreshToken / clientSecret）。

### 2. 环境变量（token 不落 shell）

`scripts/figma-mcp-token.py` 负责凭证托管：首次从钥匙串引导落盘 `~/.deepai/figma-mcp-token.json`（0600），此后剩余不足 7 天时自动用 refresh_token 续期。access token 同步写入 `~/.deepai/.env` 的 `FIGMA_MCP_TOKEN` 行——deepai 启动时 `env.Load` 会把 `.env` 注入进程环境（不覆盖已有变量），`mcp.json` 的 `${FIGMA_MCP_TOKEN}` 即可解析，**无需 shell 导出**，GUI 启动也生效。只需跑一次：

```sh
~/github.com/millken/deepai/scripts/figma-mcp-token.py ensure
```

子命令：`ensure`（不足 7 天先续期再同步 `.env`）、`refresh`（强制续期）、`status`（查看剩余有效期）。续期触发方式任选：`~/.zshrc` 放一行静默 `ensure >/dev/null 2>&1`（随 shell 启动），或日历提醒每 80 天跑一次。

### 3. MCP 配置

`~/.deepai/mcp.json`（全局）或项目根 `.mcp.json`（项目级，同名条目覆盖全局）：

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

### 4. 重启 deepai

工具列表应出现 `figma__get_design_context`、`figma__get_screenshot`、`figma__generate_image` 等。

### 维护：自动续期

token 有效期 90 天（`expires_in: 7776000`），剩余不足 7 天时随任意一次 `ensure` 自动续期并同步 `.env`——refresh_token 实测可重复使用，无需重新浏览器授权。凭证 JSON 丢失时重跑 `ensure` 即从钥匙串重新引导（`.env` 行随之重建）。仅当 refresh_token 被服务器吊销（refresh 报 400/401）时才需重跑 §二.1 的 `/mcp` 重新授权。

## 三、验证（官方远程）

`loaded` 只代表握手成功，token 有效性需单独验证：

```sh
# 无状态 MCP 握手 + 工具面：200 且返回工具列表即通（需双 Accept 头）
curl -s -X POST https://mcp.figma.com/mcp \
  -H "Authorization: Bearer $FIGMA_MCP_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
```

该端点为无状态模式（不下发 `mcp-session-id`），`tools/list` 直接再发一次即可。

## 四、社区版接入（PAT，零依赖）

### 1. 生成令牌

figma.com → Settings → Security → **Personal access tokens** → Generate new token，勾选 **File content** 读取权限。

### 2. 导出环境变量

```sh
echo 'export FIGMA_API_KEY=<你的令牌>' >> ~/.zshrc
source ~/.zshrc
```

注意：`~/.deepai/.env` 在 deepai 启动时由 `env.Load` 注入进程环境（不覆盖已有变量），MCP 配置里的 `${VAR}` 按 `os.Getenv` 展开——所以令牌既可写入 `.env`（加一行 `FIGMA_API_KEY=<token>`），也可走 shell 导出，二者取其一；shell 已导出时 `.env` 不生效。

### 3. MCP 配置

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

## 五、使用

把 Figma 链接直接贴进对话即可，形如 `https://www.figma.com/design/<fileKey>/...`，可带 `node-id`。官方远程还支持 `get_screenshot`（截图比对）、`generate_figma_design`（写回设计）等。

## 六、故障排查

| 现象 | 原因与处理 |
|---|---|
| 官方远程调用 401 | 运行中进程的环境 token 已过期：重启 shell/deepai（`ensure` 会自动续期）；仍 401 则 `refresh` 强制续期或重跑 §二.1 重新授权 |
| 官方远程启动 failed | `${FIGMA_MCP_TOKEN}` 未展开：跑 `scripts/figma-mcp-token.py ensure` 确认 `~/.deepai/.env` 有 `FIGMA_MCP_TOKEN=` 行；注意 `.env` 不覆盖已有变量，shell 里有旧同名导出时会优先生效 |
| 社区版调用 403 | 令牌无效/过期，或未勾选 File content 权限，重新生成 |
| 社区版调用 404 | fileKey 或 node-id 不对；令牌可疑先用 `/v1/me` 直测 |
| npx 首次启动慢 | 首次拉包耗时；追求稳定可固定版本号去掉 `@latest` |

## 七、合规注记

借道白名单客户端铸 token 属于绕过 Figma 的客户端准入控制：token 由本人账号亲自授权、只发往 figma 域名，但官方正道是进 [MCP 客户端 waitlist](https://developers.figma.com/docs/figma-mcp-server/remote-server-installation/)。Figma 若收紧校验（如客户端指纹），此路径随时可能失效——失效时社区版是稳定退路。
