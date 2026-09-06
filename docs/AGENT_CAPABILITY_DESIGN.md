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

判据一句话：**"漏了会出事故的放 L1，漏了只是做得差的放 L2"**。

L1 篇幅按字符计，分三档：无契约无有状态工具的角色 ≤ 900；携带 OutputSchema 的角色 ≤ 2000（correctness-reviewer 1929 是实测有效的上界，不是基准，超过它必须附 eval 数字）；绑定有状态工具或自动闸门的角色不设数字上限，但每段必须点名它保护的工具参数或闸门。docEditor 的 3811 属第三档。

**修订（M5-4，2026-09-07）**：上面这条以"是否携带 OutputSchema"划分 A/B 档的判据**已失效**——M5-4 删掉四个角色的契约后它们都不再携带 OutputSchema，按原判据应落入 A 档（≤900），但实测是 architect 1573 / product-manager 1540 / researcher 1475 / analyst 1460，全部超出 560–670 字符。

判据改为按**载荷性条款数**而非"有没有契约"：一条载荷性条款是"规则 + 违反后果"，英文里最紧凑约 55–70 词。角色需要几条就给几条的预算，上限仍是 2000 字符（correctness-reviewer 实测 1929 是唯一经语料验证有效的厚提示词尺寸，超过它必须附 eval 数字）。四个角色现在各四条（范围、证据、可执行性/验收/方法、预算），落在 1460–1573 属合规。绑定有状态工具或自动闸门的角色仍不设数字上限，但每段必须点名它保护的机制。


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

## 3. 产出契约 —— 已于 M5-4 撤销，本节仅作决策记录

> **本节描述的五个契约已全部删除，代码里不存在**；现存的 `namedSchemas` 只有 `review`。
> 撤销的理由、实测代价与行业依据见 §8 修订四。保留原文是为了让下一个想引入产出契约的人
> 先看到我们付过的代价，而不是重新交一遍学费。**不要按本节内容实施任何东西。**


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

**M5-3 实现裁定（已落地，字段名见 pkg/agent/output.go）**：

1. **全部字段加小写 json tag**（裁定 (a)，见下方修订项 2）——`Decisions`→`decisions`、`Reversible`→`reversible` 等，与 `ReviewResult`/`Issue` 的既有写法一致。
2. **`Evidence.Line` 是 `int`，另加 `EndLine int` `json:"end_line,omitempty"`**（不是 `Line string` 或 `"21-24"` 区间字符串）——见下方修订项 3。
3. **`Finding.Confidence`（high/medium/low）与 `Priority.Level`（P0..P3）没有 schema 强制的 `enum`**：jsonschema-go v0.4.3 的 `jsonschema` 结构体 tag 只能设置 `Description`，没有任何 tag 语法能设置 JSON Schema 的 `enum` 关键字；写 `jsonschema:"enum=high,enum=medium,enum=low"` 会让 `FromStruct` 直接 panic（tag 以 `WORD=` 开头是库保留前缀，见 `jsonschema-go/jsonschema/infer.go` 的 `disallowedPrefixRegexp`）。两个字段改用 `jsonschema:"one of exactly: ..."` 纯文字 description 作为退路——它会出现在 schema Prompt 的 `"description"` 字段里，但不是校验约束，模型不遵守也不会解析失败。`pkg/agent/output_test.go` 的 `TestEnumTagIsRejectedByJSONSchemaGo` 把这条库限制钉成回归测试：库若未来支持了，这条测试会变红，提示重新评估。

```go
type DesignDoc struct {
	Agent         string      `json:"agent"`
	Goal          string      `json:"goal"`
	Decisions     []Decision  `json:"decisions"`
	Components    []Component `json:"components"`
	Risks         []Risk      `json:"risks,omitempty"`
	OpenQuestions []string    `json:"open_questions,omitempty"`
	Milestones    []string    `json:"milestones,omitempty"`
}
type RequirementsSpec struct {
	Agent         string      `json:"agent"`
	Problem       string      `json:"problem"`
	Stories       []UserStory `json:"stories"`
	ScopeIn       []string    `json:"scope_in"`
	ScopeOut      []string    `json:"scope_out"`
	Acceptance    []Criterion `json:"acceptance"`
	Priorities    []Priority  `json:"priorities"`
	OpenQuestions []string    `json:"open_questions,omitempty"`
}
type ResearchFindings struct {
	Agent    string    `json:"agent"`
	Question string    `json:"question"`
	Answer   string    `json:"answer"`
	Findings []Finding `json:"findings"` // Claim, Evidence []Evidence{File, Line int, EndLine int omitempty, Quote, URL}, Confidence "high|medium|low" (description-only, not enum-enforced)
	Gaps     []string  `json:"gaps,omitempty"`
}
type AnalysisReport struct {
	Agent     string    `json:"agent"`
	Objective string    `json:"objective"`
	Method    string    `json:"method"`
	Findings  []Finding `json:"findings"`
	Caveats   []string  `json:"caveats,omitempty"`
	Artifacts []string  `json:"artifacts,omitempty"`
}
type TestReport struct {
	Agent    string        `json:"agent"`
	Command  string        `json:"command"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Failures []TestFailure `json:"failures"` // Name, File, Line int, Message
	Coverage string        `json:"coverage,omitempty"`
}
```

YAML 角色接 schema：`OutputSchema` 是 `yaml:"-"`，新增 `output_schema: string` 键映射到 `namedSchemas map[string]*OutputSchema{"review","design","requirements","research","analysis","test-report"}`，未知名字加载报错。不允许 YAML 内写 JSON Schema：`FromStruct` 的 Go 类型是 `ParseOutput[T]` 与 eval 断言的共同锚点。

**非 Strict 观测期结束判据**：契约达成率 ≥80% 连续两轮即维持非 Strict；<80% 且失败主因是 `extractJSON` 取到了错误对象（文本里配平括号的最后一个 `{…}` 不是模型的输出对象）或字段名大小写不匹配，先修本节第 1、2 条（tag/字段形态）再考虑升级 Strict——这两类失败靠 Strict 重试也救不了几次，只会烧 1.5× 预算。

## 4. Description 规范

模板（英文，≤100 rune，`renderDelegationPrompt`/`formatAgentOptions` 都在 100 处截断）：
`Use when <触发场景>; delivers <产物>. Not for <最易混淆的邻角色>.`

| 角色 | 现状 | 改后 |
|---|---|---|
| researcher | Profile for research, reading, and synthesis tasks. | Use when a question needs evidence from code/docs first; delivers cited findings. Not for analysis. |
| architect | Produces technical design documents… | Use when a change spans modules or needs interface decisions; delivers a design. Not for coding. |
| analyst | Profile for structured analysis… | Use when data/logs/metrics need interpreting; delivers method, findings, caveats. Not for research. |

researcher 的 Description 定稿是 `Not for analysis.`，不是本节曾经的草案 `Not for edits.`：researcher 的 `DefaultTools` 没有写工具，"edits" 不是它会被误派去做的事；analyst 才是它最容易被混淆的邻角色（两者共用 `Finding` 结构、工具集几乎相同）。四角色 + 三个薄 reviewer（security/arch/perf-reviewer）的定稿见各自的 L1/Description 落地，全部 ≤100 rune，`Not for` 均落在第 79–82 字符处。

加单测 `TestBuiltinDescriptionsFollowSpec`：每条 ≤100 rune、以 `Use when` 开头、含 `Not for`。**范围仅本期改写的七个角色**（architect/product-manager/researcher/analyst + security/arch/perf-reviewer）——correctness-reviewer、coder、frontend 等未改写的角色仍是旧 Description，不满足这个模板，若把它们也纳入断言范围测试会立刻报红；测试用白名单限定范围，下一期扩大改写面时再一并扩大白名单。

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
| **M5-3 五角色宪法+契约+描述** | architect/PM/researcher/analyst 的 L1（≤2000 字符（B 档），五条必备：范围、证据（标识符逐字 + 字面值 + 位置）、契约、预算、角色特有失效模式）、§3 四个 schema、§4 描述；三个薄 reviewer 补规则 2/3/5 | `compare`：断言通过率 +15pt、`not_mentions` 违规不升、成本 ≤1.5×；reviewer 沿用 REVIEW_EVAL 达标线 | 逐角色一个常量，可单独 revert；指纹保证不会拿旧基线背书 |
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

   **修订二（2026-09-06，基线实测后）**：M5-2 基线（glm-5.3，15 case × 3 run，
   `--timeout 5m`；运行产物写在 `eval/results/`，该目录不入库，数字以本节为准）
   整体断言 pass=290 fail=50 skip=35，通过率 85.3%。
   "+15pt"在数学上不可达，原因是分母结构，不是模型：

   - 340 条参与计算的断言里，`no_writes`/`tool_calls_max`/`tokens_max` 129 条 +
     `not_mentions` 86 条 = 215 条（63%）全部满分且结构性不可失败（`tokens_max`
     在 usage 缺失时恒 pass），把分母钉死；
   - 50 条失败里 43 条是 `schema_parses`，M5-3 加产出契约后本来就会翻转，这不是
     能力信号，是交付物本身；
   - 35 条 `field_*` 全部 skipped 且**不进分母**，after 一旦 schema 通过它们就进入
     分母——before/after 的"整体通过率"分母不同，本来就不是同口径；
   - 全部翻转的天花板 = (290+43+35)/(340+35) = 98.1%，最多 +12.8pt。

   **裁定：废弃"整体断言通过率 +Npt"作为达标线，改为逐指标门槛，按角色独立判定。**

   > **注（M5-4）**：下面这张六条表里的第 1 条"契约达成率"已随契约一并删除，
   > 逐角色门槛表的"契约达成率"列同样作废。现行达标线是五条，见修订四。
   > 本表保留为当时的判断记录。
   每个门槛对应 M5-3 的一个设计意图，任一角色不达标只回退该角色的 L1（§8 第 3 条原则不变）。
   所有指标都能从现有 `runs.jsonl` 纯本地重算，不重跑模型。

   | # | 指标 | 计算口径 | 门槛 | 数字由来 |
   |---|---|---|---|---|
   | 1 | **契约达成率** | run 级：`schema_parses` pass **且**该 run 无任一 `field_*` fail ÷ 已派发 run 数 | **≥ 80%** | §3 契约是 M5-3 的主交付。每角色 9（或剔除超时后 8）个样本，80% 即"至多 1 次未达成"（8/9=88.9%、7/9=77.8%；7/8=87.5%）。按 §8 第 2 条非 Strict 先观测一期，留 1 次容错；M5-4 tester ≥90% 是下一档。用 run 级而非断言级，是为了让 `field_*` 不能靠 skipped 逃出分母，也让 tester（无 field 断言）与其他角色同一口径 |
   | 2 | **mentions 命中率** | 断言级：`mentions:*` pass ÷ (pass+fail) | **≥ 基线 − 8pt**，且**五角色合计 ≥ 93.75%**（基线 75/80） | 每角色样本 13–18 条，一条断言权重 5.6–7.7pt，零容忍会被单次噪声打穿，故容 1 条（8pt ≥ 1/13）。合计零容忍，防止五个角色各花掉一次容错叠成真实倒退。合计不达标而各角色均达标时，回退 mentions 绝对下降最大的角色，直到合计达标。这是唯一有区分度的信号，不并入任何复合分 |
   | 3 | **not_mentions 违规率** | 断言级：`not_mentions:*` fail ÷ (pass+fail) | **≤ 基线**（基线全 0 ⇒ 必须为 0） | 沿用原达标线"违规不升"。编造零容忍 |
   | 4 | **护栏违规数** | 计数：`no_writes`/`tool_calls_max:*`/`tokens_max:*` 的 fail + `write_violation` | **= 0**（== 基线） | 护栏从比率里拿出来单列计数，不再稀释能力信号；它是安全底线不是能力分。`tokens_max` 在 usage 缺失期间是空判据，报出但不计 |
   | 5 | **平均耗时** | 仅**已派发** run 的 `duration_ms` 均值 | **≤ 1.5× 基线** | 沿用修订一。超标不直接判负，先重跑该角色一轮复核；复核仍超 → 不达标 |
   | 6 | **dispatch 超时数** | 计数：`error != ""` 的 run；这些 run **从 1–5 的全部分母剔除** | **≤ 基线 + 1**（analyst/tester ≤ 2，其余 ≤ 1）；已派发 run 必须 **≥ 7/9** 否则本轮不可判定 | 超时 run 无输出，其 10 条断言一条都没评估；老口径把它折成 1 个 `dispatch` fail，既压低该角色通过率、又让分母随机变化（analyst 64 vs 72），不是同口径。剔除后单列计数，并给一个"基线+1"的上限：超时是成本倒退的另一种表现（L1 更长 → 更多工具调用 → 更易撞 5m），超出与耗时同样处理——先重跑复核，复核仍超 → 不达标 |

   **判定规则**：1–4 任一不满足 → 该角色不达标，L1 回退；仅 5/6 不满足 → 该角色"待复核"，
   重跑一轮后按同规则再判，不阻塞其他角色。整体断言通过率继续在 summary 报出，
   但只作诊断列，不再是门槛。**可比性前提**：同模型（glm-5.3）、同 `--timeout 5m`、
   同 runs=3、同 case 集；`compare` 必须校验前两项一致（见下方前置任务）。

   **否决 (a)"能力分 + 护栏拆分"**：拆分方向是对的（本修订采纳了护栏单列），但把
   schema+field+mentions 折成一个"能力分"不行——before 分母里 schema 43 条全败、field
   35 条不参与，after schema 翻转的同时 field 35 条进入分母，"能力分"从 47.5%
   （75/158，skip 计失败）或 61.0%（75/123，skip 不计）跳到 90% 以上是契约交付的
   必然结果，与角色能力无关；而 mentions 在能力分里权重约一半，漏 5 条只值 −3pt，
   会被 +30pt 的顺风淹没。它把最有区分度的信号稀释在最没区分度的翻转里。

   **否决 (b)"整体门槛降到 +10pt"**：+10 只是把天花板从 13 挪到 10，仪器没变——
   护栏仍占分母 63%，每个真实信号的灵敏度被砍掉三分之二；mentions 只占 21%，漏 5 条
   只值 −1.3pt，完全不可见；field 进入分母导致前后分母不同口径；一次超时就让分母
   随机少 10 条。它是一个"不能告诉你哪里动了"的复合数字，达标或不达标都无法归因到
   某一角色的某一条 L1 规则，与 §9"归因"的要求相悖。

   **新口径基线（before，从 `runs.jsonl` 重算；M5-3 `compare` 对照此表）**：

   | 角色 | 超时/已派发 | 契约达成率 | schema 解析率 | mentions 命中率（漏） | not_mentions 违规 | 护栏违规 | 均耗时（已派发） | 旧口径断言通过率（诊断） |
   |---|---|---|---|---|---|---|---|---|
   | analyst | 1 / 8 | 0/8 = 0.0% | 0/8 = 0.0% | 16/16 = 100.0%（0） | 0/16 = 0% | 0/24 | 152,923 ms | 56/64 = 87.5% |
   | architect | 0 / 9 | 0/9 = 0.0% | 0/9 = 0.0% | 15/18 = 83.3%（3） | 0/18 = 0% | 0/27 | 200,192 ms | 60/72 = 83.3% |
   | product-manager | 0 / 9 | 0/9 = 0.0% | 0/9 = 0.0% | 16/18 = 88.9%（2） | 0/18 = 0% | 0/27 | 115,466 ms | 61/72 = 84.7% |
   | researcher | 0 / 9 | 0/9 = 0.0% | 0/9 = 0.0% | 15/15 = 100.0%（0） | 0/18 = 0% | 0/27 | 125,406 ms | 60/69 = 87.0% |
   | tester | 1 / 8 | 0/8 = 0.0% | 0/8 = 0.0% | 13/13 = 100.0%（0） | 0/16 = 0% | 0/24 | 196,229 ms | 53/61 = 86.9% |
   | **合计** | 2 / 43 | 0/43 = 0.0% | 0/43 = 0.0% | **75/80 = 93.75%（5）** | 0/86 = 0% | 0/129 | 157,274 ms | 290/338 = 85.8% |

   注：(i) analyst/tester 的均耗时高于 `summary.md` 的 135,931 / 174,426——旧
   `buildEvalSummary` 把超时 run 的 `duration_ms=0` 也算进了均值，会让 after/before
   的耗时比虚高 12.5%，这是必须先修的 harness 缺陷；(ii) 旧口径"诊断"列剔除了
   `dispatch` 伪断言，故 analyst 87.5% ≠ summary.md 的 86.2%；(iii) 5 条 mentions 漏项全
   是上下文预算常量（architect `contextFilePerFileCap`×2、`contextFilesTotalCap`×1，
   product-manager `contextFilesTotalCap`×2），M5-3 L1 的"证据"条款应当直接命中它们，
   `compare` 时单独核对这两个角色是否回收。漏项的具体形态是**值对、行号对、标识符被
   改写**（例如 `PerFileCap`/`TotalCap`/"the per-file cap"，而不是完全没读到值）——
   详见 M5-3 文案第 0 节的逐 run 复核；`compare` 时若这两个角色回收了漏项而其他
   mentions 出现新漏，优先检查新漏是否同一形态（值对但标识符被翻译/改写/缩短）。

   (iv) **tester 作为对照组的适用范围收窄**：tester 提示词本期一字未动
   （M5-4 才改），但 M5-3 把评测侧 `schema_parses`/`field_*` 的解码类型从
   `pkg/commands` 内的镜像结构体（无 json tag）换成了 `pkg/agent` 的真类型
   （`agent.TestReport`，有小写 json tag）——**给它打分的尺子换了**，即使被打分
   的输出一个字节没变。同一份 tester 输出，旧镜像可能因大小写不匹配
   `unexpected additional properties` 判 fail，新真类型判 pass。因此
   **tester 只在 mentions 命中率、均耗时、超时数三项上充当模型/中转漂移的对照**；
   它的 `contract_rate`（`schema_parse_rate` 同理）在本轮 before/after 之间不
   可比，即使数字上移也不能归因为"tester 变好了"或用来反推"漂移"——那是解码
   尺子变了，不是被评测对象变了。`namedSchemaKinds`/`evalSchemas` 换回真类型
   见本文第 3 节修订项 2；下一次真正靠 `contract_rate` 判断 tester 是 M5-4
   给它挂 `output_schema: test-report` 之后，那时前后用的是同一把尺子。

   **M5-3 逐角色门槛（由上表代入）**：

   | 角色 | 契约达成率 | mentions 命中率 | not_mentions | 护栏违规 | 均耗时上限（1.5×） | 超时上限 |
   |---|---|---|---|---|---|---|
   | analyst | ≥ 80% | ≥ 92.0% | 0 | 0 | ≤ 229,384 ms | ≤ 2 |
   | architect | ≥ 80% | ≥ 75.3% | 0 | 0 | ≤ 300,288 ms | ≤ 1 |
   | product-manager | ≥ 80% | ≥ 80.9% | 0 | 0 | ≤ 173,198 ms | ≤ 1 |
   | researcher | ≥ 80% | ≥ 92.0% | 0 | 0 | ≤ 188,109 ms | ≤ 1 |
   | tester | ≥ 80% | ≥ 92.0% | 0 | 0 | ≤ 294,343 ms | ≤ 2 |
   | 合计 | — | ≥ 93.75% | 0 | 0 | — | — |

   **M5-3 前置任务：harness 统计口径改造（`pkg/commands/agent_eval.go`，本修订不改代码）**

   1. `roleSummary`：新增 `DispatchedRuns int`（`dispatched_runs`）、`ContractRate float64`
      （`contract_rate`）、`FieldPassRate float64`（`field_pass_rate`，诊断）、
      `MentionsHits/MentionsTotal int`、`GuardViolations int`（`guard_violations`，
      = `no_writes`/`tool_calls_max`/`tokens_max` fail 数 + `write_violation`）。
      `evalSummary` 新增 `Timeout string`（写入 `--timeout` 原值）供 compare 校验。
   2. `buildEvalSummary`：`r.Error != ""` 的记录只累加 `DispatchErrors`，**跳过**其余全部
      累加（断言计数、族计数、`durationSum`）；`AvgDurationMS`/`AvgTokens` 的分母改为
      `DispatchedRuns`。新增 run 级契约判定：该 run 存在 `schema_parses:*` pass 且无
      `field_*` fail → `contract++`。`AssertionPassRate` 保留但不再含 `dispatch` 伪断言。
   3. `renderEvalSummaryMD`：新增列 `dispatched`、`contract`、`guard viol`；`avg ms` 改名
      `avg ms (dispatched)`。
   4. `renderEvalCompare`：每角色输出六行并逐行给出 `PASS`/`FAIL`/`RECHECK`：
      契约达成率（≥80%）、mentions 命中率（≥ before−8pt）、not_mentions 违规率（≤ before）、
      护栏违规数（=0）、均耗时比（≤1.5× → 否则 `RECHECK`）、超时数（≤ before+1 → 否则
      `RECHECK`；已派发 <7 → `INVALID`）；末尾输出五角色合计 mentions 命中率（≥ before）
      与每角色 `VERDICT: pass | fail | recheck | invalid`。开头校验 `Model`、`Runs`、
      `Timeout` 三者一致，不一致直接报错退出（不可比）。保留指纹未变的 WARNING。
      `avg tokens` 行在 before/after 均为 0 时改为一行 `usage unavailable`。
   5. `agent_eval_test.go`：用一份含一条 `error` 记录的 fixture 给 `buildEvalSummary`
      加 golden 单测，期望值即上方"新口径基线"表（契约 0/8、mentions 16/16、均耗时
      仅计已派发 run）；`renderEvalCompare` 加一条 RECHECK 路径和一条 Model 不一致报错的用例。
   6. 改造完成后用新 harness 对**同一份** `runs.jsonl` 重新生成 summary（不重跑模型），
      核对与上表逐格一致，再以该 summary.json 作为 M5-3 `compare` 的 before 输入。
4. **eval 语料首期 Go-only，以 deepai 自身代码做 fixture**。
   自指风险的缓解：`mentions` 锚点取自仓库真值（函数名/文件名），
   诱饵 `not_mentions` 取自"看似相关但实际不存在"的符号，模型的先验帮不上忙。

   **修订三（2026-09-06，M5-3 实测后）**：五个角色跑完 before/after，
   product-manager 与 analyst 各做了一轮复核，architect 改过 Output 段后重跑一轮。
   全部同模型（glm-5.3）、同 `--timeout 5m`、同 runs=3、同 case 集。

   | 角色/轮次 | 契约达成率 | 超时 | 写盘 | 均耗时 | 倍数 |
   |---|---|---|---|---|---|
   | architect / before | 0/9 = 0.0% | 0 | 0 | 200.2s | 1.00× |
   | architect / after | 5/9 = 55.6% | 0 | 0 | 199.5s | 1.00× |
   | architect / 重跑（改 Output 段） | 6/9 = 66.7% | 0 | 0 | 178.1s | 0.89× |
   | product-manager / before | 0/9 = 0.0% | 0 | 0 | 115.5s | 1.00× |
   | product-manager / after | 9/9 = 100.0% | 0 | 0 | 184.4s | 1.60× |
   | product-manager / 复核 | 6/9 = 66.7% | 0 | 0 | 155.6s | 1.35× |
   | **product-manager / 合并 18 次** | **15/18 = 83.3%** | | | | |
   | researcher / after | 9/9 = 100.0% | 0 | 0 | 163.9s | 1.31× |
   | analyst / after | 5/6 = 83.3% | 3 | 0 | 196.3s | 1.28× |
   | analyst / 复核 | 4/5 = 80.0% | 4 | **1** | 220.2s | 1.44× |
   | **analyst / 合并 18 次** | **9/11 = 81.8%** | **7/18** | | | |
   | tester（对照组） / after | 0/9 = 0.0% | 0 | 0 | 208.2s | 1.06× |

   **成立的结论**：产出契约从全线 0% 变为 62%–100%，是本期唯一确凿的收益；
   五角色合计 mentions 从 93.75% 升到 97.44%，基线里 architect/product-manager
   漏掉的那 5 个上下文预算标识符**全部回收**（证据条款命中了它瞄准的形态）；
   对照组 tester 提示词一字未动，契约仍 0%、耗时 1.06×、指纹未变，
   因此上述变化可归因于本期改动而非模型漂移。

   ### 缺陷一：n=9 分辨不出 80% 与 100%，单轮契约达成率不可作判据

   product-manager 在**完全相同的提示词、配置与模型**下，两轮分别是 9/9 和 6/9，
   相差 33 个百分点。真值 p≈0.85 时 n=9 的观测标准差约 12pt，两倍标准差 ±24pt——
   这两个观测来自同一真值不需要任何额外解释。因此：
   after 轮里 researcher 的 100%、product-manager 的 100%、analyst 的 83.3%
   都不足以单独支撑"达标"，architect 的 55.6% 与 66.7% 同样可能是同一真值。

   **裁定：契约达成率的判定改为合并至少两轮（n ≥ 18 已派发 run）的估计**，
   单轮数字只作诊断。合并前提是两轮的指纹相同——不同提示词的轮次不得合并
   （architect 的两轮因此不可合并）。这与 §8 修订二废弃"整体通过率 +Npt"是
   同一类毛病的另一面：那次是分母被护栏钉死导致不灵敏，这次是分子样本太少导致不稳定。

   ### 缺陷二：出错的 run 被排除出全部分母，写盘违规因此不可见

   修订二规定 `error != ""` 的 run 从指标 1–5 的分母中剔除。analyst 复核轮里
   `output-schema-strict-retry` r2 **写了文件之后才超时**，`write_violation=true`，
   但因为它是出错 run，护栏违规统计为 0，`compare` 输出 `[PASS] guard violations: 0 -> 0`。

   写盘是**已经发生的事实**，与该 run 是否跑完无关。剔除规则对"没有输出就无法评分"的
   指标（契约、mentions）是对的，对护栏是错的。

   **裁定：`write_violation` 与护栏违规的计数独立于 dispatch 状态**，
   出错 run 的写盘照常计入。`buildEvalSummary` 需相应修改（M5-4 前置）。

   ### analyst：不是"不见效"，是被超时挡住了

   analyst 两轮合计超时 7/18（基线 1/9），两轮均因已派发 < 7 判为**不可判定**。
   原因是耗时：基线 152.9s，两轮 196.3s / 220.2s，而单次上限是 300s——
   均值推到 220s 时，分布右尾大量越过上限。它的契约达成率（合并 81.8%）与
   mentions（100%）本身并不差。

   **裁定：analyst 的 L1 需要缩短而不是加码**（当时 1759 字符；修订四删掉 Output 段后
   已降到 1460，本条部分兑现），目标是把均耗时压回 1.3× 以内。不通过放宽 `--timeout` 解决——
   放宽会让本轮与后续轮次不可比，而可比性是这套基线唯一的价值。

   ### architect：改 Output 段有效但不足，转 Strict

   改 Output 段（禁裸双引号、宁少勿多、写不完就交更小的合法对象）后：
   契约 55.6% → 66.7%，均耗时 199.5s → 178.1s（0.89×），输出均长 9427 → 7437 字符，
   mentions 保持 88.9%。**上一轮"中文文本里夹裸双引号"的失败形态消失**。

   剩余失败三种，全部不是 schema 设计问题：数组闭合后多一个引号、
   多写一个 schema 之外的键（`rationale_missing`）并在闭合后多出 `]}`、
   以及一次跑满 248s 后输出为空。

   §8 第 2 条"非 Strict 先观测一期"的观测目的就是判断重试值不值。观测结论：
   **失败集中在 JSON 语法错误、多余键、空输出——这三种恰是把解析错误回喂后
   重试一次能修的**；修订二第 5 条担心的两种（extractJSON 取错对象、字段名大小写）
   一次都没出现。

   ~~裁定：architect 单独转 Strict，其余三个角色维持非 Strict 继续观测；
   architect 当前耗时是基线的 0.89×，有承担一次重试的余量。~~
   **已被修订四作废：契约本身已删除，无 Strict 可转。** 这条裁定的证据仍然成立
   （失败集中在语法错误、多余键、空输出，都是重试能修的），只是前提没了——
   如果将来重新引入某个确有程序消费者的契约，这条推理可以直接复用。

   **修订四（2026-09-07，撤销强制 JSON 产出契约）**：删除 architect /
   product-manager / researcher / analyst 四个角色的产出契约与全部五个契约类型
   （`DesignDoc`/`RequirementsSpec`/`ResearchFindings`/`AnalysisReport`/`TestReport`），
   四段 L1 的 `Output:` 段一并删除。`namedSchemas` 只剩 `review`。

   **三条依据，缺一不足以撤：**

   1. **实测代价**：四个非 Strict 契约的达成率 62%–100%，即最好情况下每三次交付
      有一次被整份丢弃。失败**全部是手写大型嵌套 JSON 的语法失手**——未转义的双引号、
      多闭合的方括号、schema 之外的多余键、输出为空——**没有一次是 schema 设计问题**。
      architect 专门改过 Output 段（禁裸双引号、宁少勿多、写不完交更小的合法对象），
      达成率从 55.6% 只升到 66.7%，说明提示词层面已到天花板。
   2. **无程序消费者**：全仓 `ParseOutput[T]` 的真实调用点只有
      `pkg/chat/review.go` 的 `ReviewResult`（review gate 用它决定放行还是回注修复）。
      其余五个契约类型**只被评测自己引用**——它们存在的唯一理由是让评测去量它们。
      四个角色的产出消费者是父模型，而父模型读文本没有问题。
   3. **行业无一家这么做**：调研 Aider / Cline / Roo Code / OpenHands / Cursor /
      Copilot / Codex CLI / Gemini CLI / Amp / Devin，**没有任何一家强制子代理的
      最终消息必须是合法 JSON**。Aider 明确拒绝用 JSON 或工具调用包裹代码返回，
      理由是这会让模型写出更差的代码；Cognition 主张传完整 trace 而不是压缩成 schema。

   **判据（下次想加契约时用这一条判）：有程序在读的才配有 schema，
   另一个模型在读的不算。**

   **达标线随之收敛为五条**（修订二那张六条的表、以及其中"契约达成率 ≥80%"
   的逐角色门槛列，**已被本条取代，不再是现行标准**）：

   | # | 指标 | 门槛 |
   |---|---|---|
   | 1 | mentions 命中率 | ≥ 基线 − 8pt，且五角色合计 ≥ 基线 |
   | 2 | not_mentions 违规率 | ≤ 基线 |
   | 3 | 护栏违规数 | = 0 |
   | 4 | 均耗时（仅已派发 run） | ≤ 1.5×，超标先复核重跑 |
   | 5 | dispatch 超时数 | ≤ 基线 + 1；已派发 < 7/9 视为不可判定 |

   **修订三里"architect 单独转 Strict"的裁定同时作废**——那条裁定的前提是契约存在。
   同理，修订二第 (iv) 条关于 tester 对照组 `contract_rate` 不可比的限定也不再适用，
   因为该指标已不存在；tester 作为漂移对照仍只对 mentions、耗时、超时三项有效。

   **M5-3 实际留下什么**：证据条款有效并保留——五角色合计 mentions 从 93.75% 升到
   97.44%，基线里 architect 与 product-manager 漏掉的 5 个上下文预算标识符全部回收。
   范围、可执行性/验收/方法、预算三段一并保留，措辞从"字段指涉"改写为"内容要求"，
   要求本身逐条未变。

   **既有 eval 结果的处置**：本期删除了四段 Output、指纹已变，
   before/after 两轮与两份复核结果**与今后的轮次不可比**，下一轮必须重建基线。
   指纹机制存在的理由正是防止拿旧基线给新提示词背书，这条不能自己破例。
   既然不可比，运行产物就没有入库的价值：`eval/results/` 与
   `eval/results-recheck-*/` 已移出版本库（`runs.jsonl` 本就 gitignore，
   只留 summary 等于留下一个无法追溯到原始运行的数字）。**结论写进本文档，
   数字不进仓库**；评测的输入 `eval/agent-cases/` 保持跟踪且保持钉版——
   fixture 跟着 HEAD 漂就无法把差异归因到提示词的改动。

## 9. 实施顺序注记

M5-1 与 M5-2 之间没有依赖，但**必须先 M5-2 跑出 before 基线再做 M5-3**：
否则第三期改完提示词无法归因，等于又一轮空谈。
M5-1 先行的理由是它是纯机制、零提示词改动，不会污染基线指纹。
