**安装与运行指南**

**目标**
本指南说明如何在本地或容器中构建、配置并启动 deerflow-go 项目的主要服务（LangGraph/Gateway/Agent），以及常见故障排查步骤。

**前提条件**
- 操作系统：Linux（推荐内核支持 Landlock，如果需要 sandbox 安全隔离）
- Go 1.20+（请按项目 go.mod 指定的版本）
- PostgreSQL（如果要启用持久化 memory/checkpoint）
- 可选：bubblewrap (`bwrap`)（用于 sandbox），Docker（可选容器部署）

**环境变量（常用）**
- `DATABASE_URL`：Postgres 连接字符串，例如 `postgres://user:pass@localhost:5432/deerflow?sslmode=disable`
- `EINO_API_KEY` / `OPENAI_API_KEY`：LLM 提供者需要的 API Key
- `PORT` 或二进制的 `--addr` 参数：监听地址（例 `:8080`）
- 其他：`MCP_*` 前缀的配置（当使用 MCP 外部工具时）

把常用环境变量写入 `.env` 文件方便使用（注意不要提交到代码仓库）。

**配置自定义模型与 API Key**
- 支持的主要环境变量与含义：
  - `DEFAULT_LLM_PROVIDER`：默认提供者名称（例如 `openai`、`siliconflow`、`anthropic`）。
  - `DEFAULT_LLM_MODEL`：默认模型名称（例如 `qwen/Qwen3.5-9B` 或 `gpt-4.1-mini`）。
  - `OPENAI_API_KEY`：用于 OpenAI 或兼容网关的 API Key。
  - `OPENAI_API_BASE_URL`：自建 OpenAI 兼容网关地址（例如内部代理）。
  - `SILICONFLOW_API_KEY`：SiliconFlow 提供者的 Key。
  - `ANTHROPIC_API_KEY`：Anthropic 提供者的 Key（若使用 Anthropic，需要 `OPENAI_API_BASE_URL` 指向兼容网关）。

- 优先级规则：
  1. 命令行参数优先（例如 `--model` 用于覆盖模型名）。
  2. 运行时通过代码传入的 provider 名称优先（某些入口会直接传入 provider 名称）。
  3. 环境变量 `DEFAULT_LLM_PROVIDER` / `DEFAULT_LLM_MODEL` 作为默认值。

- 重要实现细节与注意事项：
  - 程序通过 `pkg/llm` 构建 LLM 提供者；`NewProvider(name)` 会使用传入的 `name` 或读取 `DEFAULT_LLM_PROVIDER`。
  - `pkg/llm/eino.go` 会从环境读取具体 Key 与 Base URL；如果 Key 为空会返回错误并使启动失败。请确保在启动前设置相应环境变量。
  - LangGraph 兼容服务在 `pkg/langgraphcompat/compat.go` 中目前直接调用 `llm.NewProvider("siliconflow")`（硬编码）。如果你想改为读取 `DEFAULT_LLM_PROVIDER`，需要修改该文件或让我替你修改。

  注意：仓库已更新以移除 LangGraph 层对 provider 的硬编码。`pkg/langgraphcompat` 现在会优先读取环境变量 `DEFAULT_LLM_PROVIDER`，若未设置则回退到 `siliconflow`。因此你可以通过设置 `DEFAULT_LLM_PROVIDER` 控制 LangGraph 使用的后端提供者，例如：

  ```
  export DEFAULT_LLM_PROVIDER=openai
  export OPENAI_API_KEY=sk-...
  ./bin/langgraph --addr :8080 --model "gpt-4.1-mini"
  ```

  如果需要我把这次代码变更记录到变更日志或 README 中，我可以继续添加。

- 示例 `.env`（加入到项目根）示例：
```
SILICONFLOW_API_KEY=sk-xxxx
OPENAI_API_KEY=sk-xxxx
OPENAI_API_BASE_URL=https://api.your-gateway.local/v1
DEFAULT_LLM_PROVIDER=siliconflow
DEFAULT_LLM_MODEL=qwen/Qwen3.5-9B
```

- 启动示例（优先使用命令行覆盖模型）：
```
./bin/langgraph --addr :8080 --model "qwen/Qwen3.5-9B"
```

- Docker / docker-compose：`docker-compose.yml` 已将 `SILICONFLOW_API_KEY` 与 `DEFAULT_LLM_MODEL` 作为必填或可选环境变量，可通过 `.env` 或宿主环境注入。

- 故障排查小贴士：
  - 启动失败提示 `api key is not set`：检查对应提供者的 API Key 环境变量是否正确命名并可见给进程（`env | grep KEY`）。
  - 使用自建 OpenAI 兼容网关时，确保 `OPENAI_API_BASE_URL` 指向正确的网关地址并且网关支持 Eino 所需接口。
  - 若希望全局切换默认提供者到 `openai`/`siliconflow`，设置 `DEFAULT_LLM_PROVIDER` 并重启服务；注意 LangGraph 兼容层可能仍被硬编码为 `siliconflow`。

**本地构建（推荐）**
1. 获取代码：
```
git clone <repo-url>
cd deerflow-go
```
2. 下载依赖并整理模块：
```
go mod download
make tidy
```
3. 构建（两种方式）：
- 使用 Makefile（若存在）：
```
make build
```
  这通常会在 `bin/` 下生成可执行文件。
- 或直接构建某个可执行项：
```
go build -o bin/langgraph ./cmd/langgraph
go build -o bin/gateway ./cmd/gateway
go build -o bin/agent ./cmd/agent
```

**数据库迁移与初始化**
1. 启动本地 Postgres（或使用已有数据库），并导出 `DATABASE_URL`：
```
export DATABASE_URL=postgres://user:pass@localhost:5432/deerflow?sslmode=disable
```
2. 运行迁移（使用 `cmd/checkpoint`）：
```
go run ./cmd/checkpoint migrate
```
如果你已经构建了 `bin/checkpoint`，也可执行 `./bin/checkpoint migrate`。

**运行服务（示例）**

- 运行 LangGraph 兼容服务：
```
./bin/langgraph --addr :8080 --model "qwen/Qwen3.5-9B"
# 或：
go run ./cmd/langgraph --addr :8080 --model "qwen/Qwen3.5-9B"
```

- 运行 Gateway 服务：
```
./bin/gateway --addr :8080
# 或：
go run ./cmd/gateway --addr :8080
```

- 运行 Agent CLI/服务（示例）：
```
./bin/agent serve --addr :9090
# 或一次性运行一个任务：
go run ./cmd/agent run --help
```

注意：命令行选项请参见对应入口文件的 `--help` 输出。

**使用 Docker**
1. 构建镜像：
```
docker build -t deerflow-go .
```
2. 运行（使用 `.env` 传参）：
```
docker run -p 8080:8080 --env-file .env deerflow-go
```
3. 使用 `docker-compose`（若仓库提供 `docker-compose.yml`）：
```
docker-compose up -d
```

**运行测试**
```
make test
# 或逐包运行：
go test ./...
```

**常见故障排查**
- 无法连接数据库：检查 `DATABASE_URL`、Postgres 是否启动、端口与防火墙。
- bwrap 不可用导致 sandbox 退回 direct：确认系统安装了 `bubblewrap`，或在不安全环境下调整配置。
- LLM 相关错误：确认 `EINO_API_KEY` / `OPENAI_API_KEY` 是否正确，网络能访问目标 API。
- 端口被占用：修改 `--addr` 参数或 `PORT` 环境变量。

**安全与生产部署建议**
- 在生产环境启用 Landlock 或 bwrap 来限制 sandbox 权限。
- 对工具输入严格校验，避免命令注入与路径逃逸。
- 使用独立的 Postgres 实例并配置备份与监控。
- 对外暴露的 HTTP 接口启用鉴权与速率限制。

**后续工作（可选）**
- 为 `pkg/sandbox` 与内置工具编写详细安全配置示例。
- 提供一个本地 dev-compose（memory+stub-llm+bwrap 模拟）的集成测试环境。

---
文档位置: [docs/INSTALL_AND_RUN.md](docs/INSTALL_AND_RUN.md)
