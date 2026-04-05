# DeepAI Skill 系统设计

> 版本：2.1
> 日期：2026-04-04
> 参考：[Claude Code Skills 官方文档](https://code.claude.com/docs/zh-CN/skills)

## 一、设计理念

### 1.1 核心认知

**Skill 不是新的执行单元，而是给 Agent 的"专业手册"。**

```
Skill = YAML frontmatter（配置） + Markdown body（指令）
```

一个 `SKILL.md` 文件包含两部分：
- **Frontmatter**：告诉系统**何时**使用这个 Skill、**如何**配置运行环境
- **Markdown body**：告诉 Agent **做什么**、**怎么做**

| 维度 | Tool | Skill |
|------|------|-------|
| **本质** | 原子操作（Go 函数） | 领域知识 + 指令（Markdown） |
| **形态** | JSON Schema + Handler | YAML frontmatter + Markdown |
| **加载** | 启动时全量注册 | 渐进式（description → 正文 → 资源） |
| **触发** | Agent 自主决策调用 | 用户 `/name` 或 LLM 基于 description 自主判断 |
| **举例** | `bash`、`read_file` | `code-review`、`deploy`、`refactor` |

### 1.2 设计原则

1. **单文件定义** — 一个 SKILL.md 搞定一切，不拆分
2. **LLM 驱动触发** — 不用正则/pattern，description 写好就行
3. **Markdown 即指令** — 不需要 Workflow 引擎，LLM 按 Markdown 步骤执行
4. **渐进式加载** — description 常驻上下文，正文触发时加载，资源按需读取
5. **兼容 Agent Skills 开放标准** — 与 Claude Code 格式对齐，便于生态共享

### 1.3 与现有架构的关系

```
┌─────────────────────────────────────────────────────┐
│  SKILL.md（一个文件）                                 │
│  ┌─────────────────┐  ┌──────────────────────────┐  │
│  │ Frontmatter     │  │ Markdown body            │  │
│  │ - name          │  │ - 领域知识               │  │
│  │ - description   │  │ - 工作步骤               │  │
│  │ - allowed-tools │  │ - 检查清单               │  │
│  │ - context: fork │  │ - 输出模板               │  │
│  │                 │  │ - $ARGUMENTS 参数替换    │  │
│  │                 │  │ - !`command` 动态注入    │  │
│  └─────────────────┘  └──────────────────────────┘  │
└─────────────────────────────────────────────────────┘
          │                        │
          ▼                        ▼
   配置运行环境              注入为 SystemPrompt
   (工具/模型/subagent)     (Agent 按指令执行)
          │                        │
          └──────────┬─────────────┘
                     ▼
┌─────────────────────────────────────────────────────┐
│  现有 Agent（不变）                                    │
│  LLM + Tools + Memory + Sandbox                      │
└─────────────────────────────────────────────────────┘
```

---

## 二、SKILL.md 规范

### 2.1 文件格式

每个 Skill 是一个目录，入口为 `SKILL.md`：

```
skills/
├── code-review/
│   ├── SKILL.md                # 必需：frontmatter + 指令
│   ├── scripts/
│   │   └── security-scan.py    # 可选：辅助脚本
│   ├── templates/
│   │   └── review-report.md    # 可选：输出模板
│   └── references/
│       └── owasp-top10.md      # 可选：参考资料
│
├── deploy/
│   └── SKILL.md
│
└── api-conventions/
    └── SKILL.md
```

`SKILL.md` 结构：**YAML frontmatter** + **Markdown body**

```markdown
---
frontmatter 字段...
---

Markdown 指令内容...
```

### 2.2 Frontmatter 参考

所有字段均为可选。`description` 推荐填写，LLM 依赖它判断何时触发。

#### 标准字段（与 Claude Code 对齐）

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 显示名称，自动映射为 `/slash-command`。仅小写字母、数字、连字符，最多 64 字符 |
| `description` | string | 功能描述 + 使用场景。LLM 据此判断何时触发。前置关键用例，截断 250 字符。省略时取 Markdown body 第一段 |
| `argument-hint` | string | 自动补全提示，如 `[issue-number]` 或 `[filename] [format]` |
| `disable-model-invocation` | bool | `true` = 禁止 LLM 自动触发，仅用户 `/name` 手动调用。适用于有副作用的操作（deploy、commit） |
| `user-invocable` | bool | `false` = 从 `/` 菜单隐藏，仅 LLM 自动触发。适用于背景知识 |
| `allowed-tools` | list/string | Skill 活跃时的**免审批工具列表**。注意：不限制 Agent 可用工具集（Agent 仍可使用所有已注册工具），仅对这些工具跳过权限确认步骤。支持通配符：`Bash(gh *)` 匹配所有 gh 子命令。list: `["Read", "Grep"]`，string: `"Read Grep Glob"` |
| `model` | string | 覆盖默认模型 |
| `effort` | string | 工作量级别：`low` / `medium` / `high` / `max` |
| `context` | string | 设为 `fork` 在独立 subagent 中运行，不污染主对话 |
| `agent` | string | `context: fork` 时使用的 subagent 类型 |
| `hooks` | list | 限定于此 Skill 生命周期的 hooks。复用 Plugin 系统的 Hook 机制（`pkg/plugin/hook_plugin.go`），仅在 Skill 执行期间激活 |
| `paths` | list/string | Glob 模式，限定自动激活的文件范围 |
| `shell` | string | `` !`command` `` 使用的 shell：`bash`（默认）或 `powershell` |

#### DeepAI 扩展字段

| 字段 | 类型 | 说明 |
|------|------|------|
| `max-turns` | int | 覆盖 Agent 最大轮次 |
| `temperature` | float | 覆盖 Agent 温度 |

#### 触发控制矩阵

| Frontmatter | 用户可调用 | LLM 可调用 | description 是否常驻上下文 |
|---|---|---|---|
| （默认） | 是 | 是 | 是 |
| `disable-model-invocation: true` | 是 | **否** | **否** |
| `user-invocable: false` | **否** | 是 | 是 |

### 2.3 字符串替换

| 变量 | 说明 |
|------|------|
| `$ARGUMENTS` | 调用时传递的所有参数 |
| `$ARGUMENTS[N]` / `$N` | 按位置访问参数（0 基索引） |
| `${SESSION_ID}` | 当前会话 ID |
| `${SKILL_DIR}` | Skill 目录路径（引用脚本/模板时使用） |

若内容中不存在 `$ARGUMENTS`，参数会作为 `ARGUMENTS: <value>` 追加到末尾。

### 2.4 动态上下文注入

`` !`<command>` `` 语法在发送给 LLM **之前**执行 shell 命令，输出替换占位符：

```markdown
---
name: pr-summary
description: 总结 Pull Request 变更
context: fork
agent: Explore
allowed-tools: ["Bash(gh *)"]
---

## PR 上下文
- Diff: !`gh pr diff`
- 评论: !`gh pr view --comments`
- 变更文件: !`gh pr diff --name-only`

## 任务
总结此 PR 的主要变更...
```

这是**预处理**，不是 Claude 执行的内容。Claude 只看到最终替换后的结果。

**处理顺序**：先替换 `$ARGUMENTS` 等变量，再执行 `` !`command` ``。因此 `` !`gh issue view $ARGUMENTS` `` 可以正确引用参数。

### 安全：命令注入防护

`!`command`` 中嵌入 `$ARGUMENTS` 存在命令注入风险（如用户输入 `123; rm -rf /`）。

**防护策略**：`$ARGUMENTS` 进入 `` !`command` `` 时**必须通过环境变量传递**，不直接拼接命令字符串。

```
# 错误（命令注入风险）：
!`gh issue view $ARGUMENTS`

# 正确（通过环境变量传递）：
!`gh issue view "$SKILL_ARG_0"`
```

**实现规则**：
1. 解析 `` !`command` `` 时，提取其中的 `$ARGUMENTS` / `$N` 引用
2. 将参数值写入环境变量：`SKILL_ARG_0`、`SKILL_ARG_1`、`SKILL_ARGS`（全部参数）
3. 将命令中的 `$ARGUMENTS` 替换为 `"$SKILL_ARGS"`，`$N` 替换为 `"$SKILL_ARG_N"`
4. 通过 `exec.Command` 的 `Env` 传递环境变量，不拼接字符串
5. 设置命令超时（默认 30s），超时后 kill 进程

```go
// injectDynamicContext 安全实现
func (e *Executor) injectDynamicContext(ctx context.Context, content string, skill *Skill) (string, error) {
    // 正则匹配 !`...`
    // 对每个匹配：
    //   1. 提取 $ARGUMENTS / $N 引用
    //   2. 参数值写入 env：SKILL_ARG_0, SKILL_ARG_1, SKILL_ARGS
    //   3. 命令中的 $N → "$SKILL_ARG_N"
    //   4. exec.CommandContext(ctx, shell, "-c", cmd).WithEnv(env)
    //   5. 超时 30s，超时 kill
}
```

**禁止操作**：`!`command` `` 中不得直接拼接用户输入到命令字符串。检测到直接拼接时拒绝执行并报错。

### 2.5 支持文件

SKILL.md 保持在 **500 行以下**。详细资料放到同目录下的支持文件：

```
my-skill/
├── SKILL.md           # 必需：概览和导航
├── reference.md       # 详细 API 文档（按需加载）
├── examples.md        # 使用示例（按需加载）
└── scripts/
    └── helper.py      # 可执行脚本
```

在 SKILL.md 中用 Markdown 链接引用，Claude 知道何时加载：

```markdown
## 参考资料
- 完整 API 详情见 [reference.md](reference.md)
- 使用示例见 [examples.md](examples.md)
```

---

## 三、Skill 类型

### 3.1 参考内容（Reference）

注入领域知识，Agent 应用于当前工作。LLM 自动触发。

```markdown
---
name: api-conventions
description: API 设计规范。编写 API 端点时自动应用
---

编写 API 端点时：
- 使用 RESTful 命名
- 返回一致的错误格式
- 包含请求验证
```

### 3.2 任务内容（Task）

提供特定操作的分步指令。通常用户手动触发。

```markdown
---
name: deploy
description: 部署应用到生产环境
disable-model-invocation: true
---

部署 $ARGUMENTS：

1. 运行测试套件
2. 构建应用
3. 推送到部署目标
4. 验证部署成功
```

### 3.3 研究任务（Research）

在隔离 subagent 中运行研究，不污染主对话。

```markdown
---
name: deep-research
description: 深度研究某个主题
context: fork
agent: Explore
---

深入研究 $ARGUMENTS：

1. 使用 Glob 和 Grep 查找相关文件
2. 阅读和分析代码
3. 汇总发现，引用具体文件路径
```

---

## 四、完整示例

### 4.1 code-review（代码审查）

```markdown
---
name: code-review
description: 综合代码审查，覆盖安全、性能、质量和可维护性。当用户要求审查代码、review PR 时使用
user-invocable: true
allowed-tools: ["Read", "Grep", "Glob", "Bash"]
---

## 审查流程

按以下四个维度依次审查：

### 1. 安全审查
- 检查认证和授权逻辑
- 识别 SQL 注入、XSS、CSRF 漏洞
- 检查硬编码的密钥和 Token
- 验证输入验证和输出编码

### 2. 性能分析
- 识别 O(n²) 及以上算法
- 检查 N+1 查询问题
- 审查内存分配模式
- 审查并发和锁的使用

### 3. 代码质量
- 命名规范（变量、函数、类型）
- 函数复杂度（单函数不超过 50 行）
- 错误处理完整性
- 注释和文档质量

### 4. 可维护性
- 测试覆盖率评估
- 模块耦合度分析
- 依赖关系合理性

## 输出格式

使用 [templates/review-report.md](templates/review-report.md) 模板生成报告。

## 注意事项

- 只报告事实，不做主观评价
- 每个发现必须标注具体代码位置
- 按严重程度排序：🔴 严重 > 🟡 警告 > 🟢 建议
```

### 4.2 fix-issue（修复 GitHub Issue）

```markdown
---
name: fix-issue
description: 修复指定的 GitHub issue
disable-model-invocation: true
argument-hint: "[issue-number]"
---

修复 GitHub issue $ARGUMENTS，遵循项目编码规范。

1. 读取 issue 描述：!`gh issue view "$SKILL_ARG_0"`
2. 理解需求
3. 实现修复
4. 编写测试
5. 创建 commit
```

### 4.3 pr-summary（PR 摘要，subagent 隔离）

```markdown
---
name: pr-summary
description: 总结 Pull Request 的主要变更
context: fork
agent: Explore
allowed-tools: ["Bash(gh *)"]
---

## PR 上下文
- Diff: !`gh pr diff`
- 评论: !`gh pr view --comments`
- 变更文件: !`gh pr diff --name-only`

## 任务
总结此 Pull Request 的主要变更，包括：
1. 变更概述
2. 影响范围
3. 潜在风险
4. 建议的测试重点
```

### 4.4 migrate-component（带位置参数）

```markdown
---
name: migrate-component
description: 将组件从一个框架迁移到另一个框架
---

将 $0 组件从 $1 迁移到 $2。

保持所有现有行为和测试不变。

迁移步骤：
1. 分析 $0 组件在 $1 中的实现
2. 识别 $2 的等效 API
3. 重写组件
4. 更新所有引用
5. 运行测试验证
```

调用：`/migrate-component SearchBar React Vue`

### 4.5 deploy（仅用户触发）

```markdown
---
name: deploy
description: 部署应用到生产环境
disable-model-invocation: true
---

部署 $ARGUMENTS：

1. 运行测试：!`go test ./...`
2. 构建：!`go build -o bin/app ./cmd/app`
3. 推送镜像：!`docker push "$SKILL_ARGS"`
4. 验证健康检查：!`curl -f http://"$SKILL_ARGS"/health`
```

---

## 五、渐进式加载

### 5.1 三级加载

| 级别 | 时机 | Token 开销 | 内容 |
|------|------|-----------|------|
| **description** | 系统启动时 | ~100 tokens/skill | name + description（常驻上下文） |
| **SKILL.md 正文** | Skill 触发时 | <5k tokens | 完整 frontmatter + Markdown body |
| **支持文件** | 按需 | 无上限 | scripts/templates/references |

### 5.2 加载流程

```
系统启动
  │
  ▼
扫描 skills/ 目录，解析所有 SKILL.md 的 frontmatter
  → 非 disable-model-invocation 的 description 常驻上下文
  → 注入到系统提示（告知 LLM 有哪些 Skill 可用）
  │
用户输入 "/code-review" 或 LLM 判断匹配
  │
  ▼
处理字符串替换（$ARGUMENTS、$N）
执行 !`command` 动态注入
  → 生成完整的 prompt（替换后的 Markdown body）
  │
构建运行环境
  → allowed-tools → 免审批工具列表
  → context: fork → subagent 隔离
  → model / effort / temperature → 覆盖配置
  │
Agent 执行（按 Markdown 指令工作）
  │
需要参考文件时
  │
  ▼
按需读取 references/templates/scripts
```

---

## 六、核心接口设计

### 6.1 类型定义

```go
// pkg/skill/types.go

package skill

// Skill 技能定义
type Skill struct {
    Dir      string     // Skill 目录路径
    Meta     Frontmatter // Frontmatter 元数据
    Body     string     // Markdown body（触发后加载）
    Loaded   bool       // 正文是否已加载
}

// Frontmatter YAML frontmatter 字段
type Frontmatter struct {
    Name                   string   `yaml:"name"`
    Description            string   `yaml:"description"`
    ArgumentHint           string   `yaml:"argument-hint"`
    DisableModelInvocation bool     `yaml:"disable-model-invocation"`
    UserInvocable          *bool    `yaml:"user-invocable"` // nil = true
    AllowedTools           []string `yaml:"allowed-tools"`
    Model                  string   `yaml:"model"`
    Effort                 string   `yaml:"effort"`
    Context                string   `yaml:"context"` // "" | "fork"
    Agent                  string   `yaml:"agent"`
    Hooks                  []Hook   `yaml:"hooks"`
    Paths                  []string `yaml:"paths"`
    Shell                  string   `yaml:"shell"`

    // DeepAI 扩展
    MaxTurns    *int     `yaml:"max-turns,omitempty"`     // nil = 不覆盖
    Temperature *float64 `yaml:"temperature,omitempty"`   // nil = 不覆盖
}

// Hook Skill 级 hook 定义
type Hook struct {
    Event     string        `yaml:"event"`               // PreToolUse, PostToolUse, etc.
    Command   string        `yaml:"command"`
    OnError   HookErrorPolicy `yaml:"on_error,omitempty"` // continue | abort
    Timeout   time.Duration `yaml:"timeout,omitempty"`    // 0 = 无超时
}

type HookErrorPolicy string

const (
    HookErrorContinue HookErrorPolicy = "continue" // 记录错误，继续执行
    HookErrorAbort    HookErrorPolicy = "abort"    // 中止 Skill 执行
)

// IsUserInvocable 返回是否用户可调用（nil 视为 true）
func (f *Frontmatter) IsUserInvocable() bool {
    if f.UserInvocable == nil {
        return true
    }
    return *f.UserInvocable
}

// IsAutoInvocable 返回 LLM 是否可自动调用
func (f *Frontmatter) IsAutoInvocable() bool {
    return !f.DisableModelInvocation
}
```

### 6.2 Registry

```go
// pkg/skill/registry.go

package skill

type Registry struct {
    mu     sync.RWMutex
    skills map[string]*Skill   // name -> Skill
}

// LoadFromDir 扫描目录，解析所有 SKILL.md 的 frontmatter
// 不加载 Markdown body（延迟加载）
func (r *Registry) LoadFromDir(dir string) error

// Get 获取 Skill
func (r *Registry) Get(name string) (*Skill, bool)

// ResolveCommand 解析 /command 格式输入
// 仅处理显式命令，语义匹配由 LLM 自主完成
func (r *Registry) ResolveCommand(input string) (skillName string, args string, ok bool)

// Descriptions 生成注入系统提示的 description 列表
// 排除 disable-model-invocation: true 的 Skill
// context budget：上下文窗口的 1%，回退 8000 字符
// 每个 description 截断 250 字符，前置关键用例
func (r *Registry) Descriptions() string
// 输出格式：
// 可用技能：
// - /code-review: 综合代码审查，覆盖安全、性能...
// - /refactor: 基于 Martin Fowler 方法论的代码重构

// LoadBody 加载 Skill 的 Markdown body（第二级）
func (r *Registry) LoadBody(ctx context.Context, name string) (string, error)
```

### 6.3 Executor

```go
// pkg/skill/executor.go

package skill

type Executor struct {
    registry     *Registry
    toolRegistry *tools.Registry
    agentFactory agent.Factory // 创建 Agent 的工厂
}

// Execute 执行 Skill
func (e *Executor) Execute(ctx context.Context, name string, args string) error {
    skill, _ := e.registry.Get(name)

    // 1. 加载 body
    body, err := e.registry.LoadBody(ctx, name)
    if err != nil {
        return fmt.Errorf("load skill body %s: %w", name, err)
    }

    // 2. 字符串替换
    rendered := e.render(body, args, skill)

    // 3. 动态上下文注入（!`command`）
    rendered, err = e.injectDynamicContext(ctx, rendered, skill)
    if err != nil {
        return fmt.Errorf("inject context %s: %w", name, err)
    }

    // 4. 构建运行环境
    cfg := e.buildConfig(skill, rendered)

    // 5. 执行
    if skill.Meta.Context == "fork" {
        return e.executeInSubagent(ctx, skill, cfg)
    }
    return e.executeInline(ctx, cfg)
}

// render 执行字符串替换
func (e *Executor) render(body string, args string, skill *Skill) string {
    // 替换 $ARGUMENTS, $N, $ARGUMENTS[N]
    // 替换 ${SESSION_ID}, ${SKILL_DIR}
}

// injectDynamicContext 执行 !`command` 并替换
func (e *Executor) injectDynamicContext(ctx context.Context, content string, skill *Skill) (string, error) {
    // 正则匹配 !`...`
    // 安全处理 $ARGUMENTS → 环境变量
    // exec.CommandContext 执行，超时 30s
    //
    // 错误处理策略：
    //   - 命令执行失败：注入错误信息到占位符位置，不中断渲染
    //     格式："[命令执行失败: <error>]"
    //     让 LLM 看到失败信息并自行决策如何处理
    //   - 超时：kill 进程，注入 "[命令超时(30s)]"
    //   - 不存在任何 !`command`：直接返回原内容
}

// LoadBody 降级策略
func (r *Registry) LoadBody(ctx context.Context, name string) (string, error) {
    // 错误处理：
    //   - 文件不存在：返回 error，调用方返回给用户明确提示
    //   - 文件损坏（YAML 解析失败）：返回 error，附带解析错误详情
    //   - 权限不足：返回 error，提示检查文件权限
}

// context: fork subagent 降级策略
func (e *Executor) executeInSubagent(ctx context.Context, skill *Skill, cfg agentConfig) error {
    // 错误处理：
    //   - subagent 超时：返回超时错误给主对话，包含已完成的部分结果
    //   - subagent 失败：将错误信息回传给主对话的 LLM，由 LLM 决定是否重试
    //   - 不回退到 inline 执行（fork 和 inline 的运行环境不同）
}

// buildConfig 构建运行环境
func (e *Executor) buildConfig(skill *Skill, rendered string) agentConfig {
    cfg := e.defaultConfig

    // 免审批工具（allowed-tools 不限制可用工具，而是跳过权限确认）
    // 与 Restrict() 不同，这是在现有权限框架上叠加免审批规则
    if len(skill.Meta.AllowedTools) > 0 {
        cfg.AutoApproveTools = skill.Meta.AllowedTools
    }

    // 模型/参数覆盖
    if skill.Meta.Model != "" {
        cfg.Model = skill.Meta.Model
    }
    if skill.Meta.MaxTurns != nil {
        cfg.MaxTurns = *skill.Meta.MaxTurns
    }
    if skill.Meta.Temperature != nil {
        cfg.Temperature = *skill.Meta.Temperature
    }

    // 正文作为 system prompt 注入
    cfg.SystemPrompt += "\n\n" + rendered

    return cfg
}
```

### 6.4 集成

```go
// Skill 作为 Tool 注册到 Agent 工具集中
// LLM 通过调用 Skill 工具来触发 Skill
// 系统提示中注入 Descriptions()，让 LLM 知道有哪些 Skill 可用
// 以及何时应该调用 Skill 工具

var SkillTool = models.Tool{
    Name:        "skill",
    Description: "调用专业技能。当用户请求匹配某个技能时使用。",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "name": map[string]any{
                "type":        "string",
                "description": "技能名称",
                "enum":        registry.AvailableNames(), // 动态生成有效名称列表
            },
            "arguments": map[string]any{
                "type":        "string",
                "description": "传递给技能的参数",
            },
        },
        "required": []string{"name"},
    },
    Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
        name := call.Arguments["name"].(string)
        args, _ := call.Arguments["arguments"].(string)

        // 校验 name 是否存在
        if _, ok := registry.Get(name); !ok {
            return models.ToolResult{
                CallID:   call.ID,
                ToolName: call.Name,
                Status:   models.CallStatusFailed,
                Error:    fmt.Sprintf("unknown skill: %s, available: %v", name, registry.AvailableNames()),
            }, nil
        }

        return skillExecutor.Execute(ctx, name, args)
    },
}

// AvailableNames 返回所有可用的 Skill 名称（用于 enum 约束）
func (r *Registry) AvailableNames() []string {
    r.mu.RLock()
    defer r.mu.RUnlock()
    names := make([]string, 0, len(r.skills))
    for name := range r.skills {
        names = append(names, name)
    }
    return names
}
```

---

## 七、Skill 存储位置

| 位置 | 路径 | 适用范围 |
|------|------|---------|
| 全局 | `~/.deepai/skills/<name>/SKILL.md` | 所有项目 |
| 项目 | `.deepai/skills/<name>/SKILL.md` | 仅当前项目 |
| 插件 | `<plugin>/skills/<name>/SKILL.md` | 启用插件的位置 |

优先级：插件 > 项目 > 全局。同名时高优先级覆盖低优先级。

支持 monorepo 嵌套发现：当 Agent 操作的文件路径匹配某个子目录时，额外扫描该子目录下的 `.deepai/skills/`。

**触发条件**：Agent 调用文件类工具（`read_file`、`write_file`、`glob`、`grep`）时，检查目标文件路径是否在嵌套目录下。若是，从该目录向上查找最近的 `.deepai/skills/`。

**示例**：
```
Agent 调用 read_file("packages/frontend/src/App.tsx")
  → 文件路径在 packages/frontend/ 下
  → 额外扫描 packages/frontend/.deepai/skills/
  → 发现 react-conventions Skill，临时注入到当前上下文
```

**缓存策略**：嵌套目录的 Skill 发现结果缓存在会话中，同一子目录不重复扫描。

### 热重载

项目级 Skill 在会话期间编辑后自动生效，无需重启。全局 Skill 需重启会话。

**实现机制**：
- 采用**惰性检测**策略：每次 Skill 触发时比较 `SKILL.md` 的 `mtime`（修改时间），而非文件监听
- 粒度：单个 Skill 级别（只重新解析被触发的那个 Skill，不全量扫描）
- 并发安全：`Registry` 使用 `sync.RWMutex`，热重载时写锁替换整个 `Skill` 对象（copy-on-write），不影响正在执行的请求（它们持有旧对象的引用）
- Frontmatter 变更（如新增 `disable-model-invocation`）在下次触发时生效
- Description 列表按固定间隔（60s）或会话切换时刷新

---

## 八、目录结构总览

```
deepai/
├── pkg/
│   ├── skill/                    # [新增] Skill 系统
│   │   ├── types.go              # 核心类型
│   │   ├── registry.go           # 注册表（加载/查询）
│   │   ├── executor.go           # 执行器（渲染/注入/运行）
│   │   ├── render.go             # 字符串替换 + !`command` 注入
│   │   └── registry_test.go
│   ├── agent/                    # [不变]
│   ├── tools/                    # [不变]
│   └── ...
├── skills/                       # [新增] 项目级 Skill
│   ├── code-review/
│   │   ├── SKILL.md
│   │   ├── scripts/
│   │   └── templates/
│   ├── deploy/
│   │   └── SKILL.md
│   └── api-conventions/
│       └── SKILL.md
└── docs/
    └── SKILL_DESIGN.md
```

---

## 九、实现路线图

| 阶段 | 内容 | 时间 |
|------|------|------|
| **Phase 1** | SKILL.md 解析（frontmatter + body）、Registry、description 注入 | 1 周 |
| **Phase 2** | 字符串替换（$ARGUMENTS）、动态上下文注入（!`command`）**含安全防护** | 1 周 |
| **Phase 3** | Executor、allowed-tools 集成、context: fork subagent | 1 周 |
| **Phase 4** | Skill 作为 Tool 注册、LLM 自主调用、enum 约束 | 0.5 周 |
| **Phase 5** | 热重载（mtime 惰性检测）、monorepo 嵌套发现 | 0.5 周 |
| **Phase 6** | 错误处理与降级策略、Hook 集成 | 0.5 周 |
| **Phase 7** | 内置 Skill 示例、集成测试、文档 | 1 周 |
| **Phase 8** | 权限模型（多租户 Skill 隔离、用户级 Skill 访问控制） | 0.5 周 |

### 测试策略

| 层级 | 方法 | 覆盖范围 |
|------|------|---------|
| 单元测试 | Go 标准 testing | frontmatter 解析、字符串替换、安全过滤、Registry |
| 集成测试 | 端到端执行已知 SKILL.md | 完整加载→渲染→注入→执行流程 |
| Skill 测试 | `skill test <name>` 命令 | 验证 SKILL.md 格式、frontmatter 合法性、支持文件可达性 |
| 安全测试 | 注入用例库 | `$ARGUMENTS` 中嵌入 `; rm -rf`、`` `$(malicious)` ``、路径遍历等 |

---

*文档版本: 2.1*
*最后更新: 2026-04-04*
*适用范围: DeepAI Skill 系统*
*参考: [Claude Code Skills](https://code.claude.com/docs/zh-CN/skills), [Agent Skills 开放标准](https://agentskills.org)*
