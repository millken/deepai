# deepai 角色能力设计（M5）

> 状态：草案，待拍板。调研日期 2026-09-06，基线 main@68e04a3。
> 目标：让 15 个内置 + 4 个项目角色携带可执行知识、产出契约与验证手段，
> 且每一步改动都有 before/after 数字。

## −1. 现状证据（本轮调研核实，行号为基线时点）

**A. 角色提示词厚度悬殊。** `pkg/agent/types_config.go` 内置角色系统提示字符数：

| 角色 | 字符数 | 角色 | 字符数 |
|---|---|---|---|
| bash | 79 | perf-reviewer | 428 |
| product-manager | 194 | ui-designer | 435 |
| general-purpose | 210 | arch-reviewer | 441 |
| researcher | 218 | security-reviewer | 490 |
| analyst | 220 | coder | 1370 |
| architect | 292 | correctness-reviewer | 1929 |
| news | 381 | document-editor | 3811 |
| frontend | 409 | | |

只有 `correctnessReviewerSystemPrompt` 与 `docEditorSystemPrompt` 写了方法论
（硬禁令、失效模式、跨调用不变量、预算纪律）。其余是名词罗列。
即：项目内部已自证厚提示词有效，但只做了 2/15。

**B. Skill 系统与角色是断开的，且绑定机制曾被删除。**
`SubagentExecutor.Execute`（`pkg/agent/subagent.go`）从不注入技能目录；
只有 general-purpose 与 coder 的 `DefaultTools` 含 `skill`。
`pkg/skill/types.go` 的 `Frontmatter` 没有 `agent` / `context` 字段——
它们在 `64c05ea`（2026-08-17，"remove never-wired view_image and skill hook/fork paths"）
连同 `ExecuteFork`、`SubagentRunner`、`AgentConfig.RunInSubagent/AgentType` 一起被删除，
理由是零生产调用方。而 `.deepai/skills/docx-{polish,format,summarize}/SKILL.md`
（`807587a`，2026-08-11）至今仍写着 `agent: document-editor`，正文被迫声明：
> "The `agent: document-editor` frontmatter above is a declaration of intent only —
> on the current wiring it does not restrict which tools are available to you."

`pkg/skill/parser.go:100` 用裸 `yaml.Unmarshal`，未知字段静默忽略，作者得不到任何反馈。
这是"角色技能=空谈"最直接的实证：机制被删，文件仍声明，靠散文让模型自己去调 `task`。

**C. Skill frontmatter 多个字段在 agent 边界被丢弃。**
`Executor.buildConfig` 产出的 `AllowedTools/Model/MaxTurns/Temperature/Effort`
全仓无消费者（`react.go:1015` 只读 `Data["system_prompt"]`）。
`paths` 已实现 `MatchPaths`/`DescriptionsFiltered`，但 `repl.go:690` 调的是无参
`Descriptions()`，路径过滤未接线。

**D. 产出契约只覆盖 4 个 reviewer，共用一个泛型 `ReviewResult`。**
其余 11 个角色的产出无法程序化校验。

**E. 没有任何角色质量度量。** `docs/REVIEW_EVAL_DESIGN.md` 只有设计，
仓库无 `eval/` 目录。`/doctor`（`repl.go:1573`）体检模型/技能/MCP，不体检 agent 画像。

**F. 唯一的正面样板是对抗式自审**：厚提示词 + Strict schema +
`ParseOutput[ReviewResult]` 程序化解析 + worktree 快照硬约束 + 有界修复回注
（`pkg/chat/review.go`）。本设计即把这个"角色 + 契约 + 闸门"模式推广到其他角色。

---


## 0. 结论

推荐 **(e) 组合，主干为 "三层载体"**：L0 选人用 Description（≤100 字）；L1 常驻"角色宪法"（Go 常量或 YAML `system_prompt`，400–900 字，只写硬约束与失效模式）；L2 按需 playbook 以 **skill** 承载，通过新增的 skill↔agent 绑定在**子代理构造期**注入。产出契约（d）只给 5 个"产出被程序或下游步骤消费"的角色，其中仅 reviewer 用 Strict。度量走 `deepai eval agents`，复用 REVIEW_EVAL 的物化/指纹/summary 骨架，断言改为与 schema 字段绑定的确定性检查。

不选 (a) 单独加厚常量：15 段 2000 字常量不可由项目覆盖、无法分项度量、每次子代理调用都付全额 token。不选 (c) 单独靠 skill：skill 目前连子代理都进不去，且没有"硬约束"的位置。

## 1. 载体机制：三层与边界判据

| 层 | 载体 | 注入时机 | 内容判据 |
|---|---|---|---|
| L0 | `AgentTypeConfig.Description` | 父 agent 目录渲染 | 只答"何时派我"（§4） |
| L1 宪法 | `SystemPrompt`（内置常量 / YAML `system_prompt(_file)` / MD body） | 子代理构造期 | 满足任一：违反后**不可恢复**（docEditor 的 NEVER insert）；**每次任务都成立**的产出格式与预算纪律（correctness 规则 5）；与工具语义绑定的**跨调用不变量**（protect/author 每次都传）；已知失效模式的处置流程 |
| L2 playbook | `.deepai/skills/role-<type>/SKILL.md`（+references/） | 角色 `skills:` 预加载 → 构造期；或运行期 `skill` 工具 | 检查表、方法论、模板、领域名词表——**读一次就能照做、不读也不会造成不可恢复损害**的知识 |

判据一句话：**"漏了会出事故的放 L1，漏了只是做得差的放 L2"**。L1 上限 900 字（英文约 350 词）：docEditor 的 2000 字是因为它绑定了 4 个有状态工具，是上界不是基准。

为什么 L2 用 skill 而不是 `.deepai/agents/<type>.md` body：MD body 已经是 `SystemPrompt`（`ParseAgentMarkdown`），塞 playbook 就退化成 (a)；skill 有独立目录（可带 references）、有 Registry/热重载/描述目录、可被多个角色共享（`golang` 同时服务 coder/tester/perf-reviewer），且用户可用 `/role-architect` 直接查看。

## 2. skill ↔ 角色绑定

### 2.1 数据结构

```go
// pkg/skill/types.go Frontmatter 新增（SKILL_DESIGN §2.2 已定义，未实现）
Context string `yaml:"context"` // "" | "fork"
Agent   string `yaml:"agent"`   // context=fork 时的 agent_type

// pkg/agent/types_config.go
type AgentTypeConfig struct { ...; Skills []string `json:"skills,omitempty" yaml:"skills,omitempty"` }
// yaml_loader.go yamlAgentConfig 加 Skills []string `yaml:"skills"`；mergeConfig：override 非空则整体替换
// agentmd.go agentMDFrontmatter 加 Skills（Claude Code 的 `skills:` 键，同名对齐）

// pkg/subagent/types.go SubagentConfig 加 Skill string   // task 工具新参数 skill
// pkg/agent/subagent.go SubagentExecutor 加 skills *skill.Registry，WithSkillRegistry()
// pkg/tools/registry.go 加 WithAgentType(ctx, string) / AgentTypeFromContext(ctx)（放 tools 包：skill 与 agent 包都已依赖它，无环）
```

### 2.2 `Execute` 的注入顺序（全部在 `buildAgentConfig` 之前，一次性拼进 `systemPrompt`）

1. `profileCfg.SystemPrompt`（L1）
2. `profileCfg.Skills` 逐个 `skills.LoadBody` → 追加（L2 预加载；未知名字**硬失败**，与未知 agent_type 同策略）
3. `task.Config.Skill` 非空：校验 `Meta.Agent=="" || Meta.Agent==agentType`，否则 `error("skill %q is bound to agent %q")`；通过则 `Render(body, args)` 追加。fork skill 的 `Model/MaxTurns/Temperature` 在此消费：`Model` 覆盖 `modelAlias`、`MaxTurns` 填 `resolveMaxToolCalls` 的 caller 位、`Temperature` 覆盖 `subTemperature`——这是三字段自 Phase 3 以来第一个真实消费者
4. 若 `"skill"` ∈ 选中工具集：追加 `skills.Descriptions()`（不含 fork 类且 `Agent≠agentType` 的 skill，见 2.3）
5. OutputSchema 提示（现有）

**与前缀稳定性共存**：以上全是构造期常量——同一角色、同一 skills 列表，每次派发的系统提示字节相同，子代理自身的前缀缓存可命中；Run 内不再变化，不触碰 trailing injection。`react.go:1015` 的运行期 skill 注入只在子代理主动调 `skill` 工具时发生，与主 agent 行为一致，不新增路径。

### 2.3 `context: fork` 的路由（skill 工具 handler，`pkg/skill/tool.go`）

```
caller := tools.AgentTypeFromContext(ctx)   // Execute 里与 WithUserInteraction 同处设置
switch {
case Meta.Context != "fork":            → 现状：返回 body
case caller == Meta.Agent:               → 已在目标角色内，inline 返回 body（document-editor 自己调 docx-polish）
case caller == "":                       → 主 agent：不返回 body，Content = "Run via task(agent_type=<Agent>, skill=<name>, prompt=<args>)"，Data 不含 system_prompt
default:                                 → 其他子代理：CallStatusFailed "skill bound to <Agent>; subagents cannot delegate — report back"
}
```

为什么不让 handler 直接派 task：skill 包拿不到 pool；且会绕过 `filterTaskTool` 的递归防护。路由指令 + task `skill` 参数让 body **只落在目标子代理**，主 agent 上下文零污染——`docx-*` 三个 SKILL.md 的 Step 0 散文即可删除，改为机制保证。

### 2.4 `allowed-tools`

维持 SKILL_DESIGN 语义（免审批，不是限制）。子代理 `NonInteractive` 无审批环节，故在子代理内天然无操作；主 agent 侧在权限层落地前保持丢弃并在 `buildConfig` 注释注明。新增一条**加载期 lint**（`LoadAllReported` 的 `SkillWarning`）：fork skill 的 `allowed-tools` 若含目标 profile `DefaultTools` 以外的工具（`task/skill/ask_clarification` 除外）则告警——它是作者意图与实际权限脱节的唯一可静态发现点。`paths` 接线（repl 改调 `DescriptionsFiltered`）不属本轮，记为 M5-5 候选。

## 3. 产出契约

| 角色 | 结构 | Strict | 理由 |
|---|---|---|---|
| 4 reviewer | `ReviewResult`（现有） | 是 | review gate 程序化解析 |
| architect | `DesignDoc` | 否 | 消费者是父 LLM；Strict 重试浪费预算，但 schema 提示强制"决策必须带理由与备选" |
| product-manager | `RequirementsSpec` | 否 | 同上，验收标准必须 Given/When/Then |
| researcher | `ResearchFindings` | 否 | 强制每条结论带证据定位，eval 靠它算"有据率" |
| analyst | `AnalysisReport` | 否 | 同上 |
| tester（项目 YAML） | `TestReport` | 否 | 结果需要机器读 pass/fail 计数 |
| coder/frontend/bash/ui-designer/news/doc-editor/general | 无 | — | 产出是 diff/命令输出/文件，schema 只会逼模型把代码塞进 JSON |

非 Strict 在 `Execute` 里只贡献提示后缀、不校验不重试（L3 门控已存在），fail-soft 不需重造。eval 用 `ParseOutput` 事后解析，解析失败单列 invalid-run。

```go
type DesignDoc struct {
	Agent, Goal string
	Decisions  []Decision   // Topic, Choice, Rationale, Alternatives []string, Reversible bool
	Components []Component  // Name, Responsibility, Files []string, Interfaces []string
	Risks      []Risk       `json:",omitempty"` // Description, Mitigation
	OpenQuestions, Milestones []string `json:",omitempty"`
}
type RequirementsSpec struct {
	Agent, Problem string
	Stories  []UserStory  // Role, Want, SoThat
	ScopeIn, ScopeOut []string
	Acceptance []Criterion // ID, Given, When, Then, Verifiable bool
	Priorities []Priority  // Item, Level "P0".."P3", Reason
	OpenQuestions []string `json:",omitempty"`
}
type ResearchFindings struct {
	Agent, Question, Answer string
	Findings []Finding // Claim, Evidence []Evidence{File, Line, Quote, URL}, Confidence "high|medium|low"
	Gaps []string `json:",omitempty"`
}
type AnalysisReport struct { Agent, Objective, Method string; Findings []Finding; Caveats, Artifacts []string `json:",omitempty"` }
type TestReport struct { Agent, Command string; Passed, Failed, Skipped int; Failures []TestFailure /*Name, File, Line, Message*/; Coverage string `json:",omitempty"` }
```

YAML 角色接 schema：`OutputSchema` 是 `yaml:"-"`，新增 `output_schema: string` 键映射到 `namedSchemas map[string]*OutputSchema{"review","design","requirements","research","analysis","test-report"}`，未知名字加载报错。不允许 YAML 内写 JSON Schema：`FromStruct` 的 Go 类型是 `ParseOutput[T]` 与 eval 断言的共同锚点。

## 4. Description 规范

模板（英文，≤100 rune，`renderDelegationPrompt`/`formatAgentOptions` 都在 100 处截断）：
`Use when <触发场景>; delivers <产物>. Not for <最易混淆的邻角色>.`

| 角色 | 现状 | 改后 |
|---|---|---|
| researcher | Profile for research, reading, and synthesis tasks. | Use when a question needs evidence from code/docs before deciding; delivers cited findings. Not for edits. |
| architect | Produces technical design documents… | Use when a change spans modules or needs interface decisions; delivers DesignDoc. Not for coding. |
| analyst | Profile for structured analysis… | Use when data/logs/metrics need structured interpretation; delivers findings+caveats. Not for research. |

加单测 `TestBuiltinDescriptionsFollowSpec`：每条 ≤100 rune、以 `Use when` 开头、含 `Not for`。

## 5. 验收/度量：`deepai eval agents`

复用 REVIEW_EVAL 的 P1 骨架（临时仓库物化、chdir、快照、指纹、`runs.jsonl`/`summary.{md,json}`），差异只在语料与判定：

```
eval/agent-cases/<agent_type>/<case>/
  manifest.yaml   # agent_type, prompt, context_files, expect[]
  fixture/        # 物化成临时仓库的文件树
```

`expect` 断言全部确定性、不用 LLM 裁判（v1）：
`schema_parses` · `field_min_count: {path: decisions, n: 2}` · `field_nonempty_all: findings[].evidence` · `mentions: [pkg/agent/subagent.go, filterTaskTool]`（必须触及的事实锚点） · `not_mentions: [...]`（诱饵，防瞎编） · `tool_calls_max: 25` · `no_writes`（快照） · `tokens_max`。

指标：每 case 断言通过率（主）、schema 解析率、`mentions` 命中率（=事实覆盖）、`not_mentions` 违规率（=编造）、成本。每 case 3 次，报稳定度。**指纹** = sha256(解析后的 SystemPrompt + 预加载 skill body + schema Prompt)，写进每条结果：换指纹重跑即 before/after，`deepai eval compare a.json b.json` 输出逐指标差值。

首期语料：architect/product-manager/researcher/analyst 各 3 case + tester 3 case = 15，全部以 deepai 自身代码为 fixture（`mentions` 锚点可从仓库真值直接取，不需要人工标注）；review 32 case 沿用 REVIEW_EVAL。

## 6. 分期

| 期 | 内容 | 验收 | 风险/回滚 |
|---|---|---|---|
| **M5-1 绑定机制** | §2 全部：Frontmatter `context/agent`、`Skills` 字段（YAML/MD）、task `skill` 参数、`WithAgentType`、fork 路由、Execute 四步注入、allowed-tools lint；`docx-*` 删 Step 0 散文改依赖机制 | 单测：主 agent 得路由 stub 且 Data 无 body；匹配子代理 inline；其他子代理失败；未知 skill 硬失败；`Skills` 预加载后系统提示跨 Run 字节相同（前缀稳定）；`selectSubagentTools` 仍剔除 task。手工：`/docx-polish x.docx` 一次 task 到位 | 纯增量，无字段默认值变化；回滚 = revert 单提交 |
| **M5-2 eval 基线** | `deepai eval agents` + `compare`；15 case；用**当前薄 prompt**跑出 before | fake task tool 全链路单测；真实模型 runs=3 的 summary 入库 | 只加 `pkg/commands/agent_eval.go` 与 `eval/`，零生产改动 |
| **M5-3 五角色宪法+契约+描述** | architect/PM/researcher/analyst 的 L1（≤900 字，仿 correctness 七条：范围、证据、预算、产出、失效模式）、§3 四个 schema、§4 描述；三个薄 reviewer 补规则 2/3/5 | `compare`：断言通过率 +15pt、`not_mentions` 违规不升、成本 ≤1.5×；reviewer 沿用 REVIEW_EVAL 达标线 | 逐角色一个常量，可单独 revert；指纹保证不会拿旧基线背书 |
| **M5-4 L2 playbook 与项目角色** | `role-architect/role-researcher/role-tester` 三个 skill（检查表+模板）；tester/devops/database/technical-writer YAML 改写为"宪法 + `skills:` + `output_schema:`"；`namedSchemas` | 同 M5-3 的 compare；tester `TestReport` 解析率 ≥90% | skill 目录可删；YAML 可回退 |
| M5-5（可选） | `paths` 接线、frontend/ui-designer/news 宪法、`--judge` 主观评分 | 视 M5-3/4 数字决定 | — |

## 7. 反面清单

- **不**把 15 个 prompt 都写到 2000 字：每次派发全额付费、无法分项归因、项目 YAML 一覆盖就整段消失。docEditor 的长度来自 4 个有状态工具，不是范本。
- **不**给每个角色默认 `skills:` 一串：预加载即常驻，L2 变 L1；只预加载"该角色每次都用"的那一个（≤1 个，`role-<type>`）。
- **不**把所有角色都上 Strict schema：coder/frontend 的产出是文件；Strict 重试烧预算却换不来可用性。
- **不**让 skill handler 自己 spawn 子代理，**不**在 task 工具放开 `tools/system_prompt` 参数：前者破坏递归防护，后者让 skill 变成提权通道。
- **不**用 LLM 当 v1 裁判：非确定性会淹没 15pt 的差异；先用字段/锚点断言。
- **不**在运行期按轮次拼角色知识：破坏 M4-2 前缀稳定；一切注入止于 `buildAgentConfig` 之前。
- **不**用中文长句写 Description：100 rune 截断在 `…` 处，触发条件必须在前 60 字内。

## 8. 已拍板（2026-09-06，用户裁定）

1. **fork skill 走 task 工具的 `skill` 参数**（而非仅靠角色 `skills:` 预加载）。
   保留 `$ARGUMENTS`，docx 三个 skill 不常驻。
2. **非 Strict 角色先观测一期**：M5-2/M5-3 报出解析率后再决定是否升级 Strict，
   本轮不改 `WithStrict`。
3. **M5-3 达标线接受**：断言通过率 +15pt、`not_mentions` 违规不升、成本 ≤1.5×；
   任一角色不达标则该角色的 L1 回退，不阻塞其他角色。

   **修订（2026-09-06，M5-2 实测后）**：基线模型 `glm-5.3` 经中转访问，
   provider **不回传 usage**，`task` 结果的 `Data["subagent_usage"]` 恒为空，
   `runs.jsonl` 的 `tokens` 全为 0。用户裁定：保持 glm-5.3 作为基线模型
   （基线必须反映生产实际使用的模型，这比成本指标本身更重要），
   **成本判据从"平均 token ≤1.5×"改为"平均耗时 ≤1.5×"**。
   注意代理指标的局限：耗时受网络与中转排队影响，噪声显著大于 token，
   因此耗时超标时**不直接判定不达标**，而是先重跑该角色一轮复核；
   若 provider 后续开始回传 usage，立即改回 token 判据。
4. **eval 语料首期 Go-only，以 deepai 自身代码做 fixture**。
   自指风险的缓解：`mentions` 锚点取自仓库真值（函数名/文件名），
   诱饵 `not_mentions` 取自"看似相关但实际不存在"的符号，模型的先验帮不上忙。

## 9. 实施顺序注记

M5-1 与 M5-2 之间没有依赖，但**必须先 M5-2 跑出 before 基线再做 M5-3**：
否则第三期改完提示词无法归因，等于又一轮空谈。
M5-1 先行的理由是它是纯机制、零提示词改动，不会污染基线指纹。
