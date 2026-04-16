# deepai-go 包指南

方法说明：我通过公共 GitHub API 检查了相关仓库的元数据与近期活动，并查看了 `pkg.go.dev` 的搜索/结果页面来判断包的可见性。原始请求包含 `gh search`，但在此环境中 `gh` 未经认证，无法直接查询 GitHub，因此未使用 `gh`。

## 1. LLM 客户端
**推荐：cloudwego/eino-ext**

- GitHub: https://github.com/cloudwego/eino-ext
- 推荐理由：
  - 这是我当前找到的最合适的 Go 方案，作为单一多提供商客户端层，已包含对 OpenAI 与 Claude 的集成，并支持流式与工具调用功能。
  - OpenAI 适配器支持自定义 `BaseURL`，这使得像 SiliconFlow 这样的兼容 OpenAI 的提供商无需分叉客户端即可使用。
  - 比起轻量单厂商 SDK，它更适合 deepai-go，因为你的项目需要同时支持多个提供商。
  - Eino 生态活跃且面向生产环境，而我看到的其他统一 Go LLM SDK 要么规模更小，要么过于新。
- 证据：
  - `cloudwego/eino-ext` 的 README 列出了对 OpenAI 与 Claude 的 ChatModel 支持。
  - `components/model/openai` 支持流式、工具调用和 `BaseURL`。
  - `components/model/claude` 支持流式和工具调用。
- 维护状态：
  - 所有者：`cloudwego`
  - Stars：640
  - 最近提交：2026-03-26
- 注意事项：
  - 这是一个生态级别的包，不是微型 SDK，你将接受 Eino 的抽象与设计。
  - 若只想要最瘦的厂商 SDK，可考虑 `openai/openai-go` 与 `anthropics/anthropic-sdk-go`，但它们无法优雅地解决多提供商统一层的问题。

## 2. Agent 框架
**推荐：cloudwego/eino**

- GitHub: https://github.com/cloudwego/eino
- 推荐理由：
  - 这是我发现的最有说服力的生产级 Go agent 框架，既活跃又被广泛采用。
  - 提供一流的 agent/tool 抽象、工作流组合、流式支持、多 agent 模式以及与 MCP 的集成，且都在同一生态内。
  - 相比 GitHub 上那些小型 Go agent 项目，它在生产就绪度上更有优势。
  - 若希望采用更 Go 原生的模式并依赖更活跃的生态，Eino 比 `langchaingo` 更合适。
- 证据：
  - README 展示了 ADK 风格的 agent、工具使用、图组合、流式、人类介入与子 agent 模式。
  - 在围绕 Go AI 框架的搜索中，Eino 多次出现。
- 维护状态：
  - 所有者：`cloudwego`
  - Stars：10,298
  - 最近提交：2026-03-27
- 注意事项：
  - 相较于 Cobra/pgx 级别的成熟基础库，Eino 更年轻，随着生态成熟可能会有 API 变动。
  - 文档总体不错，但生态中部分内容在中文示例/文档中更常见。

## 3. 沙箱 / 进程隔离
**推荐：opencontainers/runc/libcontainer**

- GitHub: https://github.com/opencontainers/runc/tree/main/libcontainer
- 推荐理由：
  - 若需要真正的进程隔离（命名空间、cgroup、capabilities、文件系统控制），这是我找到的最成熟的 Go 基础库。
  - 相比轻量的“安全 exec”封装，它更严肃——这是容器风格隔离背后的库，而不是仅仅封装 `exec.Command`。
  - 它是唯一明确覆盖生产级沙箱难点的选项。
- 证据：
  - `libcontainer` README 明确说明了命名空间、cgroup、capabilities 与文件系统访问控制。
  - `pkg.go.dev` 搜索 `libcontainer` 指向 `opencontainers/runc/libcontainer`。
- 维护状态：
  - 所有者：`opencontainers`
  - Stars（`opencontainers/runc`）：13,150
  - 最近提交：2026-03-28
- 注意事项：
  - 该方案比简单的 exec 辅助更重，且以 Linux/容器为中心。
  - 如果 deepai-go 仅需超时与基本 RLIMIT，标准库 `os/exec` 配合 `golang.org/x/sys/unix` 会更简单。
  - 我未发现单一轻量库能同时优雅地提供超时、内存限制、syscall 过滤与文件系统隔离。

## 4. MCP 客户端
**推荐：mark3labs/mcp-go**

- GitHub: https://github.com/mark3labs/mcp-go
- 推荐理由：
  - 目前看起来这是面向客户端使用最成熟、实用的 Go MCP 实现。
  - 在我检查的 `pkg.go.dev` 搜索结果与 GitHub stars 上，它都领先于官方 Go SDK。
  - 支持 stdio、SSE 以及可流式的 HTTP 传输，已被 Go MCP 生态广泛使用。
- 证据：
  - 在 `model context protocol go` 的 `pkg.go.dev` 搜索中，`mark3labs/mcp-go/mcp` 出现在 `modelcontextprotocol/go-sdk/mcp` 之前。
  - README 描述了 stdio、SSE 与可流式 HTTP 传输。
- 维护状态：
  - 所有者：`mark3labs`
  - Stars：8,476
  - 最近提交：2026-03-26
- 注意事项：
  - 官方 SDK `modelcontextprotocol/go-sdk` 是更贴近规范的选择，正在快速改进中。
  - 若你更看重与上游 MCP 语义的严格一致性，应关注官方 SDK。

## 5. Postgres
**推荐：jackc/pgx/v5**

- GitHub: https://github.com/jackc/pgx
- 推荐理由：
  - 这是目前针对 Postgres 的标准且成熟的 Go 选择。
  - 提供原生 Postgres 支持、更优的性能特性、一流的连接池（`pgxpool`），以及对 Postgres 特性的更好访问，相比 `sqlx` 更贴近底层驱动。
  - `sqlx` 仍是有用的辅助层，但它本身并不是驱动或池的替代。
- 证据：
  - `pkg.go.dev` 对 `pgx` 和 `postgres pgx` 的搜索结果中，`github.com/jackc/pgx/v5` 排在前列。
  - `sqlx` 在历史 stars 上更高，但近期活动相对较少。
- 维护状态：
  - 所有者：`jackc`
  - Stars：13,532
  - 最近提交：2026-03-22
- 注意事项：
  - `pgx` 比 ORM 更底层，会需要编写更多显式 SQL。
  - 若想保留 `database/sql` 的便利，可在 `pgx/stdlib` 之上叠加 `sqlx`，但我不建议把 `sqlx` 作为核心依赖。

## 6. SSE / HTTP 流式
**推荐：Go 标准库 `net/http`**

- GitHub: https://github.com/golang/go/tree/master/src/net/http
- 推荐理由：
  - 对于服务器端的 SSE，通常不需要额外依赖：使用 `net/http`，正确设置头并 flush 即可。
  - 这是 Go 生态中最经过考验的 HTTP 实现。
  - 对于 deepai-go，可使流式路径保持简单、可观测且依赖少。
- 证据：
  - `pkg.go.dev/net/http` 显示其为标准库（发布于 2026-03-06），被 1,769,979 个包导入。
  - 关于 SSE 的包搜索也出现了若干基于 `net/http` 的辅助库，但没有一个比直接使用标准库更合适用于服务器端流。
- 维护状态：
  - 所有者：`golang`
  - Stars（`golang/go`）：133,185
  - 最近提交：2026-03-28
- 注意事项：
  - 这是实现建议，不是第三方 SSE 框架的推荐。
  - 若同时需要 SSE 客户端库或消息代理抽象，可考虑 `r3labs/sse/v2`，但对简单服务器流我不建议默认引入它。

## 7. CLI 框架
**推荐：spf13/cobra**

- GitHub: https://github.com/spf13/cobra
- 推荐理由：
  - 对于需要子命令、帮助生成、Shell 完成等功能的生产级 Go CLI，它仍然是最稳妥的默认选择。
  - 拥有最深的生态采用度与大量可复用的示例。
  - 虽然 `urfave/cli` 也不错且仍然活跃，但对于更大型的运维/开发者 CLI，Cobra 仍是“稳妥选择”。
- 证据：
  - `pkg.go.dev` 的 `cobra` 搜索结果将 `github.com/spf13/cobra` 列为首选。
  - 针对更广泛的“cli framework”查询 `urfave/cli/v3` 可能排在前面，但在生产惯例与生态重力方面，Cobra 更有优势。
- 维护状态：
  - 所有者：`spf13`
  - Stars：43,524
  - 最近提交：2025-12-10
- 注意事项：
  - Cobra 较比极简的 flag 驱动 CLI 更重。
  - 如果 deepai-go 最终只有非常小的命令面，标准库 `flag` 或 `urfave/cli` 会更加轻量。

## 8. 配置 / 环境变量
**推荐：caarlos0/env/v11**

- GitHub: https://github.com/caarlos0/env
- 推荐理由：
  - 对于以环境变量为优先的配置方案，这是我找到的最清晰、专注的库：小巧、无额外依赖、维护活跃，且为基于结构体的环境解析而设计。
  - 当不需要大型多源配置系统时，它比 `viper` 更合适。
  - 相较于较老的 `envconfig` 类库，`caarlos0/env` 的维护更为活跃，API 也更直观。
- 证据：
  - `pkg.go.dev` 搜索 `caarlos0 env` 时，`github.com/caarlos0/env/v11` 排在首位。
  - 虽然 `kelseyhightower/envconfig` 仍会出现在 `envconfig` 的搜索中，但我更推荐 `caarlos0/env`。
- 维护状态：
  - 所有者：`caarlos0`
  - Stars：6,067
  - 最近提交：2026-03-01
- 注意事项：
  - 此库仅专注于环境变量解析。
  - 若未来需要从文件、命令行、环境和远程存储分层加载配置，`koanf` 是更好的扩展路径。

## 总结

如果今天要为 deepai-go 选型，我会选择以下栈：

- LLM 客户端：`cloudwego/eino-ext`
- Agent 框架：`cloudwego/eino`
- 沙箱：`opencontainers/runc/libcontainer`
- MCP 客户端：`mark3labs/mcp-go`
- Postgres：`jackc/pgx/v5`
- SSE：`net/http`
- CLI：`spf13/cobra`
- 配置/环境：`caarlos0/env/v11`
