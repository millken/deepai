# DeepAI 基础库 - 个性化 Agent 开发指南

> **愿景**：将 DeepAI 打造成一个模块化、可扩展的 Agent 基础库，开发者可以基于此快速构建各种领域特定的智能助手。

---

## 一、设计理念

### 1.1 核心原则

| 原则 | 说明 | 实践 |
|------|------|------|
| **模块化** | 每个组件独立可用 | 可单独使用 tools/sandbox/memory 等模块 |
| **可组合** | 组件间松耦合，灵活组合 | Agent = LLM + Tools + Memory + Sandbox |
| **可扩展** | 通过接口而非实现编程 | 自定义 Tool/LLMProvider/Memory 只需实现接口 |
| **安全优先** | 默认安全，按需开放 | 沙箱隔离、权限控制、审计日志 |
| **配置驱动** | 行为通过配置定制 | YAML 定义 Agent/工具，减少硬编码 |

### 1.2 架构层次

```
┌─────────────────────────────────────────────────────────────────┐
│                        应用层 (Applications)                      │
│   代码助手 │ 数据分析 │ 客服机器人 │ 研究助手 │ 自动化运维 │ ...    │
├─────────────────────────────────────────────────────────────────┤
│                        框架层 (Framework)                         │
│  ┌─────────────┐  ┌─────────────┐                               │
│  │ Agent Runner│  │ Multi-Agent │                               │
│  │ (单Agent)   │  │ (多Agent)   │                               │
│  └─────────────┘  └─────────────┘                               │
├─────────────────────────────────────────────────────────────────┤
│                        核心层 (Core)                              │
│  ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐        │
│  │ Agent  │ │  LLM   │ │ Tools  │ │ Memory │ │Subagent│        │
│  └────────┘ └────────┘ └────────┘ └────────┘ └────────┘        │
├─────────────────────────────────────────────────────────────────┤
│                        基础设施层 (Infrastructure)                │
│  ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐        │
│  │Sandbox │ │Checkpoint│ │ Vector │ │  MCP   │ │ Proxy  │        │
│  └────────┘ └────────┘ └────────┘ └────────┘ └────────┘        │
└─────────────────────────────────────────────────────────────────┘
```

### 1.3 使用模式

**模式一：直接使用核心模块**
```go
// 只用沙箱执行命令
sb, _ := sandbox.New("session", "/workspace")
result, _ := sb.Exec(ctx, "go test ./...", 30*time.Second)

// 只用工具注册表
registry := tools.NewRegistry()
registry.Register(myTool)
result, _ := registry.Call(ctx, "my_tool", args, nil)
```

**模式二：组装 Agent**
```go
// 组合各组件构建 Agent
agent := agent.New(agent.AgentConfig{
    LLMProvider: llm.NewProvider("openai"),
    Tools:       myToolRegistry,
    Sandbox:     sb,
    Memory:      myMemoryService,
})
```

**模式三：使用完整框架**
```go
// 通过配置文件驱动
app := deepai.NewApp(configs.Load("agents.yaml"))
app.Run(ctx, "my-agent", userMessage)
```

---

## 二、典型个性化 Agent 场景

### 2.1 场景概览

| 场景 | Agent 类型 | 核心能力 | 典型工具 |
|------|-----------|----------|----------|
| **代码助手** | Coder | 代码生成、调试、重构 | bash, file, git, lsp |
| **数据分析** | Analyst | 数据查询、可视化、报告 | sql, chart, pandas |
| **客服机器人** | Support | 意图识别、知识检索、工单 | kb_search, ticket, crm |
| **研究助手** | Researcher | 文献检索、摘要、综合 | search, pdf, note |
| **运维助手** | DevOps | 监控、部署、故障排查 | kubectl, prometheus, log |
| **写作助手** | Writer | 大纲、草稿、润色 | outline, grammar, format |
| **安全审计** | Security | 漏洞扫描、日志分析 | nmap, log_analysis, report |

---

### 2.2 代码助手 (Code Assistant)

**场景描述**：帮助开发者编写、调试、重构代码。

**配置示例**：
```yaml
# configs/agents/code-assistant.yaml
id: code-assistant
name: "Code Assistant"
description: "专业的编程助手，支持多种编程语言"

system_prompt: |
  你是一个专业的编程助手。
  - 遵循最佳实践和设计模式
  - 编写清晰、可维护的代码
  - 添加必要的注释和文档
  - 考虑边界情况和错误处理

model: "anthropic/claude-sonnet-4-6"
max_turns: 20
temperature: 0.1

tools:
  # 文件操作
  - read_file
  - write_file
  - edit_file        # [需新增] 精确编辑
  
  # 代码执行
  - bash             # 运行测试、构建
  - run_code         # [需新增] 安全执行代码片段
  
  # 代码分析
  - grep_code        # [需新增] 代码搜索
  - lsp_go_to_def    # [需新增] LSP 集成
  - git_diff         # [需新增] Git 操作
  
  # 辅助
  - ask_clarification
  - present_file

memory:
  enabled: true
  type: session      # 会话级记忆
  remember_code_style: true  # 记住用户代码风格

sandbox:
  enabled: true
  backend: landlock  # 优先使用 Landlock
  allowed_paths:
    - ${WORKSPACE}
  read_only_paths:
    - /usr
    - /go
```

**自定义工具示例**：
```go
// RunCodeTool 安全执行代码片段
func RunCodeTool() models.Tool {
    return models.Tool{
        Name:        "run_code",
        Description: "在隔离环境中执行代码片段并返回结果",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "language": map[string]any{
                    "type": "string",
                    "enum": []string{"python", "javascript", "go", "bash"},
                },
                "code": map[string]any{
                    "type": "string",
                    "description": "要执行的代码",
                },
                "timeout": map[string]any{
                    "type": "integer",
                    "description": "超时秒数，默认10",
                },
            },
            "required": []string{"language", "code"},
        },
        Handler: runCodeHandler,
    }
}

func runCodeHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
    lang := call.Arguments["language"].(string)
    code := call.Arguments["code"].(string)
    timeout := 10
    if t, ok := call.Arguments["timeout"].(float64); ok {
        timeout = int(t)
    }

    sb := tools.SandboxFromContext(ctx)
    
    // 根据语言选择执行器
    var cmd string
    var filename string
    switch lang {
    case "python":
        filename = "script.py"
        cmd = fmt.Sprintf("python3 %s", filename)
    case "javascript":
        filename = "script.js"
        cmd = fmt.Sprintf("node %s", filename)
    case "go":
        filename = "main.go"
        cmd = fmt.Sprintf("go run %s", filename)
    case "bash":
        cmd = code
    }

    // 写入文件（非bash）
    if filename != "" {
        if err := sb.WriteFile(filename, []byte(code)); err != nil {
            return models.ToolResult{
                CallID:   call.ID,
                ToolName: call.Name,
                Status:   models.CallStatusFailed,
                Error:    err.Error(),
            }, err
        }
    }

    // 执行
    result, err := sb.Exec(ctx, cmd, time.Duration(timeout)*time.Second)
    if err != nil {
        return models.ToolResult{
            CallID:   call.ID,
            ToolName: call.Name,
            Status:   models.CallStatusFailed,
            Error:    err.Error(),
            Content:  result.Stdout + result.Stderr,
        }, err
    }

    return models.ToolResult{
        CallID:   call.ID,
        ToolName: call.Name,
        Status:   models.CallStatusCompleted,
        Content:  result.Stdout,
        Data: map[string]any{
            "exit_code": result.ExitCode,
            "stderr":    result.Stderr,
            "duration":  result.Duration.String(),
        },
    }, nil
}
```

---

### 2.3 数据分析助手 (Data Analyst)

**场景描述**：查询数据库、分析数据、生成可视化报告。

**配置示例**：
```yaml
# configs/agents/data-analyst.yaml
id: data-analyst
name: "Data Analyst"
description: "数据分析和可视化专家"

system_prompt: |
  你是一个数据分析专家。
  - 使用 SQL 查询数据时注重效率
  - 生成清晰的图表和可视化
  - 提供数据驱动的洞察
  - 解释分析方法和结论

model: "anthropic/claude-sonnet-4-6"
max_turns: 15
temperature: 0.1

tools:
  - db_query          # 数据库查询
  - db_schema         # [需新增] 查看表结构
  - python_execute    # [需新增] 执行 Python 分析
  - create_chart      # [需新增] 创建图表
  - export_report     # [需新增] 导出报告
  - read_file
  - write_file
  - ask_clarification

# 数据库连接配置
connections:
  - name: main_db
    type: postgres
    connection_string: ${DATABASE_URL}
    read_only: true
    
  - name: analytics
    type: bigquery
    credentials: ${BIGQUERY_CREDENTIALS}
    read_only: true
```

**自定义工具示例**：
```go
// DBQueryTool 数据库查询工具
func DBQueryTool(name string, db *sql.DB, readOnly bool) models.Tool {
    return models.Tool{
        Name:        fmt.Sprintf("db_query_%s", name),
        Description: fmt.Sprintf("在 %s 数据库上执行 SQL 查询", name),
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "sql": map[string]any{
                    "type":        "string",
                    "description": "SQL 查询语句",
                },
                "limit": map[string]any{
                    "type":        "integer",
                    "description": "返回行数限制，默认100",
                    "default":     100,
                },
            },
            "required": []string{"sql"},
        },
        Groups: []string{"database", name},
        Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
            sqlQuery := call.Arguments["sql"].(string)
            limit := 100
            if l, ok := call.Arguments["limit"].(float64); ok {
                limit = int(l)
            }

            // 安全检查
            if readOnly && !isReadOnlyQuery(sqlQuery) {
                return models.ToolResult{
                    CallID:   call.ID,
                    ToolName: call.Name,
                    Status:   models.CallStatusFailed,
                    Error:    "只读数据库只允许 SELECT 查询",
                }, nil
            }

            // 添加 LIMIT
            if !strings.Contains(strings.ToUpper(sqlQuery), "LIMIT") {
                sqlQuery = fmt.Sprintf("%s LIMIT %d", sqlQuery, limit)
            }

            rows, err := db.QueryContext(ctx, sqlQuery)
            if err != nil {
                return models.ToolResult{
                    CallID:   call.ID,
                    ToolName: call.Name,
                    Status:   models.CallStatusFailed,
                    Error:    err.Error(),
                }, err
            }
            defer rows.Close()

            // 转换为 JSON
            results, _ := rowsToJSON(rows)
            jsonResult, _ := json.MarshalIndent(results, "", "  ")

            return models.ToolResult{
                CallID:   call.ID,
                ToolName: call.Name,
                Status:   models.CallStatusCompleted,
                Content:  string(jsonResult),
                Data: map[string]any{
                    "row_count": len(results),
                    "truncated": len(results) >= limit,
                },
            }, nil
        },
    }
}

func isReadOnlyQuery(sql string) bool {
    sql = strings.ToUpper(strings.TrimSpace(sql))
    return strings.HasPrefix(sql, "SELECT") ||
           strings.HasPrefix(sql, "SHOW") ||
           strings.HasPrefix(sql, "DESCRIBE") ||
           strings.HasPrefix(sql, "EXPLAIN")
}
```

---

### 2.4 客服机器人 (Customer Support)

**场景描述**：回答用户问题、检索知识库、创建工单。

**配置示例**：
```yaml
# configs/agents/customer-support.yaml
id: customer-support
name: "Customer Support"
description: "智能客服助手"

system_prompt: |
  你是一个专业的客服助手。
  - 友好、耐心、专业
  - 优先从知识库寻找答案
  - 无法解决时创建工单
  - 保护用户隐私

model: "anthropic/claude-sonnet-4-6"
max_turns: 10
temperature: 0.2

tools:
  - kb_search         # 知识库搜索
  - kb_article        # [需新增] 获取文章详情
  - create_ticket     # [需新增] 创建工单
  - update_ticket     # [需新增] 更新工单
  - user_lookup       # [需新增] 查询用户信息
  - ask_clarification

memory:
  enabled: true
  type: persistent    # 持久化记忆
  remember_user_preferences: true
  remember_conversation_history: true

# RAG 配置
rag:
  enabled: true
  collection: "support-kb"
  top_k: 5
  threshold: 0.7
```

---

### 2.5 研究助手 (Research Assistant)

**场景描述**：文献检索、信息收集、报告生成。

**配置示例**：
```yaml
# configs/agents/research-assistant.yaml
id: research-assistant
name: "Research Assistant"
description: "学术研究和信息收集助手"

system_prompt: |
  你是一个研究助手。
  - 收集信息时注明来源
  - 区分事实和观点
  - 提供平衡的视角
  - 生成结构化的研究报告

model: "anthropic/claude-sonnet-4-6"
max_turns: 25
temperature: 0.1

tools:
  - web_search        # [需新增] 网页搜索
  - web_fetch         # [需新增] 获取网页内容
  - pdf_extract       # [需新增] PDF 提取
  - note_create       # [需新增] 创建笔记
  - note_search       # [需新增] 搜索笔记
  - read_file
  - write_file
  - ask_clarification
  - task              # 委派子任务

subagents:
  - type: general-purpose
    name: "web-scraper"
    tools: [web_fetch, read_file]
    
  - type: analyst
    name: "data-analyzer"
    tools: [python_execute, create_chart]
```

---

### 2.6 运维助手 (DevOps Assistant)

**场景描述**：监控、部署、故障排查。

**配置示例**：
```yaml
# configs/agents/devops-assistant.yaml
id: devops-assistant
name: "DevOps Assistant"
description: "运维和故障排查助手"

system_prompt: |
  你是一个运维专家。
  - 执行操作前确认影响范围
  - 优先使用只读命令诊断
  - 记录所有操作
  - 提供根本原因分析

model: "anthropic/claude-sonnet-4-6"
max_turns: 20
temperature: 0.05  # 更低温度，减少误操作

tools:
  # Kubernetes
  - kubectl_get
  - kubectl_describe
  - kubectl_logs
  - kubectl_exec
  
  # 监控
  - prometheus_query
  - grafana_dashboard
  
  # 日志
  - log_search
  - log_analyze
  
  # 云服务
  - aws_cli
  - gcloud_cli
  
  # 通用
  - bash
  - read_file
  - ask_clarification

# 权限控制
permissions:
  read_only: false
  require_confirmation:
    - kubectl_delete
    - kubectl_scale
    - aws_cli
  audit_all: true
```

---

## 三、项目现状分析

### 1.1 已完成的核心能力

| 模块 | 路径 | 功能描述 | 成熟度 |
|------|------|----------|--------|
| **Agent 核心** | `pkg/agent/` | ReAct 循环、多轮对话、工具调用 | ✅ 生产就绪 |
| **LLM 抽象** | `pkg/llm/` | 多 Provider 支持、流式输出、工具调用 | ✅ 生产就绪 |
| **工具系统** | `pkg/tools/` | 注册、校验、沙箱执行、权限控制 | ✅ 生产就绪 |
| **沙箱隔离** | `pkg/sandbox/` | Landlock/bwrap/direct 三级隔离 | ✅ 生产就绪 |
| **子代理池** | `pkg/subagent/` | 并发任务、事件流、超时控制 | ⚠️ 基础完成 |
| **澄清机制** | `pkg/clarification/` | 多轮问答、选项确认 | ✅ 生产就绪 |
| **记忆系统** | `pkg/memory/` | 会话记忆、事实提取、持久化 | ⚠️ 基础完成 |
| **MCP 客户端** | `pkg/mcp/` | Stdio/SSE/HTTP 传输、工具发现 | ✅ 生产就绪 |
| **API 网关** | `pkg/gateway/` | HTTP API、会话管理、Postgres 存储 | ✅ 生产就绪 |
| **代理日志** | `pkg/proxy/` | 请求/响应日志、事件存储 | ✅ 生产就绪 |

### 1.2 已支持的 LLM Provider

```
- OpenAI (通过 litellm)
- Anthropic (通过 litellm)
- SiliconFlow (通过 litellm)
- 其他 OpenAI 兼容 API
```

### 1.3 内置工具

| 工具名 | 功能 | 沙箱隔离 |
|--------|------|----------|
| `bash` | Shell 命令执行 | ✅ |
| `read_file` | 读取文件 | ✅ |
| `write_file` | 写入文件 | ✅ |
| `view_image` | 图像分析 | ❌ |
| `task` | 子代理任务委派 | ❌ |
| `ask_clarification` | 用户澄清 | ❌ |
| `present_file` | 文件呈现 | ❌ |

### 1.4 内置 Agent 类型

| 类型 | 系统提示侧重 | 默认工具 | 最大轮次 |
|------|-------------|----------|----------|
| `general-purpose` | 平衡助手 | 全部 | 8 |
| `researcher` | 证据收集与综合 | read/glob/present/clarify/task | 10 |
| `coder` | 代码生成与调试 | bash/file/glob/present/clarify/task | 12 |
| `analyst` | 结构化分析 | file/glob/present/clarify | 10 |

---

## 二、打造个性化 Agent 的扩展点

### 2.1 Agent 类型扩展

**现状**：系统预定义了 4 种 Agent 类型，定义在 `pkg/agent/types_config.go`。

**扩展方式**：

```go
// 1. 定义新的 Agent 类型
const AgentTypeMyAgent AgentType = "my-custom-agent"

// 2. 配置 Agent 行为
var myAgentConfig = agent.AgentTypeConfig{
    Type:         AgentTypeMyAgent,
    Name:         "My Custom Agent",
    Description:  "专注于特定领域的智能助手",
    SystemPrompt: "你是一个专业的XX领域助手...",
    DefaultTools: []string{"bash", "read_file", "my_custom_tool"},
    MaxTurns:     15,
    Temperature:  0.1,
}

// 3. 注册到系统
agent.BuiltinAgentTypes[AgentTypeMyAgent] = myAgentConfig
```

**待完成工作**：
- [ ] 支持从配置文件（YAML/JSON）动态加载 Agent 类型
- [ ] 支持 Agent 类型的热重载
- [ ] 支持 Agent 类型的版本管理

---

### 2.2 自定义工具开发

**现状**：工具通过 `models.Tool` 结构定义，需要实现 `ToolHandler` 函数。

**扩展示例**：

```go
// 定义工具
func MyCustomTool() models.Tool {
    return models.Tool{
        Name:        "my_custom_tool",
        Description: "执行特定领域的操作",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "query": map[string]any{
                    "type":        "string",
                    "description": "查询内容",
                },
            },
            "required": []string{"query"},
        },
        Groups:  []string{"custom"},
        Handler: myCustomToolHandler,
    }
}

// 实现处理器
func myCustomToolHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
    query, _ := call.Arguments["query"].(string)

    // 从上下文获取沙箱（如果需要）
    sandbox := tools.SandboxFromContext(ctx)

    // 执行业务逻辑
    result := doSomething(query)

    return models.ToolResult{
        CallID:   call.ID,
        ToolName: call.Name,
        Status:   models.CallStatusCompleted,
        Content:  result,
    }, nil
}

// 注册工具
registry.Register(MyCustomTool())
```

**待完成工作**：
- [ ] 工具的配置化定义（通过 YAML/JSON）
- [ ] 工具的权限分级与访问控制
- [ ] 工具执行的审计日志
- [ ] 工具市场/插件系统
- [ ] 远程工具（通过 HTTP/gRPC 调用）

---

### 2.3 记忆系统增强

**现状**：`pkg/memory/` 提供了基础的会话记忆和事实提取功能。

**当前结构**：
```go
type Document struct {
    SessionID string
    User      UserMemory    // 工作上下文、个人偏好
    History   HistoryMemory // 历史交互摘要
    Facts     []Fact        // 提取的事实
}
```

**待完成工作**：
- [ ] **向量化记忆存储**：集成向量数据库（如 pgvector、Milvus）
- [ ] **长期记忆**：跨会话的持久化记忆
- [ ] **记忆检索**：基于语义相似度的记忆召回
- [ ] **记忆压缩**：自动摘要和遗忘机制
- [ ] **个性化画像**：用户偏好学习和存储

---

### 2.4 LLM Provider 扩展

**现状**：通过 `litellm` 支持多种 Provider，也可直接实现 `LLMProvider` 接口。

**接口定义**：
```go
type LLMProvider interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
    Stream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}
```

**待完成工作**：
- [ ] 本地模型支持（Ollama、vLLM）
- [ ] 多模型负载均衡
- [ ] 模型路由（根据任务类型选择模型）
- [ ] 成本监控与限制
- [ ] 响应缓存

---

### 2.5 子代理（Subagent）系统

**现状**：`pkg/subagent/` 提供了基础的子任务池和执行器。

**待完成工作**：
- [ ] 子代理的独立工具集配置
- [ ] 子代理间的通信机制
- [ ] 层级任务分解
- [ ] 子代理结果聚合策略
- [ ] 分布式子代理执行

---

### 2.6 MCP 工具集成

**现状**：`pkg/mcp/` 支持 Stdio/SSE/HTTP 三种传输方式连接 MCP 服务器。

**使用方式**：
```go
// 连接 MCP 服务器
client, err := mcp.ConnectStdio(ctx, "my-server", "/path/to/server", nil, "--arg1")
if err != nil {
    log.Fatal(err)
}
defer client.Close()

// 获取工具并注册
mcpTools, err := client.Tools(ctx)
for _, tool := range mcpTools {
    registry.Register(tool)
}
```

**待完成工作**：
- [ ] MCP 服务器的配置化管理
- [ ] MCP 工具的自动发现和注册
- [ ] MCP 服务器的健康检查和重连
- [ ] MCP 工具的权限控制

---

## 三、推荐开发优先级

### 第一阶段：基础设施（1-2 周）

1. **配置系统重构**
   - 支持从 YAML 文件加载 Agent 类型配置
   - 支持从 YAML 文件加载工具定义
   - 环境变量覆盖配置

2. **日志与可观测性**
   - 结构化日志（slog）
   - 指标收集（Prometheus）
   - 分布式追踪（OpenTelemetry）

3. **错误处理增强**
   - 统一错误码
   - 重试策略
   - 熔断机制

### 第二阶段：核心能力增强（2-3 周）

1. **记忆系统升级**
   - 集成 pgvector 进行向量存储
   - 实现语义检索
   - 添加记忆压缩策略

2. **工具系统扩展**
   - HTTP 工具（远程 API 调用）
   - 数据库工具（查询执行）
   - 文档处理工具（PDF、Word）

3. **LLM 能力增强**
   - 多模型路由
   - 响应缓存
   - 流式输出的中断/恢复

### 第三阶段：个性化能力（2-3 周）

1. **用户画像系统**
   - 偏好学习
   - 行为分析
   - 个性化推荐

2. **领域知识库**
   - 知识库管理
   - RAG 检索增强
   - 知识更新

3. **多模态支持**
   - 图像理解（已有基础）
   - 语音输入/输出
   - 文档解析

### 第四阶段：生产化（1-2 周）

1. **安全加固**
   - 工具权限分级
   - 敏感数据脱敏
   - 审计日志

2. **性能优化**
   - 并发控制
   - 资源限制
   - 缓存策略

3. **部署支持**
   - Docker 镜像
   - Kubernetes 部署
   - 配置管理

---

## 四、架构改进建议

### 4.1 插件化架构

```
┌─────────────────────────────────────────────────────────┐
│                    Gateway / API Layer                   │
├─────────────────────────────────────────────────────────┤
│                      Agent Runtime                       │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐    │
│  │ Agent 1 │  │ Agent 2 │  │ Agent 3 │  │ Agent N │    │
│  └────┬────┘  └────┬────┘  └────┬────┘  └────┬────┘    │
├───────┴────────────┴────────────┴────────────┴─────────┤
│                    Plugin System                         │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌─────────┐ │
│  │ToolPlugin│  │LLMPlugin │  │MemPlugin │  │MCPPlugin│ │
│  └──────────┘  └──────────┘  └──────────┘  └─────────┘ │
├─────────────────────────────────────────────────────────┤
│                    Infrastructure                        │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌─────────┐ │
│  │ Sandbox  │  │Checkpoint│  │  Vector  │  │  Cache  │ │
│  └──────────┘  └──────────┘  └──────────┘  └─────────┘ │
└─────────────────────────────────────────────────────────┘
```

### 4.2 配置文件结构建议

```yaml
# agents.yaml
agents:
  - id: my-custom-agent
    name: "My Custom Agent"
    description: "专注于XX领域的智能助手"
    system_prompt: |
      你是一个专业的XX领域助手...
    model: "anthropic/claude-sonnet-4-6"
    max_turns: 15
    temperature: 0.1
    tools:
      - bash
      - read_file
      - write_file
      - my_custom_tool
    plugins:
      - name: knowledge-base
        config:
          collection: "my-domain"
      - name: memory
        config:
          enable_long_term: true

# tools.yaml
tools:
  - name: my_custom_tool
    description: "执行特定领域的操作"
    type: http
    config:
      endpoint: "https://api.example.com/execute"
      method: POST
      timeout: 30s
      auth:
        type: bearer
        secret_ref: MY_API_KEY
    input_schema:
      type: object
      properties:
        query:
          type: string
          description: "查询内容"
      required: ["query"]
```

### 4.3 目录结构建议

```
deepai/
├── cmd/
│   ├── deepai/          # 主程序入口
│   ├── gateway/         # API 网关
│   └── proxy/           # LLM 代理
├── pkg/
│   ├── agent/           # Agent 核心
│   ├── llm/             # LLM 抽象
│   ├── tools/           # 工具系统
│   ├── sandbox/         # 沙箱隔离
│   ├── subagent/        # 子代理
│   ├── memory/          # 记忆系统
│   ├── clarification/   # 澄清机制
│   ├── mcp/             # MCP 客户端
│   ├── gateway/         # HTTP 网关
│   ├── proxy/           # API 代理
│   ├── checkpoint/      # 状态持久化
│   ├── plugin/          # [新增] 插件系统
│   ├── vector/          # [新增] 向量存储
│   ├── rag/             # [新增] RAG 检索
│   └── config/          # [新增] 配置管理
├── configs/             # [新增] 配置文件
│   ├── agents.yaml
│   ├── tools.yaml
│   └── providers.yaml
├── plugins/             # [新增] 插件目录
│   ├── tools/
│   ├── llm/
│   └── memory/
└── docs/
```

---

## 五、技术选型建议

| 领域 | 推荐 | 理由 |
|------|------|------|
| 向量数据库 | pgvector | 与现有 Postgres 集成，运维简单 |
| 配置管理 | caarlos0/env + koanf | 环境变量优先，支持多源 |
| 插件系统 | go-plugin | HashiCorp 成熟方案 |
| 结构化日志 | slog | 标准库，性能好 |
| 指标收集 | Prometheus client | 生态成熟 |
| 分布式追踪 | OpenTelemetry | 行业标准 |
| 缓存 | ristretto | 高性能本地缓存 |

---

## 六、快速开始示例

### 6.1 创建一个简单的自定义 Agent

```go
package main

import (
    "context"
    "log"

    "github.com/millken/deepai/pkg/agent"
    "github.com/millken/deepai/pkg/llm"
    "github.com/millken/deepai/pkg/models"
    "github.com/millken/deepai/pkg/sandbox"
    "github.com/millken/deepai/pkg/tools"
    "github.com/millken/deepai/pkg/tools/builtin"
)

func main() {
    ctx := context.Background()

    // 1. 创建沙箱
    sb, err := sandbox.New("my-agent", "/tmp/my-agent-workspace")
    if err != nil {
        log.Fatal(err)
    }
    defer sb.Close()

    // 2. 创建工具注册表
    registry := tools.NewRegistry()
    registry.Register(builtin.BashTool())
    for _, tool := range builtin.FileTools() {
        registry.Register(tool)
    }

    // 3. 创建 LLM Provider
    provider := llm.NewProvider("openai") // 需要 OPENAI_API_KEY

    // 4. 创建 Agent
    myAgent := agent.New(agent.AgentConfig{
        LLMProvider: provider,
        Tools:       registry,
        Sandbox:     sb,
        AgentType:   agent.AgentTypeCoder,
        Model:       "gpt-4o",
        MaxTurns:    10,
        SystemPrompt: "你是一个专业的Go开发助手，帮助用户编写高质量的Go代码。",
    })

    // 5. 运行 Agent
    result, err := myAgent.Run(ctx, "session-1", []models.Message{{
        ID:        "m1",
        SessionID: "session-1",
        Role:      models.RoleHuman,
        Content:   "帮我写一个HTTP服务器",
    }})
    if err != nil {
        log.Fatal(err)
    }

    println(result.FinalOutput)
}
```

### 6.2 添加自定义工具

```go
// 定义一个查询数据库的工具
func DatabaseQueryTool(db *sql.DB) models.Tool {
    return models.Tool{
        Name:        "db_query",
        Description: "执行SQL查询并返回结果",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "sql": map[string]any{
                    "type":        "string",
                    "description": "SQL查询语句（只读）",
                },
            },
            "required": []string{"sql"},
        },
        Groups: []string{"database", "readonly"},
        Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
            sql, _ := call.Arguments["sql"].(string)

            // 安全校验：只允许SELECT
            if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
                return models.ToolResult{
                    CallID:   call.ID,
                    ToolName: call.Name,
                    Status:   models.CallStatusFailed,
                    Error:    "只允许执行SELECT查询",
                }, nil
            }

            rows, err := db.QueryContext(ctx, sql)
            if err != nil {
                return models.ToolResult{
                    CallID:   call.ID,
                    ToolName: call.Name,
                    Status:   models.CallStatusFailed,
                    Error:    err.Error(),
                }, err
            }
            defer rows.Close()

            // 转换结果为JSON
            results := []map[string]any{}
            columns, _ := rows.Columns()
            for rows.Next() {
                values := make([]any, len(columns))
                pointers := make([]any, len(columns))
                for i := range values {
                    pointers[i] = &values[i]
                }
                rows.Scan(pointers...)
                row := map[string]any{}
                for i, col := range columns {
                    row[col] = values[i]
                }
                results = append(results, row)
            }

            jsonResult, _ := json.Marshal(results)
            return models.ToolResult{
                CallID:   call.ID,
                ToolName: call.Name,
                Status:   models.CallStatusCompleted,
                Content:  string(jsonResult),
            }, nil
        },
    }
}
```

---

## 七、基础库 API 设计建议

### 7.1 核心接口抽象

作为基础库，需要提供清晰、稳定的接口抽象：

```go
// pkg/core/interfaces.go

// AgentRuntime Agent 运行时接口
type AgentRuntime interface {
    // Run 执行一次对话
    Run(ctx context.Context, sessionID string, messages []Message) (*RunResult, error)
    
    // Events 返回事件通道（用于流式输出）
    Events() <-chan AgentEvent
    
    // Stop 停止当前执行
    Stop(reason string) error
}

// ToolExecutor 工具执行器接口
type ToolExecutor interface {
    // Register 注册工具
    Register(tool Tool) error
    
    // Execute 执行工具调用
    Execute(ctx context.Context, call ToolCall) (ToolResult, error)
    
    // List 列出可用工具
    List() []Tool
    
    // Restrict 限制可用工具集
    Restrict(allowed []string) ToolExecutor
}

// MemoryStore 记忆存储接口
type MemoryStore interface {
    // Load 加载会话记忆
    Load(ctx context.Context, sessionID string) (*MemoryDocument, error)
    
    // Save 保存会话记忆
    Save(ctx context.Context, doc *MemoryDocument) error
    
    // Search 语义检索记忆
    Search(ctx context.Context, query string, opts SearchOptions) ([]MemoryItem, error)
}

// LLMBackend LLM 后端接口
type LLMBackend interface {
    // Chat 同步对话
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    
    // Stream 流式对话
    Stream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error)
    
    // Close 关闭连接
    Close() error
}

// SandboxExecutor 沙箱执行器接口
type SandboxExecutor interface {
    // Exec 执行命令
    Exec(ctx context.Context, cmd string, timeout time.Duration) (*ExecResult, error)
    
    // ReadFile 读取文件
    ReadFile(path string) ([]byte, error)
    
    // WriteFile 写入文件
    WriteFile(path string, data []byte) error
    
    // Close 关闭沙箱
    Close() error
}
```

### 7.2 Builder 模式构建

提供流畅的 Builder API：

```go
// pkg/core/builder.go

// AgentBuilder Agent 构建器
type AgentBuilder struct {
    config AgentConfig
}

func NewAgent() *AgentBuilder {
    return &AgentBuilder{
        config: AgentConfig{
            MaxTurns:    8,
            Temperature: ptr(0.1),
        },
    }
}

// 设置模型
func (b *AgentBuilder) WithModel(provider, model string) *AgentBuilder {
    b.config.Provider = provider
    b.config.Model = model
    return b
}

// 设置系统提示
func (b *AgentBuilder) WithSystemPrompt(prompt string) *AgentBuilder {
    b.config.SystemPrompt = prompt
    return b
}

// 添加工具
func (b *AgentBuilder) WithTools(tools ...Tool) *AgentBuilder {
    if b.config.Tools == nil {
        b.config.Tools = NewToolRegistry()
    }
    for _, t := range tools {
        b.config.Tools.Register(t)
    }
    return b
}

// 设置沙箱
func (b *AgentBuilder) WithSandbox(sb SandboxExecutor) *AgentBuilder {
    b.config.Sandbox = sb
    return b
}

// 设置记忆
func (b *AgentBuilder) WithMemory(store MemoryStore) *AgentBuilder {
    b.config.Memory = store
    return b
}

// 设置最大轮次
func (b *AgentBuilder) WithMaxTurns(n int) *AgentBuilder {
    b.config.MaxTurns = n
    return b
}

// 构建
func (b *AgentBuilder) Build() (AgentRuntime, error) {
    return NewAgentRuntime(b.config)
}

// 使用示例
agent, err := core.NewAgent().
    WithModel("openai", "gpt-4o").
    WithSystemPrompt("你是一个专业的编程助手").
    WithTools(builtin.BashTool(), builtin.FileTools()...).
    WithSandbox(sandbox.New("session", "/workspace")).
    WithMaxTurns(15).
    Build()
```

### 7.3 工具定义 DSL

提供简洁的工具定义方式：

```go
// pkg/tools/dsl.go

// DefineTool 简化的工具定义
func DefineTool(name, description string, schema any, handler ToolHandler) Tool {
    return Tool{
        Name:        name,
        Description: description,
        InputSchema: schema,
        Handler:     handler,
    }
}

// ToolFunc 工具函数签名
type ToolFunc func(ctx context.Context, args map[string]any) (string, error)

// SimpleTool 创建简单工具
func SimpleTool(name, description string, fn ToolFunc) Tool {
    return Tool{
        Name:        name,
        Description: description,
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "input": map[string]any{
                    "type": "string",
                },
            },
        },
        Handler: func(ctx context.Context, call ToolCall) (ToolResult, error) {
            input, _ := call.Arguments["input"].(string)
            result, err := fn(ctx, map[string]any{"input": input})
            return ToolResult{
                CallID:   call.ID,
                ToolName: call.Name,
                Status:   CallStatusCompleted,
                Content:  result,
            }, err
        },
    }
}

// 使用示例
var WeatherTool = tools.DefineTool(
    "get_weather",
    "获取指定城市的天气信息",
    map[string]any{
        "type": "object",
        "properties": map[string]any{
            "city": map[string]any{
                "type":        "string",
                "description": "城市名称",
            },
            "unit": map[string]any{
                "type":        "string",
                "enum":        []string{"celsius", "fahrenheit"},
                "default":     "celsius",
            },
        },
        "required": []string{"city"},
    },
    func(ctx context.Context, call ToolCall) (ToolResult, error) {
        city := call.Arguments["city"].(string)
        // ... 调用天气 API
        return ToolResult{
            CallID:   call.ID,
            ToolName: "get_weather",
            Status:   CallStatusCompleted,
            Content:  fmt.Sprintf("%s 天气: 晴, 25°C", city),
        }, nil
    },
)
```

### 7.4 配置加载器

支持从多种格式加载配置：

```go
// pkg/config/loader.go

type AgentConfigLoader struct {
    paths []string
}

func LoadAgentConfig(path string) (*AgentConfig, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    
    var cfg AgentConfig
    switch filepath.Ext(path) {
    case ".yaml", ".yml":
        err = yaml.Unmarshal(data, &cfg)
    case ".json":
        err = json.Unmarshal(data, &cfg)
    default:
        return nil, fmt.Errorf("unsupported config format: %s", path)
    }
    
    // 环境变量替换
    cfg = expandEnvVars(cfg)
    
    // 验证
    if err := cfg.Validate(); err != nil {
        return nil, err
    }
    
    return &cfg, nil
}

// 从目录加载所有 Agent
func LoadAgentConfigs(dir string) (map[string]*AgentConfig, error) {
    files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
    if err != nil {
        return nil, err
    }
    
    configs := make(map[string]*AgentConfig)
    for _, f := range files {
        cfg, err := LoadAgentConfig(f)
        if err != nil {
            return nil, fmt.Errorf("load %s: %w", f, err)
        }
        configs[cfg.ID] = cfg
    }
    return configs, nil
}
```

---

## 八、扩展开发指南

### 8.1 开发自定义工具

**步骤一：定义工具结构**

```go
// plugins/tools/my_tool.go
package mytools

import (
    "context"
    "github.com/millken/deepai/pkg/models"
)

type MyToolConfig struct {
    APIKey     string `json:"api_key" yaml:"api_key"`
    Endpoint   string `json:"endpoint" yaml:"endpoint"`
    Timeout    int    `json:"timeout" yaml:"timeout"`
}

func NewMyTool(cfg MyToolConfig) models.Tool {
    return models.Tool{
        Name:        "my_tool",
        Description: "我的自定义工具",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "action": map[string]any{
                    "type":        "string",
                    "description": "操作类型",
                    "enum":        []string{"create", "read", "update", "delete"},
                },
                "data": map[string]any{
                    "type":        "object",
                    "description": "操作数据",
                },
            },
            "required": []string{"action"},
        },
        Groups: []string{"custom", "my-domain"},
        Handler: &myToolHandler{cfg: cfg},
    }
}

type myToolHandler struct {
    cfg MyToolConfig
}

func (h *myToolHandler) Execute(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
    action := call.Arguments["action"].(string)
    
    // 实现业务逻辑
    switch action {
    case "create":
        return h.handleCreate(ctx, call)
    case "read":
        return h.handleRead(ctx, call)
    default:
        return models.ToolResult{
            CallID:   call.ID,
            ToolName: call.Name,
            Status:   models.CallStatusFailed,
            Error:    fmt.Sprintf("未知操作: %s", action),
        }, nil
    }
}
```

**步骤二：注册工具**

```go
// 在应用启动时注册
registry := tools.NewRegistry()

// 从配置加载工具设置
var cfg MyToolConfig
yaml.Unmarshal(configData, &cfg)

// 注册
registry.Register(NewMyTool(cfg))
```

### 8.2 开发自定义 LLM Provider

```go
// plugins/llm/my_provider.go
package myllm

import (
    "context"
    "github.com/millken/deepai/pkg/llm"
    "github.com/millken/deepai/pkg/models"
)

type MyProvider struct {
    apiKey   string
    baseURL  string
    client   *http.Client
}

func NewMyProvider(apiKey, baseURL string) *MyProvider {
    return &MyProvider{
        apiKey:  apiKey,
        baseURL: baseURL,
        client:  &http.Client{Timeout: 60 * time.Second},
    }
}

func (p *MyProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
    // 1. 转换请求格式
    // 2. 调用 API
    // 3. 转换响应格式
    // ...
}

func (p *MyProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
    ch := make(chan llm.StreamChunk, 64)
    
    go func() {
        defer close(ch)
        
        // 实现流式调用
        // for each chunk {
        //     ch <- llm.StreamChunk{Delta: "..."}
        // }
    }()
    
    return ch, nil
}

// 注册到工厂
func init() {
    llm.RegisterProvider("my-provider", func(cfg map[string]any) (llm.LLMProvider, error) {
        apiKey := cfg["api_key"].(string)
        baseURL := cfg["base_url"].(string)
        return NewMyProvider(apiKey, baseURL), nil
    })
}
```

### 8.3 开发自定义记忆存储

```go
// plugins/memory/redis_store.go
package mymemory

import (
    "context"
    "encoding/json"
    "time"
    
    "github.com/redis/go-redis/v9"
    "github.com/millken/deepai/pkg/memory"
)

type RedisStore struct {
    client *redis.Client
    ttl    time.Duration
}

func NewRedisStore(addr string, ttl time.Duration) *RedisStore {
    return &RedisStore{
        client: redis.NewClient(&redis.Options{Addr: addr}),
        ttl:    ttl,
    }
}

func (s *RedisStore) Load(ctx context.Context, sessionID string) (memory.Document, error) {
    key := "memory:" + sessionID
    data, err := s.client.Get(ctx, key).Bytes()
    if err == redis.Nil {
        return memory.Document{SessionID: sessionID}, nil
    }
    if err != nil {
        return memory.Document{}, err
    }
    
    var doc memory.Document
    if err := json.Unmarshal(data, &doc); err != nil {
        return memory.Document{}, err
    }
    return doc, nil
}

func (s *RedisStore) Save(ctx context.Context, doc memory.Document) error {
    key := "memory:" + doc.SessionID
    data, err := json.Marshal(doc)
    if err != nil {
        return err
    }
    return s.client.Set(ctx, key, data, s.ttl).Err()
}
```

---

## 九、总结与路线图

### 9.1 基础库成熟度目标

| 阶段 | 目标 | 关键指标 |
|------|------|----------|
| **MVP** | 核心功能可用 | Agent/Tools/LLM 稳定 |
| **v1.0** | 生产就绪 | 完善文档、测试覆盖 >80% |
| **v1.5** | 易于扩展 | 配置驱动、插件系统 |
| **v2.0** | 生态完善 | 工具市场、社区贡献 |

### 9.2 优先级排序

**P0 - 必须完成（MVP）**
- [ ] 稳定的核心 API（Agent/Tools/LLM）
- [ ] 完善的错误处理
- [ ] 基础文档和示例
- [ ] 单元测试覆盖

**P1 - 重要（v1.0）**
- [ ] YAML 配置支持
- [ ] 记忆系统增强（向量检索）
- [ ] 更多内置工具
- [ ] 性能优化

**P2 - 期望（v1.5）**
- [ ] 插件系统
- [ ] Web 管理界面
- [ ] 监控指标
- [ ] 分布式支持

### 9.3 成功因素

| 因素 | 说明 |
|------|------|
| **接口稳定** | 保持向后兼容，版本化变更 |
| **文档完善** | API 文档、教程、示例 |
| **易于上手** | 快速开始 < 5 分钟 |
| **可观测性** | 日志、指标、追踪 |
| **社区友好** | 贡献指南、Issue 模板 |

---

*文档版本: 2.0*
*最后更新: 2026-04-03*
*适用范围: DeepAI 基础库*
