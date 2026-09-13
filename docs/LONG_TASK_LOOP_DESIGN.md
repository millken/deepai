# 长任务闭环(Mission Loop)设计 — 设计 → 评审 → 实施 → 评审

> 状态:**已实施(2026-09-13)**,Phase 1–5 全部落地,见 §十四「实施记录」。第 7 轮评审修订的正文即实现依据;实现中有两处对本文的偏离,已在 §十四 列明。v1 自审见 §八 C1–C12;v2–v6 见 §八;v7 响应 R36–R39(终态清注入、`design_failed` 两种含义、评审范围收窄、外部写者),处置见 §八。
> 目标:让一次长任务在无人逐步批准的情况下走完 **设计 → 设计评审(可多轮) → 实施 → 实施评审(可多轮)** ,并且用**章程(charter)**而不是对话记忆来防止跑偏。
> 前置:对抗式实施审查已落地([ADVERSARIAL_REVIEW_DESIGN.md](ADVERSARIAL_REVIEW_DESIGN.md),`pkg/chat/review.go` 的 `runEpisode`);plan mode 已落地(`pkg/agent/plan.go`);确定性编排层已被实测否决(`87772b6` 删除 `pkg/orchestrator`)。

---

## 一、背景:三个"显然的方案"都不行

### 1.1 纯提示词驱动 —— 已被判定脆弱

在系统提示里要求主 agent "先设计、再请人审、再实施、再审"零成本,但链路能否走完取决于模型发挥。`docs/AUTONOMOUS_MULTI_AGENT.md` 缺失能力 #2/#4 的原话:**协调全靠主模型即兴决定调谁,脆弱、无结构保障**;reviewer 类型存在,但**无机制自动跑完整闭环**。实施侧的对抗审查已经用代码门补上了后半段;前半段(设计及其评审)和"两段之间的衔接"仍靠即兴。

### 1.2 独立编排引擎 —— 已被实测证伪

commit `87772b6`(2026-06-09,"实测编排功能不可用,整体移除")删除了 `pkg/orchestrator/`(implement→verify→review→fix、design 面板、build 串联、黑板、`MaxAgentCalls`)。`docs/ARCHITECTURE_REVIEW.md` §5.4 的结论仍然有效:**不要重建确定性编排层**。教训是:在 ReAct 循环**之外**另建一层状态机,与会话/事件/持久化脱节,不可维护。

### 1.3 把实施审查再套一层"全流程编排" —— 会重蹈覆辙

`runEpisode` 已经是"寄生在 REPL turn 循环上的有界 for"。正确的推广是**再包一层同样形态的有界循环**,而不是引入工作流引擎、阶段注册表、或 fan-out 编排 DSL。新循环只多做三件事:强制设计阶段只读、把设计评审做成和 `reviewGate` 同构的门、用落盘章程锁住目标以免长任务漂走。

### 1.4 本方案的定位

同样的 design→review→implement→review 语义,但**寄生在 REPL 现有的 turn 循环上**,不新建编排层:

- 任务循环是 `pkg/chat/mission.go` 的 `runMission`,`runTurn` 本身不改;
- DESIGN / IMPLEMENT 两相都由 `runMission` 自己的同形 for 驱动。实施相**不调用** `runEpisode`——`runEpisode` 只有 `fixMsg` / `""` 两种出口(`review.go:308-336`),表达不了 S1 的当场升层;
- 实施评审复用 `reviewGate` / `dispatchReview` / 快照与归因,不复制门逻辑;`reviewGate` 改为返回结构化结果,`runEpisode` 只取其中的修复消息,普通对话零分叉;
- 设计评审复用现有 subagent 池 + `task` 执行链路 + `ParseOutput`;
- 每个设计修订轮、每个实施轮都是一个普通 turn,持久化/记忆调度/事件渲染全部走现成路径。

---

## 二、问题定义:长任务如何"跑偏"

跑偏不是一种感觉,是四类可观测失败。本设计的全部机制都对着这四类:

| # | 跑偏形态 | 典型症状 | 本设计的对策 |
|---|---|---|---|
| D1 | **目标漂移** | 压缩/多轮修订之后,agent 开始做"对话里最近提到的事",而不是用户最初要的事 | 章程锁定原始 brief,每轮与每次评审只对照章程,不对照最新闲聊 |
| D2 | **范围膨胀** | 修一个函数变成顺手重构邻包 | 章程锁 `scope_files`;实施门用代码做硬范围检查。越界走自己的计数器,不耗实施评审轮次 |
| D3 | **计划-实现错位** | 设计写了 A,代码做了 B,两边各自自洽 | 实施评审 prompt 带章程的 acceptance;未满足的准则是结构化 issue,不是散文 |
| D4 | **设计本身错了还继续堆代码** | 实施评审反复打回同一处,根因在接口选错 | 升层有**两条**信号:reviewer 填 `fault_layer=design`(提示词必须为此改 Rule 3),以及代码侧兜底(见 §5.4)。升层后的设计相有独立轮次额度 |

次要但必须处理的失败:

| # | 形态 | 对策 |
|---|---|---|
| D5 | 自证偏差 | 设计评审与实施评审都是独立子代理,看不到实现者推理(沿用对抗审查的信息隔离) |
| D6 | 审查空转 | 每层硬编码轮数上限;升层也有上限;升层后设计轮另计 |
| D7 | 压缩失忆 | 章程在磁盘 + `SessionCarry` 经 `buildTurnInjection` 尾部注入,不把"唯一副本"放在可被 compact 的消息里 |
| D8 | 用户逐步批准把"自动"打断 | 任务循环内 `exit_plan_mode` 不再弹 Yes/Revise/Cancel,改由设计评审门接管 |

---

## 三、现状与差距

| 能力 | 现状 | 差距 |
|---|---|---|
| 只读设计阶段 | plan mode:`enter_plan_mode` / `write_plan` / `exit_plan_mode`;计划落 `.deepai/plans/<ts>.md`(`plan.go`);内容上限 64KiB(`plan.go:151`);`planToolNames` 8 个只读工具(含 `present_file`),无 `task` | ⚠️ 三处生命周期缺口,见 R9/R10/人闸:① REPL 每 turn `agent.New`(`repl.go:1279`),`cfg.PlanMode` 为真则构造期 `enterPlanMode` → 无条件 `initPlanFile()` 新开秒级路径(`react.go:330-331`,`plan.go:56,81-90`),跨轮计划文件对不齐;② `a.exitPlanMode()` 是唯一退出路径,任务内若禁止它退出,turn 末 `r.planMode = runAgent.IsPlanMode()`(`repl.go:1489`)会把 true 续进 IMPLEMENT;③ `exit_plan_mode` 在有 `UserInteraction` 时阻塞等人批准(`plan.go:225-275`) |
| 设计产出角色 | `architect` / `product-manager` 子代理存在,无 Strict schema(M5-4 已撤非程序消费的 schema) | ⚠️ 角色在,但**没有代码读取**它们的产出做门禁。types_config.go:388-396 写明:schema 只有被程序消费才配活 |
| 设计评审 | `arch-reviewer` 审的是**代码 diff 的架构**,不是计划文档 | ❌ 缺"对照 brief 审一份设计稿"的 reviewer |
| 实施→评审→修复 | `runEpisode` + `correctness-reviewer`,`maxReviewRounds=2`,fail-soft,快照归因;`reviewMaxToolCalls=20`;预算/超时默认 150k / 10m | ⚠️ 复用门与归因,不复用 `runEpisode` 外壳(只有两出口,见 R17);快照守卫绑在 `ReviewAfterEdit` 上(R18) |
| 范围锁定 | 实施审查范围 = 本轮归因文件(`carry.EditedFiles() ∪ changedSince`),对照的是 episode 起始用户句 | ❌ 不对照设计范围;越界没有硬门 |
| 升回设计 | 实施审查 2 轮后交人工,不会重开设计。correctness-reviewer Rule 3(`types_config.go` 该提示词)把"diff 未触及的既有问题"定为越界,会**主动阻止**把根因归到计划 | ❌ 缺 `fault_layer` 字段、缺提示词开口、缺代码兜底信号 |
| 长任务状态 | 会话线性消息 + 易失的 `SessionCarry`;sessions 表已有 `metadata` JSON 列 | ❌ 中断后续跑没有"停在设计还是实施"的落盘状态 |
| 触发 | 模型自觉进 plan mode;实施审查靠 `review_after_edit` opt-in | ❌ 没有"从这一句开始跑完整闭环"的入口 |

---

## 四、总体架构

```
用户 /mission <任务>  或  (可选) enter_plan_mode 升级
        │
        ▼
   runMission(mission.go)                  ← 外层有界循环,不进 runTurn
        │
        ├─ 落盘 .deepai/missions/<id>/brief.md = 起始用户句(此后只读)
        ▼
   ┌──────────── DESIGN 相 ────────────┐
   │  每个 turn: PlanMode=true,         │
   │    PlanFile=.deepai/missions/<id>/design.md
   │  门: 只读这一份 design.md          │
   │  轮次: 初始 3;升层后另计 2         │
   │  fail 且未达本相上限 → 修订,仍 DESIGN
   │  fail 且达上限 → status=design_failed(leaveMission)
   │  pass → 锁章程;runMission 置       │
   │    r.planMode=false 后进 IMPLEMENT │
   └────────────┬─────────────────────┘
                │
                ▼
   ┌──────────── IMPLEMENT 相 ─────────┐
   │  mission.go 同形 for,不调用 runEpisode
   │  每 turn 进门前: r.planMode=false,
   │    不注册 enter_plan_mode
   │  进相拍 S_impl(不论 ReviewAfterEdit);门内拍 after
   │  reviewGate(before=S_impl) → gateResult{next, escalate, passed}
   │  escalate → 回 DESIGN;next 非空 → 修复/范围/空转催促
   │  passed → leaveMission(done);空转/轮次用尽且未 pass → leaveMission(handed_over)
   └──────────────────────────────────┘
```

两相都是"turn 结束后面的门"。复用的是门、快照、归因,不是 `runEpisode` 外壳。

```
DESIGN ──审──▶ DESIGN(修订) ──审──▶ IMPLEMENT
                                      │
                                      ▼
                                 范围硬门? ──越界──▶ 范围修复(独立计数)
                                      │通过
                                      ▼
                                 实施评审
                                      │
                       ┌── pass ──────┴── fail ──┐
                       ▼                         ▼
                  status=done          升层信号?
                                         │是            │否
                                         ▼              ▼
                                      DESIGN        修复轮
```

---

## 五、详细设计

### 5.1 何时进入任务循环(触发)

三档,互不打架:

| 档 | 入口 | 默认 |
|---|---|---|
| 显式 | `/mission [文本]`。有文本 = 新任务;无文本 = 只认**当前会话** `metadata.mission_id` 指向且 `status==active` 的任务,查不到或已终态则要求带文本,不扫描磁盘上其他会话留下的任务 | 唯一默认入口 |
| 升级 | 配置 `mission_on_plan: true` 时,普通 episode 里主 agent 调用了 `enter_plan_mode`,该 turn 结束后把**剩余工作**升级为任务 | **关** |
| 普通 | 不进 `runMission`,仍走今天的 `runEpisode` | 缺省路径,行为零变化 |

不按启发式(长度、"复杂"、模型自报)自动开任务——误开的成本是一次设计评审 + 可能的实施审查,和 `review_after_edit` 首发默认关是同一理由。

新任务的第一个 DESIGN turn **不以** `/mission` 原文当唯一输入:先把原文写入 `brief.md`,再以 §5.6 的 `[mission-design round 1/3]` 合成消息跑 `runTurn`(内含 brief + 计划必须列出的字段 + `exit_plan_mode` 不再等人)。这样第一轮设计评审不会因为 planModePrompt 缺 G/W/T 而结构性 fail(R20)。

升级时:该 turn 的 `enter_plan_mode` 已经按现状写到 `.deepai/plans/<ts>.md`。建任务目录后,若那份文件非空则**复制**到 `design.md`,之后所有 DESIGN turn 只认 `design.md`(§5.2)。不把旧时间戳路径继续当权威。

`/mission abort` 结束当前任务(不回滚已落地的编辑,与实施审查"第 3 次 fail 不回滚"一致):走 `leaveMission(aborted)`(§5.5),清掉 `metadata.mission_id`。`/mission status` 打印 `status`、相、轮次、章程摘要。续跑只认 `status==active`(R32)。

**`/clear` 与活动任务(R24)**:`clearSession`(`repl.go:2103`)会换新 `SessionCarry` 并清空消息,章程渲染即丢,磁盘上的任务却仍是"未完成"。任务进行中的 `/clear` **先走与 `/mission abort` 同一条路径**,再执行今天的清会话。不留"空会话 + 磁盘未完成"的半截状态。`/new` 另开会话,旧任务留在旧会话的 metadata 里,不自动 abort。

### 5.2 DESIGN 相:复用 plan mode,接管批准权

**谁来设计**:主 agent,强制 `PlanMode=true`。不派 `architect` 子代理。理由:主 agent 持有用户澄清与会话;子代理从零起跑且无记忆(`subagent.go` 不传 `MemoryService`);orchestrator 把活丢给孤立 coder 的路已经失败过。

**工具集**:`planToolNames` 现为 8 个只读工具(`plan.go:21-23`:`read_file` / `list_dir` / `glob` / `grep` / `find` / `code_map` / `ask_clarification` / `present_file`),再加上 Restrict 之后动态注册的 `write_plan` 与 `exit_plan_mode`。**仍然没有 `task`**,防止借 coder 绕过只读。

**计划文件钉死(R9,Phase 3 必做)**:REPL 每 turn `agent.New`(`repl.go:1279`),`cfg.PlanMode` 为真则构造期 `enterPlanMode()` → 今天无条件 `initPlanFile()` 生成新的秒级路径(`react.go:330-331`,`plan.go:56,81-90`)。DESIGN 修订轮若再走这条路,上一轮计划留在旧文件,新 agent 拿到空 `planFile`;门若读新路径则判空,催促消息白烧一轮。

修法:

```
AgentConfig 增 PlanFile string
New(): a.planFile = cfg.PlanFile
enterPlanMode(): 仅当 a.planFile == "" 时才 initPlanFile()
runMission 每个 DESIGN turn:
    PlanMode = true
    PlanFile = <workdir>/.deepai/missions/<id>/design.md
门只读这一份 design.md:非空才派设计评审;空则催促,计一轮
```

非任务路径(`PlanFile` 空)行为与今天逐字相同。`write_plan` 是 `os.WriteFile` 全量覆盖(`plan.go:170`),不是补丁——修订轮必须整份重发计划。合成修订消息须写明这一点,并给出 `design.md` 路径,避免模型只口头改、不再调用 `write_plan`。

**`exit_plan_mode` 在任务内的语义**(对码:`plan.go:225-275` 在 `ui != nil` 时 `AskQuestion` 三选一):

```
若 ctx 带 mission 且相 == DESIGN:
    不询问用户、不退出 plan mode
    返回:"计划已提交独立设计评审。继续留在 plan mode。"
否则:
    保持今天的行为(交互批准 / 非交互自动通过)
```

门不看"是否调用了 exit_plan_mode",只看 `design.md` 是否非空。agent 只 `write_plan` 不 exit 也一样进门。

**谁退出 plan mode(R10)**:任务内禁止 `exit_plan_mode` 调用 `a.exitPlanMode()` 之后,那条函数仍是 agent 侧唯一退出路径;turn 末 `r.planMode = runAgent.IsPlanMode()`(`repl.go:1489`)会把 `true` 读回 REPL。若没人翻转,下一 turn `AgentConfig.PlanMode` 仍为真 → IMPLEMENT 拿到只读工具集,任务卡死。

**每 turn 进门前按相强制 `r.planMode`(R10 + R21)**:只在转换点翻转防不住中途漂移——`enter_plan_mode` 对交互 agent 恒注册(`react.go:327`),IMPLEMENT 中模型一调用它,readback 就把 true 读回,下一 turn 又是只读,R10 的卡死原样复现。

```
每个 runMission 驱动的 turn,在构造 AgentConfig 之前:
    DESIGN    → r.planMode = true;  PlanFile = design.md
    IMPLEMENT → r.planMode = false; PlanFile = ""
                AgentConfig.DisableEnterPlan = true
                // New() 跳过 registerPlanTools,本 turn 没有 enter_plan_mode
    r.ui.SetStatus(r.currentModel, r.planMode)
```

转换点(设计 pass / 升回)只改**相**,下一 turn 的强制赋值覆盖 readback。`DisableEnterPlan` 缺省 false,非任务路径零变化。

设计门 pass 之后立刻把相切到 IMPLEMENT,不必等下一拍才改 `r.planMode`(页脚与工具集以进门前强制为准)。

**设计评审者拿到什么**(信息隔离,镜像实施审查 ADVERSARIAL_REVIEW_DESIGN §4.4):

| 给 | 不给 | 理由 |
|---|---|---|
| `brief.md`(任务起始句,固定锚点) | 主 agent 的思考/工具轨迹 | 不被实现者的思路带偏 |
| 当前 `design.md`(计划全文) | 整个会话历史 | 聚焦待审文档 |
| 上一轮设计裁决(修订轮) | — | 让 reviewer 审"反驳是否成立",而不是换词重报 |
| 若由 IMPLEMENT 升回:升级原因 + 当时的实施 issues | — | 设计必须回答"为什么原章程在代码里走不通" |

**新内置类型 `design-reviewer`**:

```go
AgentTypeDesignReviewer: {
    Type:         AgentTypeDesignReviewer, // "design-reviewer"
    Name:         "Design Reviewer",
    Description:  "Adversarially reviews a design/plan against the original brief.",
    SystemPrompt: designReviewerSystemPrompt,
    DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
    MaxToolCalls: 0, // 与另外四个 reviewer profile 相同:直接 task 调用不封顶
    Temperature:  0.2,
    OutputSchema: FromStruct[DesignReviewResult](WithStrict(true), WithMaxRetries(1)),
}
```

- **不给 bash**:设计稿没有可编译的硬信号;给 bash 只会诱使 reviewer 去"顺便验证"而碰树。只读工具够核对计划是否引用了真实标识符。
- **不复用 `arch-reviewer`**:那条提示词的 scope 是"THIS change"(代码 diff),和"对照 brief 审计划"不是同一份考纲。
- **门侧封顶,不是 profile 封顶**:设计门派发时传入 `max_tool_calls: reviewMaxToolCalls`(**20**,`review.go:283`),与实施审查同一常量。配额耗尽可恢复(强制收尾仍须满足 Strict schema);墙钟到点不可恢复。这是 `13e883f` 的既定分工,design-reviewer 不另作例外。
- **预算与超时**:不新开配置项。设计门读的是已经解析进 `ReplConfig` 的 `ReviewTokenBudget` / `ReviewTimeout`(`pkg/chat` 调不到 `pkg/commands` 里未导出的 resolver;`resolveReviewTokenBudget` / `resolveReviewTimeout` 只在装配时跑一次,`pkg/commands/chat.go:371-372`)。缺省即 **150_000 token / 10 分钟**(常量:`pkg/commands/review_config.go:25`,`pkg/chat/review.go:265`)。30k / 5m 是设计原文里已被实测打掉的两个数:超预算在子代理里是硬错误且 `FinalOutput` 为空(`react.go`);5 分钟会被推理模型的思考时间常规打穿(本机配置若用 glm-5.3 即属此类;仓库内置 glm 默认是 `glm-4-plus` / `glm-4-flash`,`setup.go:122`)。设计评审的输入不比代码评审小(brief + 最多 64KiB 计划),没有收紧的理由。装配语义:config 0/缺省 → 上述默认;负值 → 不限。`resolveReviewTokenBudget` 函数注释仍写"30k"(`review_config.go:33`)是代码债,以常量为准。

提示词核心约束(与 correctness-reviewer 同构,只换对象):

```
You are an independent adversarial design reviewer.
You receive the original brief and a plan document. You do NOT see
the author's reasoning.

Pass only if all of the following hold:
1. The plan solves the brief — not a nearby or larger problem.
2. In-scope files are named as they exist (or will exist) in the repo;
   out-of-scope is explicit. For a Go change, name the *_test.go files
   the plan expects to add or edit — the implementer will write them.
3. Every acceptance criterion is Given/When/Then with one observable
   outcome. A criterion you cannot imagine falsifying does not count.
4. Each Decision that cites existing code names file + exact identifier.

Rules:
1. Do not assume the plan's intent is the brief's intent — verify it.
2. Every issue MUST include a concrete failure scenario: if this plan
   were implemented as written, what observable thing would be wrong
   or missing. An issue without a scenario does not count.
3. You MUST NOT edit any project file.
4. If you cannot construct a failure scenario, verdict "pass".
   Do not fail a plan for style or taste.
5. On pass you MUST fill scope_files and acceptance; empty either
   is treated as fail by the gate (not by you hedging).
6. Your run is bounded. Reason from the plan first; spend tool calls
   only to verify identifiers exist. Emit the verdict while you still
   have budget.
```

第 2 条要求点名测试文件是**软约束**(计划阶段很难穷举)。硬约束在实施相的测试伴生豁免(§5.4),不依赖 reviewer 一次列全。

**`DesignReviewResult`**(新 schema,`namedSchemas["design_review"]`):

```go
type DesignReviewResult struct {
    Agent      string   `json:"agent"`
    Verdict    string   `json:"verdict"`
    Summary    string   `json:"summary"`
    Issues     []Issue  `json:"issues"`
    ScopeFiles []string `json:"scope_files"`
    Acceptance []string `json:"acceptance"`
}
```

`Issue` 增两个 `omitempty` 字段(现有四类 reviewer 的 strict 校验不受影响,infer 规则与 `Scenario` 相同,已在 ADVERSARIAL_REVIEW_DESIGN §4.3 核实):

```go
// Area 供 design-reviewer: scope | feasibility | completeness | acceptance | risk
Area string `json:"area,omitempty"`
// FaultLayer 供实施评审升层: implementation | design
FaultLayer string `json:"fault_layer,omitempty"`
```

**pass 的代码判定**(不只信 `verdict` 字符串):

```
isDesignPass(v) :=
    verdict=="pass" (或 issues 为空,沿用 isPassVerdict)
    && len(scope_files) > 0
    && len(acceptance) > 0
    && 每条 acceptance 含可观察结果(最低:非空且长度 >= 12)
```

缺 `scope_files`/`acceptance` 的 "pass" 按 fail 处理并回注"章程字段不完整"——否则锁门会写下空范围,实施相的硬范围检查将放行一切或放行虚无。

**有界性**(两套计数,不互相重置):

| 计数器 | 值 | 何时用 |
|---|---|---|
| `maxDesignRounds` | **3** | 任务起始的 DESIGN 相 |
| `maxEscalatedDesignRounds` | **2** | 每次从 IMPLEMENT 升回后的 DESIGN 相,从 0 另计 |
| `maxDesignEscalations` | **1** | 整次任务最多升回一次 |

第 3 次(或升层后第 2 次)仍 fail → `leaveMission(design_failed)`,把 issues 呈给用户,**不自动开工**(升层后的那次 worktree 上可能已有未审实施,见 §5.5 / R37)。最坏设计评审次数 = 3 + 2 = 5,有界;升层后仍允许一轮修订再重审,不会出现"初始花了 2 轮、升回去只剩 0–1 轮且不容修订"。

### 5.3 章程锁定:防跑偏的唯一源

设计门 pass 时写入 `.deepai/missions/<id>/charter.lock.json`,此后 IMPLEMENT 相只认这一份:

```go
type Charter struct {
    MissionID    string   `json:"mission_id"`
    Brief        string   `json:"brief"`         // 任务起始句,永不改
    DesignHash   string   `json:"design_hash"`   // sha256(design.md)
    ScopeFiles   []string `json:"scope_files"`   // 锁门时已规范化,见下
    Acceptance   []string `json:"acceptance"`
    LockedAt     time.Time `json:"locked_at"`
    Escalation   int      `json:"escalation"`    // 已从实施升回设计的次数
}
```

锁章程时 **`runMission` 规范化 `scope_files`**(R19),不信任 LLM 原文:

```
对每条路径: strings.TrimSpace → filepath.ToSlash → filepath.Clean
去掉开头 "./"
相对 workdir(若已是绝对路径且在 workdir 下则 Rel;在 workdir 外则丢弃并记 reviews.jsonl)
去重,保序
允许尚不存在的路径(Pass 条件 2 的 "or will exist")
空结果 → isDesignPass 失败(与缺字段同)
```

落盘与内存 charter 都用这份规范化列表。同时复制 `design.md` 为 `design.locked.md`。磁盘上的 `write_plan` 文件可以继续改,但**门与注入读的是 locked 副本**,直到下一次设计 pass 换锁。

**注入路径**(D7):不把章程只追加成一条 user 消息——compact/aging 会摘要或丢掉它。走与 memory / todo 相同的 per-Run 尾部注入:

- `SessionCarry` 增 `charter *Charter`(或渲染好的只读文本 + hash);`leaveMission` 与升层动作必须把它置空(或升层时改挂归档+原因,不得标 locked)。注入只在 carry 上有章程段时发出——终态后普通 turn 不得再看到 locked charter(R36)。
- `buildTurnInjection` 定义在 `promptbuild.go:326`。`react.go:532` 在 Run 开头算一次;skill / todo 变化后 `toolexec.go:527`、`:554` 会在**同一 turn 中途重算**。章程挂在这段注入上,中途重算仍会带上它。位置必须在**尾部**,与现有 memory/todo 注入同侧,不碰系统提示前缀(前缀不稳会打掉 provider prompt cache)。段名只是注入文本的标题,不是新的组装 API。
- 实施评审 prompt 的"原始需求"**改为章程**:brief + acceptance + 已规范化的 scope_files + design.locked.md。`buildReviewPrompt` 增加可选 charter 块;`reviewGate` 在任务内把 brief 换成章程锚点。

压缩之后模型仍能看见章程,因为注入发生在每次 LLM 请求前,不依赖消息列表里那份。

### 5.4 IMPLEMENT 相:复用门,不调用 `runEpisode`

`runEpisode` 只有两种出口(`review.go:308-336`):`reviewGate` 返回非空 `fixMsg` → 再修;返回 `""` → 结束。pass、fail-soft、轮次用尽交人工全部塌缩成 `""`。S1 的"当场升层"是第三种出口,现契约表达不了。

**拍板(R17,不留 hedge)**:

1. `reviewGate` 改为返回
   ```
   type gateResult struct {
       next     string // 修复/范围/空转催促;空 = 本相不再自动续轮
       escalate string // "", "fault_layer", "repeat_file", "scope"
       passed   bool   // 仅当 correctness-reviewer 给出 pass 裁决(R31)
   }
   ```
   `next==""` 不再表示"评审 pass"。今天的门对 `len(scope)==0` 直接 `return ""`(`review.go:361-363`),pass、无可审、fail-soft、轮次用尽全部塌缩成空串。任务必须把"无可审"从 pass 里拆开,否则纯文本 turn 或 revert 光了越界文件会静默 DONE。
2. `runEpisode` 只读 `next`。无活动任务时 `escalate` 恒空、`passed` 可忽略,行为与今天逐字相同。
3. `runMission` 的 IMPLEMENT 相**自己写同形 for**,调用 `runTurn` + `reviewGate(before=S_impl)`,解释 `escalate` / `passed`。**不调用 `runEpisode`**。循环里不再拍 per-turn `before`(R33)。
4. 复用的是 `dispatchReview`、归因、`buildReviewPrompt`,不复制门逻辑。

"不写第二套 episode"收窄为:不复制审查门;第三出口属于任务相,循环外壳在 `mission.go`。

```
进入 IMPLEMENT 相时(设计 pass 之后,第一拍之前):
    r.carry.ClearEditedFiles()
    r.reviewPrev = nil
    S_impl = takeWorktreeSnapshot(workdir)   // 相起点,落盘 implement.baseline;不论 ReviewAfterEdit
    persist status=active
    下一 turn 输入 = [mission-implement] 合成消息(§5.6)

for {
    按 IMPLEMENT 强制 r.planMode=false、DisableEnterPlan=true
    runTurn(...)
    if turnErr != nil { return }             // 残缺编辑不审;status 仍 active
    out = reviewGate(parentCtx, request, S_impl, round)  // before 即 S_impl(R33)
    if out.escalate != "" && escalation 未尽 {
        r.reviewPrev = nil
        r.carry.ClearEditedFiles()
        清掉 carry 上的章程注入(R28)
        升层回 DESIGN; break
    }
    if out.passed {
        leaveMission(done); return
    }
    if out.next == "" {
        leaveMission(handed_over); return   // 实施轮次用尽 / 空转用尽 / fail-soft
    }
    以 out.next 为下一 turn 输入          // 修复 / 范围 / [mission-idle]
}
```

`ClearEditedFiles` / `reviewPrev = nil` 必须由 `mission.go` 在进相与离相时做:IMPLEMENT 循环不再调用 `runEpisode`,而那两步目前只活在 `runEpisode` 入口(`review.go:313-314`)。清空的理由(R27 改写):① `reviewPrev=nil` 防止 S2 拿跨相旧裁决比较;② 非 git 下评审范围回退到 `EditedFiles`(R30),DESIGN 前普通对话的工具记录必须先洗掉。范围硬门在 git 可用时不再读累积记录(R25),所以"残留归因进第一轮范围门"不再是理由。

**空 scope ≠ 完成(R31)**:IMPLEMENT 相内,git 可用时 `stillDirty` 为空、或非 git 时 `EditedFiles` 为空,都是"无可审",不是 pass。门合成 `[mission-idle]`(§5.6),独立计数 `idleRound`,`maxIdleRounds=2`。两种真实场景:第一个 IMPLEMENT turn 只说话不动手;范围修复轮把越界文件全部 revert 后恰好没有其他改动。两次空转后 `next==""` 且 `passed==false` → `status=handed_over`,不标 done。只有 correctness-reviewer 给出至少一次 pass 裁决才写 `status=done`。任务内实施评审 fail-soft(超时/挂掉/不可解析)同样 `passed==false`、`next==""` → `handed_over`,明示未经审查,不得塌成 done。

#### 5.4.0 `reviewGate` 任务分支(R22,选定一案)

不新增 `force bool` 参数。`reviewGate` 读 REPL 上的活动任务:当 `r.mission != nil && r.mission.phase == IMPLEMENT` 时走任务分支,否则走今天的两行守卫。普通 `/review` 与 `runEpisode` 零分叉。`r.mission` 只在任务 `status==active` 时非空;任一终态都置 nil(§5.5 `leaveMission`,R36),所以任务结束后下一拍普通 turn 自动走普通守卫。

#### 5.4.1 章程注入

见 §5.3。`buildReviewPrompt` 在任务内必须带上 locked charter;correctness-reviewer 的"stated task"就是这份章程,不是最新闲聊。

#### 5.4.2 范围硬门(D2):当下状态复核,不信任累积记录

`SessionCarry.EditedFiles()` 只增不减,唯一清空口是 `ClearEditedFiles()`(`session_carry.go:115-131`)。模型按范围修复消息 revert 了 `pkg/unexpected/foo.go` 之后,该路径仍留在集合里(revert 本身还可能再被 `RecordEditedFile` 写进去)。若硬门继续做 `edited \ allow`,第二次进门依旧越界 → `scopeRound >= 2` → S3 误升层,烧掉整次任务唯一的升层额度(R25)。

**拍板**:越界判定不信任累积工具记录,也不给 carry 加 per-file 移除口。权威是**相对 IMPLEMENT 相起点快照 `S_impl` 的当前差集**(与快照侧 `changedSince` 只遍历当下 dirty 条目的语义对齐,`review.go:113-118`)。

```
进入 IMPLEMENT 时拍 S_impl,落盘 implement.baseline(path → stamp)
门口 after = takeWorktreeSnapshot(workdir)   // 门内拍,循环不再拍 per-turn before
gitOK = after.root != "" && S_impl.root != ""

if gitOK:
    stillDirty  = Rel(workdir, after.changedSince(S_impl))
    allow       = charter.scope_files ∪ 范围豁免 ∪ 测试伴生
    越界        = stillDirty \ allow
    reviewScope = stillDirty ∩ (charter.scope_files ∪ 测试伴生)   // 豁免只免越界,不进评审(R38)
else:
    范围硬门关闭,警示一次;S3 禁用
    reviewScope = resolveWorktreePaths(carry.EditedFiles())   // 与今天一致(R30)
    越界        = ∅

若越界非空:
    不派 reviewer
    合成 scope-violation issues
    走范围修复轮,递增 scopeRound,不递增 reviewRound
若 reviewScope 为空:
    无可审,不是 pass(R31);走 [mission-idle],见循环
revert 回 git-clean(文件从 porcelain 消失) → 不在 stillDirty → 退出越界集
```

路径在做差之前仍统一为 workdir 相对(R19)。测试伴生匹配也在相对形态下做。

**非 git 只关硬门,不关实施评审(R30)**:今天的门注释原话是"the tool side survives non-git directories and snapshot failures"(`review.go:353-355`)。`S_impl` 为零值时 `changedSince` 恒返回 nil(`review.go:108-118`)。若评审范围也改成 `stillDirty ∩ …`,则 reviewScope 恒空,correctness-reviewer 一次都不会派。拍板:git 可用时 `stillDirty` 当权威(硬门 + 评审范围);非 git 时硬门与 S3 关闭,评审范围回退到 `resolveWorktreePaths(carry.EditedFiles())`。

**stamp 一致达不到(R35)**:`fileStamp` 含 `modTime`(`review.go:54-57`),任何 revert 都更新 mtime。对相起点已经 dirty 的文件,revert 到起点内容后 stamp 仍不等,永远洗不掉。"stamp 回到与 S_impl 一致"仅理论,不写进判定。实际唯一清除路径是文件从 `git status --porcelain` 消失。常见场景(任务新建/新改的文件删除或 `git checkout --`)走这条路,不受影响。不在本期用内容 hash 替代——与今天 episode 归因同一条残差(`review.go:28-33`)。

**范围豁免**(硬编码,不配置):`.deepai/**`、`go.sum`、`go.work.sum`、`*.lock`。豁免的是生成物与任务元数据——只免于越界判定,不进入 `reviewScope`(R38)。`allow ∪ 测试伴生` 是冗余写法:测试伴生已在 `allow` 里;若评审范围跟 `allow` 做交,生成物会被喂给 reviewer,浪费门侧 20 次工具调用。

**测试伴生**(R3,硬编码):本仓库实施纪律是 TDD 红灯先行,每个实施轮都会写 `*_test.go`,设计阶段无法穷举。下列路径视为在范围内,即使 `scope_files` 没写:

- `foo_test.go`,当 `foo.go` 在 `scope_files` 中(去 `_test` 后缀后的实现文件在范围内);
- 任一 `*_test.go`,与某个范围内 `.go` 文件位于同一目录;
- 该目录下的 `testdata/**`。

只豁免测试与其夹具,不豁免同目录里新冒出来的非测试 `.go`。漏列的实现文件仍走硬门。

**独立计数**:`maxScopeFixRounds = 2`(常量)。两次越界修复都不占用 `maxReviewRounds`。检测不派 reviewer(零 LLM);修复轮是一个完整的 LLM turn(模型要执行 revert),只是不再派实施评审(R29)。论据是"不与真缺陷评审抢 `maxReviewRounds`",不是"修复免费"。

范围修复轮的合成消息只要求撤回或证明该文件是测试伴生(伴生判定失败才到这里)。**不**在这条消息里教模型去改章程——改章程的唯一入口是升层。

#### 5.4.3 升层信号:提示词开口 + 代码兜底(D4 / R1)

v1 只加了 `Issue.FaultLayer` schema 字段,没有改 correctness-reviewer 提示词。其对码 Rule 3 原文是:

> THIS change is the entire scope: a defect it introduces, or one it was supposed to fix and did not. A pre-existing problem in code the diff does not touch is out of scope no matter how real it is — do not report it.

"章程本身选错了接口"正好被这条规则叫停。所以不是"模型可能忘了填",是提示词在**积极阻止**它填。Phase 1 必须改提示词,字段才能活。

**提示词开口**(任务内才生效,普通 `/review` 无章程、行为不变)。两处同时写,互相 redundance:

1. `correctnessReviewerSystemPrompt` 在 Rule 3 后插入一条**有条件**的规则:

```
3a. If and only if the user message includes a locked charter
    (brief + scope_files + acceptance + plan): judge the change
    against the brief as well as the plan. If the change faithfully
    implements the plan but the plan cannot satisfy the brief —
    wrong interface, files that must exist but are not in scope,
    acceptance that cannot be true in this codebase — set
    issue.fault_layer="design" and name the charter clause that
    cannot hold. That is a defect in the plan, not a pre-existing
    bug in untouched code, and is in scope. Otherwise omit
    fault_layer or set "implementation".
```

2. `buildReviewPrompt` 的章程块用一句话重复:"若根因在计划,填 fault_layer=design 并点名章程哪一条走不通。"

Rule 3 本身不删——没有章程时它仍然在保护"不要把既有 bug 算进修复轮"。

**代码兜底**(不信任模型一定填字段)。任务分支的 `reviewGate` 在升层次数未尽时置 `escalate`,由 `runMission` 消费(R17)。下列任一即升层:

| # | 信号 | 何时 |
|---|---|---|
| S1 | 任一条 issue `FaultLayer=="design"`(大小写不敏感) | 当场,不等轮次用尽 |
| S2 | 将要交人工时(`reviewRound >= maxReviewRounds`)且连续两轮 fail 的 issue 集合共享至少一条 `File` | 代替"呈给用户",先给一次重做设计的机会 |
| S3 | `scopeRound >= maxScopeFixRounds` 且越界文件在测试伴生判定之后仍非空 | 反复改章程外的实现文件 = 章程漏了必改文件 |

S2 的假阳性:一个难修的实现 bug 会烧掉唯一一次升层。接受——升层后还有 2 轮设计,最坏是多付一次设计相;不接受的是 D4 完全纸面化。

升层动作:`runMission` 把 `charter.lock.json` 改名为 `charter.vN.json`;清掉 `SessionCarry` 上的章程注入(或改挂"归档章程 + 升层原因"段,不得再标 locked,R28);`r.reviewPrev = nil`;`r.carry.ClearEditedFiles()`;合成 `[mission-escalate]`(含实施 issues / 越界文件 / "原章程何处走不通");`escalation++`;设计轮另计。新章程锁定后再把 charter 挂回 carry。

`maxDesignEscalations = 1`。再错交人工。

#### 5.4.4 与 `review_after_edit` / 快照(R18)

现门与 episode 有**两道** `ReviewAfterEdit` 守卫,任务内两道都要绕过:

| 位置 | 今天 | 任务 IMPLEMENT |
|---|---|---|
| `review.go:348` `if !r.cfg.ReviewAfterEdit \|\| r.planMode` | 关开关或 plan mode → 整门跳过 | 任务分支不读 `ReviewAfterEdit`;`r.planMode` 进门前已被强制为 false。DESIGN 相不走实施 `reviewGate` |
| `review.go:319-321` `if r.cfg.ReviewAfterEdit { before = take... }` | 开关关则 `before` 为零值 | **进 IMPLEMENT 时无条件拍 `S_impl`**,门内无条件拍 `after`,任务分支把 `S_impl` 当作 `before` 传入(R33)。循环里不再拍 per-turn 快照。零值基线上 `changedSince` 直接返回 nil(`review.go:108-118`),bash 介导编辑会对硬门和评审同时隐身 |

开关 `review_after_edit` 只约束普通 `runEpisode`。任务内不会派两次 correctness-reviewer。

**任务分支的 `before` 就是 `S_impl`(R33)**:`reviewGate(parentCtx, initialRequest, before, round)` 在任务内传相起点,不传"这一 turn 之前"的快照。per-turn `before` 删掉,避免实现者两可,也避免 §5.4 循环与 §5.4.2 权威互相打架。门内仍拍 `after` 做当下差集。`S_impl` 落盘后续跑重载,不依赖循环局部变量。

### 5.5 落盘布局与续跑

```
.deepai/missions/<id>/
  brief.md              # 起始用户句,只读
  design.md             # 当前计划。每个 DESIGN turn 的 AgentConfig.PlanFile 钉在这里;门也只读这里
  design.locked.md      # 最近一次 pass 的计划快照
  charter.lock.json     # 有则 IMPLEMENT,无则 DESIGN
  charter.v<n>.json     # 升层归档
  implement.baseline    # IMPLEMENT 相起点快照 S_impl(path→stamp);门口 changedSince 的对照
  state.json            # 见下
  reviews.jsonl         # 每次门的裁决,审计用
```

`state.json` 是唯一相源。不把相存进 `SessionCarry` 当权威——carry 不落库,崩溃后续跑会丢相。

```
status: active | done | design_failed | handed_over | aborted
phase, design_round, escalated_design_round, implement_round, scope_round, idle_round, escalation
```

| status | 何时写入 | 续跑 |
|---|---|---|
| `active` | 任务创建与每个非终态落盘 | **只认这个**(R32) |
| `done` | 实施评审至少一次 pass | 否 |
| `design_failed` | 本相设计轮用尽仍 fail。初始相:尚未实施;升层后:worktree 上已有未通过评审的实施编辑(R37) | 否 |
| `handed_over` | 实施轮次用尽 / 空转用尽 / 升层额度已尽再触发 S1–S3 / 实施评审 fail-soft | 否 |
| `aborted` | `/mission abort` 或任务进行中 `/clear`,以及本会话再开新 `/mission <文本>` | 否 |

进入任一终态时立刻落盘,并走 `leaveMission`(R36):

```
leaveMission(status):
    persist status                       // done | design_failed | handed_over | aborted
    清掉 SessionCarry 上的章程注入       // 不得再标 locked;磁盘 lock 文件保留作审计
    r.mission = nil
    r.reviewPrev = nil
    若 status==aborted: 清掉 metadata.mission_id
```

`r.mission` 生命周期:**创建或续跑 `status==active` 时挂上**;升层只改相、不清 `r.mission`(R28 仍只换章程注入);**任一终态、`/mission abort`、`/clear` 的 abort 路径都走 `leaveMission`**。全文只有这一处置空。不清的话,任务结束后 `buildTurnInjection` 会把死章程继续注进每一个普通 turn("stay inside scope_files"),直到用户 `/clear`。`done` / `handed_over` / `design_failed` 保留 `metadata.mission_id`,以便 `/mission status` 仍能读到刚结束的那份;无文本 `/mission` 因 `status!=active` 不会续跑。

`/mission` 无文本只在 `metadata.mission_id` 指向 `status==active` 时续跑;指向终态或 metadata 已清则要求带文本。`/mission status` 打印 `status`(终态后读磁盘,不依赖 `r.mission`)。

`id` 用 `20060102-150405` + 4 位随机后缀,写入当前会话 `metadata.mission_id`。sessions 表已有 `metadata` JSON 列(`pkg/chat/session.go` 的建表语句),无需迁移。

`-c` 续接:只认**当前会话** `metadata.mission_id` 且 `status==active`(R23、R32)。命中则下一条用户输入默认续跑(UI 提示一行,**并写明:任务活跃期间任何外部写者的改动视同任务改动**,R34/R39),IMPLEMENT 相重载 `implement.baseline` 为 `S_impl`、从磁盘重挂章程进 carry。无 metadata 或 `status` 不是 `active` → 不当续跑,`/mission` 无文本时要求补任务描述。不扫描 `.deepai/missions/` 里其他会话的残留。用户发全新 `/mission <文本>` 则把本会话旧任务 `leaveMission(aborted)`。Ctrl+C 打断 turn → 与今天 episode 一样结束当轮、不审查残缺编辑;`status` 仍 `active`,`r.mission` 仍挂着,下一条输入续相。

**任务活跃期间任何外部写者的改动算进任务(R34/R39,已知行为)**:`S_impl` 是相起点,此后 worktree 上的改动不论来自主 agent、用户手改、并行的第二个 REPL、编辑器保存、还是格式化守护进程,都进 `stillDirty`。Ctrl+C 之后范围外文件被改再续跑,范围门会合成消息命令模型 revert 那份改动。这是章程执法的自然结果,会销毁外部工作。本期不豁免;§十二记录。UI 续跑提示必须带这一句。

### 5.6 合成消息(持久化,带可辨识前缀)

沿用对抗审查的选择:合成消息以 user 角色入历史,否则续接后 assistant 在回答一条不存在的消息。

```
[mission-design round 1/3] Produce an implementation plan and write_plan
it to .deepai/missions/<id>/design.md (full replace). The plan MUST
include: (1) in-scope files, including *_test.go you expect to add;
(2) Given/When/Then acceptance criteria with one observable outcome
each; (3) approach and risks. exit_plan_mode no longer waits for the
user — an independent design review runs when design.md is non-empty.
Original request:
<brief>

[mission-design-review round 1/3] An independent design review of your
plan at .deepai/missions/<id>/design.md found the following issues.
Rewrite the ENTIRE plan via write_plan (the file is replaced, not
patched). For each issue: fix it in the new plan, or state explicitly
why it is not a real problem.

1. [completeness] — <message>
   failure scenario: <scenario>
...

[mission-implement] Charter is locked. Implement it. Write tests first
(TDD). Stay inside scope_files (plus *_test.go companions). Do not
edit the charter. Acceptance:
<acceptance 摘要>
In scope:
<scope_files>

[mission-scope round 1/2] These files are outside the locked charter
scope (and are not test companions of an in-scope file). Revert them.
Repeated out-of-scope implementation files escalate to design; do not
edit the charter yourself.

- pkg/unexpected/foo.go

[mission-idle 1/2] No in-scope dirty files since the implement
baseline. Talking is not progress. Write the tests and the change the
charter requires. Two idle turns without a reviewed pass hand the
mission back to the user; do not treat silence as done.

[mission-escalate 1/1] The implementation gate concluded the charter
itself is wrong (fault_layer=design / repeated same-file fail /
repeated scope miss). Re-enter design. Locked charter has been
archived as charter.v1.json. You have 2 design-review rounds.
...
```

升层后的设计评审前缀用 `round i/2`,与初始相的 `i/3` 区分。

### 5.7 配置与手动入口

```yaml
# ~/.deepai/config.yaml
mission_on_plan: false   # 缺省关;true 时 enter_plan_mode 升级为任务
# 设计评审与实施评审共用,不另开键:
# review_token_budget:  <int, 0/缺省=150000, 负值=不限>
# review_timeout:       <int 分钟, 0/缺省=10, 与 request_timeout 同一类型>
# review_model:         <models[] 别名, 缺省空=主模型>  # 见下
```

**`review_model`(2026-09-13 补)**:两道门派发 reviewer 时传的模型别名,与编辑后审查共用同一个键。理由与"不另开预算/超时键"同源:同源盲区在设计评审、实施评审、普通编辑后审查是同一个问题。设计评审尤其吃这一条 —— 计划阶段没有编译器去否定任何一方,reviewer 与作者同模型时,作者认为成立的前提 reviewer 多半也认为成立。别名不在注册表时启动时丢弃并警示(一个拼错的别名会让设计评审每次 fail-soft,而设计侧 fail-soft 是"不实施",等于把任务停死)。

只新增一个 bool。不新增 `mission_token_budget` / `mission_timeout`——再写一套 30k / `5m` duration 字符串会:(a) 复述两个已被 `13e883f` 作废的阈值;(b) 给 `Config` 多一条与 `review_timeout int` / `request_timeout int` 不一致的解析面。

不把轮次常量做成配置——防止配成无限(对抗审查对 `maxReviewRounds` 的同一决定)。

斜杠命令:`/mission`、`/mission abort`、`/mission status`。不接受自由子命令当"给任务的额外指令"。运行中纠偏本期不做:用户只能 Ctrl+C 后 `/mission` 续跑并追加一句,或 abort。若将来 REPL 支持在 turn 中途注入消息,再接到任务循环上,不作为本期依赖。

### 5.8 UI

复用现有 `ui.Info` 与 subagent 进度块,不扩 `ReplUI`:

- DESIGN 相开始:`mission: design phase (round i/3)` 或升层后 `(escalated round i/2)`
- 设计评审 pass/fail:与实施审查同款一行
- 锁章程:`mission: charter locked, N files, M acceptance criteria`
- 范围硬门:`mission: N file(s) out of charter scope — scope-fix i/2`(写明不占实施评审轮)
- 空转催促:`mission: nothing to review — idle i/2`(写明不占实施评审轮,也不等于完成)
- 升层:`mission: escalating to design (1/1), reason=fault_layer|repeat-file|scope`
- 终态:`mission: done|design_failed|handed_over|aborted`。`design_failed` 若 `escalation>0`,必须再跟一行"实施改动未经审查"(R37);初始 DESIGN 相失败不必
- 续跑提示:相 + 轮次 + "**任务活跃期间任何外部写者的改动视同任务改动**"
- fail-soft / Ctrl+C:明确写"本次未经设计评审"或"实施改动未经审查",沿用对抗审查的警示措辞,不让未审被误认为已审。升层后的 `design_failed` 接同一句"实施改动未经审查"

---

## 六、降级与安全边界

1. **fail-soft 总原则**:设计评审链路上任何失败(超时/挂掉/schema 最终失败/池不可用)→ **不实施**,黄色警示,把计划留给用户。这与实施审查相反(实施审查 fail-soft 是放行已落地的编辑)。设计尚未改业务代码,放行等于跳过整段闭环;停住更便宜也更安全。正因为停摆代价高,预算/超时必须复用已经调过的 150k / 10m,不能用 30k / 5m 把诚实的设计评审常规打死。
2. **设计评审写树**:无 bash,只读工具。仍做审查前后 `git status` 快照;树变则丢弃裁决、按失败处理(不实施)。非 git 目录设计评审同样明示盲区;实施相非 git 不关评审,只关硬门与 S3,范围回退工具记录(R30)。
3. **空计划 / 空章程字段**:计一轮 fail,不开工。
4. **大计划**:计划文件已有 64KiB 上限(`plan.go:151`)。设计评审 prompt 超 64KiB 则截断并标注,reviewer 可用 `read_file` 自取全文(门侧 20 次工具调用够用)。
5. **可中断**:全程挂 turn ctx,Ctrl+C 结束当轮;任务状态留盘。
6. **不碰 Gateway / 已删除的 `-q`**:只做 REPL。与对抗审查 §六-6、§十一决策点 7 对齐。headless 入口若以后出现,再评估 `/mission` 的非交互等价物,不作为本期依赖。
7. **成本封顶**:两相共享 `review_token_budget` / `review_timeout` 与父回合剩余预算(现有 `RemainingTokenBudgetFromContext`)。设计门另受 `reviewMaxToolCalls=20`。

---

## 七、非本期(记录,不实现)

1. **Turn 中途纠偏**:本期任务循环是 turn 之间的门。用户 Ctrl+C 后续跑或 abort。
2. **产品经理相**:不把 brief 再经 `product-manager` 洗一遍。歧义靠 DESIGN 相的 `ask_clarification`。多一道 LLM 相而没有程序消费其 schema,违反"schema 只有代码读才配活"。
3. **强制 coder 子代理实施**:主 agent 自己改。孤立 coder 是 orchestrator 失败模式之一。
4. **多审投票 / 跨模型**:沿用对抗审查 §七,等 REVIEW_EVAL 基线。设计评审可另开一份更小的 eval(计划语料),不阻塞本期。
5. **验收命令硬门**(`go test` 退出码):有价值,但是第二条实施门。先让章程 acceptance 进 correctness-reviewer prompt;硬门作为 IMPLEMENT 相增强,避免本期同时发明两种终止条件。
6. **Headless `--mission`**:本期只做 REPL。

---

## 八、对抗式审查记录

### 第 1 轮(自审,2026-09-13)

| # | 发现 | 处置 |
|---|---|---|
| C1 | **再造编排层的诱惑**。阶段表 + 角色注册 + 黑板是 87772b6 的原样。 | **采纳**:`runMission` 两相同形 for + 枚举相。v4 收窄:不复制审查门,但 IMPLEMENT **不调用** `runEpisode`(R17)。 |
| C2 | **实施 fail-soft 放行 vs 设计 fail-soft 放行语义相反**。照抄实施审查会让设计评审一挂就直接改代码,闭环名存实亡。 | **定案**:设计失败/不可用 → 不实施;实施失败/不可用 → 放行已编辑(已落地,停不住)。§六-1 写明。 |
| C3 | **`exit_plan_mode` 人闸会破坏"自动"**。对码 `plan.go:230-271`。 | **补入 §5.2**:任务 DESIGN 内不询问、不退出,由门接管。 |
| C4 | **空 `scope_files` 的 pass 会废掉范围硬门**(放行一切或放行虚无)。 | **`isDesignPass` 代码强制非空**,不信任模型的 verdict 字符串。 |
| C5 | **章程若只活在消息里,compact 后必漂**(D1/D7)。SESSION_DESIGN 原则 3:压缩不改库,但运行时视图会丢。 | **磁盘 lock + `buildTurnInjection` 尾部注入**,消息里的合成前缀只是给人看的副本。 |
| C6 | **升层 × 设计轮乘积**。若升层重置设计计数,最坏爆炸。 | v1 写成"不重置、共用 3 轮"。**v2 推翻后半句**(见 R4):不重置初始计数,但升层后另给 2 轮。乘积仍有界(3+2)。 |
| C7 | **范围硬门过死**:实施时才发现必须改未点名文件。 | **不提供静默扩 scope**。v1 只说靠 `fault_layer`;v2 补测试伴生豁免(R3)+ S3 反复越界升层(R1)。 |
| C8 | **默认开等于拿存量用户当测试集**(对抗审查 §八-1 同构,且更贵:多一整段设计相)。 | **只经 `/mission` 进入**;`mission_on_plan` 默认关。 |
| C9 | **派 architect 子代理做设计看起来干净,实则丢上下文**。 | **主 agent + plan mode**。architect 类型保持手动 `task` 可用,不进本循环。 |
| C10 | **`DesignReviewResult` 与 `ReviewResult` 是否合并**。合并会让实施评审也"必须"填 scope_files,污染现有 gate。 | **独立 struct + 独立 namedSchema**;`Issue` 只加 omitempty 字段。 |
| C11 | **`mission_on_plan` 升级时机**:`enter_plan_mode` 发生在 turn 中,升级若立即切循环会重入 `runTurn`。 | **当 turn 结束后再升级**,下一拍进 `runMission` 的 DESIGN 门(计划多半已写出)。不拆 `runTurn`。 |
| C12 | **续跑权威放 SessionCarry 会在崩溃后丢相**。 | **`state.json` 为相的权威**;carry 只缓存章程渲染。 |

### 第 2 轮(用户评审,2026-09-13)

| # | 发现 | 处置 |
|---|---|---|
| R1 | **阻断:`fault_layer` 没有人会填**。Phase 1 只加 schema,不改 correctness-reviewer 提示词;Rule 3 把"章程选错接口"定义成越界。D4 / §5.4 升层 / C6 / 决策点 5 / §十二-2 全部落空。 | **两个修法都采纳**:Rule 3a 条件开口(仅当 user 消息带 locked charter)+ `buildReviewPrompt` 复述;代码兜底 S2(将交人工且连续同文件 fail)与 S3(范围轮用尽)。Phase 1 把提示词改动列为必做,不只加字段。 |
| R2 | **阻断:30k / 5m 是 `13e883f` 已作废的阈值**。超预算硬失败且 FinalOutput 为空;5m 被推理模型常规打穿。设计失败 → 整任务停摆,比实施审查更严重,却用了更紧的数。文档还自称与 resolver 相同,而 resolver 的 0 → 150_000。 | **取消独立预算/超时键**。设计门复用 `review_token_budget` / `review_timeout`(150k / 10m,int 分钟)。§五.2 / §五.7 / §六-1 / §六-7 改写。 |
| R3 | **阻断:测试文件会稳定踩范围硬门**。TDD 红灯先行,实施轮必写 `*_test.go`;设计无法穷举;升层又因 R1 是死的。 | **测试伴生豁免**(同实现文件或同目录的 `*_test.go` + `testdata/**`)。设计评审提示词软要求点名测试文件,不作为硬依赖。 |
| R4 | **升层大概率没有轮次可用**。不重置 3 轮 + 升层 1 次:初始花 2 轮 pass 后升回去只剩 1 轮且不容修订。决策点 4 与 5 必须一起重定。 | **升层后独立 `maxEscalatedDesignRounds=2`**。初始 3 轮不动。§九 #4/#5 改写。 |
| R5 | **范围硬门与实质评审共用 `maxReviewRounds=2`**。两次越界耗尽预算,一次 correctness 评审都没跑过。 | **`maxScopeFixRounds=2` 独立计数**。越界不递增 `reviewRound`。 |
| R6 | **`design-reviewer` `MaxToolCalls: 0` 若指出的是门侧不封顶,则与 `13e883f`/`reviewMaxToolCalls=20` 的约定冲突**。配额耗尽可恢复,墙钟不可恢复。 | **profile 保持 0**(与另外四个 reviewer 一致,直接 task 不封顶);**设计门传入 `max_tool_calls: 20`**,复用 `reviewMaxToolCalls`。 |
| R7 | **"PI 设计"在仓库里不存在**。§5.3/§5.5/§七/§十共 5 处引用未提交文档的 A3/A5/Phase。§5.3 把实现选择挂在外部编号上,评审人无法判断。`metadata` 列本身已核实存在。 | **删全部外部设计编号引用**,判断内联:注入走现有 `buildTurnInjection` 尾部;`metadata` 列已在;`steering`/`headless` 改为"本期不做,不依赖未落地能力";阶段纪律改为本文件自己的句子。 |
| R8 | **`mission_timeout: 5m` 是 duration 字符串**,与 `review_timeout` / `request_timeout` 的 int 分钟不一致。 | 随 R2 取消该键。 |

### 第 3 轮(用户评审,2026-09-13)

集中在 plan mode 生命周期:agent 每 turn 重建,plan 状态分居 REPL 与 agent 两侧。R9/R10 是同一根因的两个面。

| # | 发现 | 处置 |
|---|---|---|
| R9 | **阻断:`planFile` 每 turn 重建**。`agent.New`(`repl.go:1279`) + `enterPlanMode` → `initPlanFile()` 新开秒级路径(`react.go:330-331`,`plan.go:56,81-90`)。DESIGN 修订轮拿到空文件,上一轮计划在旧路径;门"看计划文件是否非空"没定义读哪个;agent 可能只口头改不再 `write_plan`。§5.5 两个 hedge 在现状下都落不了地。 | **`AgentConfig.PlanFile` 预设**;`enterPlanMode` 仅在 `a.planFile==""` 时 `initPlanFile`;每个 DESIGN turn 传入 `.deepai/missions/<id>/design.md`;门只读这一份。`write_plan` 全量覆盖,修订轮必须整份重发。`mission_on_plan` 升级时把旧时间戳文件复制到 `design.md`。Phase 3 列为必做。 |
| R10 | **高:设计 pass 后没人退出 plan mode**。任务内 `exit_plan_mode` 不调 `a.exitPlanMode()`(唯一退出路径);readback(`repl.go:1489`)把 true 续进 IMPLEMENT,下一 turn 只读,无法实施。 | **设计门 pass 之后、进 IMPLEMENT 之前**,`runMission` 置 `r.planMode=false` 并 `SetStatus`(发生在当轮 readback 之后)。升回 DESIGN 时再置回 true。§5.2 写明翻转责任。 |
| R11 | **高:§5.4.4 缺入口坐标**。现门第一行 `review.go:348`:`if !r.cfg.ReviewAfterEdit \|\| r.planMode { return "" }`。 | **点名这一行**。任务 IMPLEMENT 绕过 `ReviewAfterEdit`;`r.planMode` 靠 R10 的翻转变 false。第二项的正面作用:DESIGN 相天然不会误触实施门。 |
| R12 | **中:"走现成 resolver"误导**。两函数在 `pkg/commands` 未导出,装配时已写入 `ReplConfig`(`chat.go:371-372`);`pkg/chat` 只能读字段。 | **改为"复用已解析的 ReplConfig 字段"**;常量行号保留并补包路径。 |
| R13 | **低:`buildTurnInjection(react.go:532) 每 Run 算一次`不准确**。定义在 `promptbuild.go:326`;`toolexec.go:527,554` 会中途重算。 | **照实改写**。中途重算仍带章程,对注入是好消息。 |
| R14 | **低:plan mode 白名单漏 `present_file`**(`plan.go:21-23` 实际 8 个)。 | **补上**。design-reviewer 的 6 个 DefaultTools 名字已核实存在,不动。 |
| R15 | **低:Rule 3 引文截断**,原文末尾还有 `— do not report it`。 | **补全引文**。 |
| R16 | **低:"本仓库默认 glm-5.3"**。glm-5.3 来自本机 `~/.deepai/config.yaml`;仓库内置 glm 默认是 `glm-4-plus` / `glm-4-flash`(`setup.go:122`)。 | **改为"本机配置"**。 |

### 第 4 轮(用户评审,2026-09-13)

集中在"IMPLEMENT 复用 runEpisode"没接稳的接缝。

| # | 发现 | 处置 |
|---|---|---|
| R17 | **高:升层没有从门传回 `runMission` 的通道**。`runEpisode` 只有 `fixMsg` / `""` 两出口(`review.go:308-336`),S1 当场升层是第三出口。 | **`reviewGate` 返回 `gateResult{next, escalate}`**;`runEpisode` 只读 `next`;IMPLEMENT 循环在 `mission.go`,不调用 `runEpisode`。不留"改返回值或重写循环"双案。 |
| R18 | **高:快照也绑在 `ReviewAfterEdit` 上**(`review.go:319-321`)。任务内开关可为 false → 零值基线 → `changedSince` 返回 nil(`:108-118`)→ bash 介导编辑对硬门和评审隐身。 | **`runMission` IMPLEMENT 循环无条件拍 `before` 快照**。§十一-3 补守恒测试。 |
| R19 | **中:三种路径形态做集合差必然误判**。章程相对路径(LLM 原文)、`changedSince` 绝对路径、工具记录 git-canonical。 | **锁门时 `runMission` 规范化 `scope_files`**;越界与测试伴生匹配前统一为 workdir 相对。 |
| R20 | **中:首 turn 没有合成消息**,planModePrompt 不够 G/W/T,且仍教 `exit_plan_mode` 等人批准 → 第一轮设计评审结构性 fail。 | 补 `[mission-design round 1/3]` 作为任务首条 user 消息。 |
| R21 | **中:只在转换点翻转 `planMode`**,IMPLEMENT 中途 `enter_plan_mode` + readback 会重现 R10 卡死。 | **每 turn 进门前按相强制**;IMPLEMENT 设 `DisableEnterPlan`,跳过 `registerPlanTools`。 |
| R22 | **低:§5.4.4 两案并存**(`不走提前返回` / `force bool`)。 | **选定任务分支**:`reviewGate` 读 `r.mission`,不新增 `force bool`。 |
| R23 | **低:`/mission` 无文本续跑语义前后不一**(磁盘任意未完成 vs 会话 metadata)。 | **只认当前会话 `metadata.mission_id`**。 |
| R24 | **低:`/clear` 与活动任务未定义**。`clearSession` 换新 carry,磁盘任务仍未完成。 | **任务进行中 `/clear` 先 abort 再清会话**。 |

### 第 5 轮(用户评审,2026-09-13)

对着代码推演范围修复轮闭环。

| # | 发现 | 处置 |
|---|---|---|
| R25 | **高:revert 后工具记录仍钉住越界文件**。`EditedFiles()` 只增不减(`session_carry.go:115-131`);快照侧 revert 后自然退出,记录侧不会。硬门用累积集做差 → 第二次仍越界 → S3 误升层。 | **越界权威改为 `after.changedSince(S_impl)`**,不信任累积记录,不改 carry API。`S_impl` 进 IMPLEMENT 时拍并落盘。非 git 关硬门且禁 S3。单测:revert 后退出越界集;集成:revert 后 S3 不触发。 |
| R26 | **中:进 IMPLEMENT 的第一拍没有输入消息**。历史停在设计对话。 | 补 `[mission-implement]`(章程已锁、scope/acceptance 摘要、TDD、不越界、不改章程)。 |
| R27 | **中:归因与 `reviewPrev` 生命周期未定义**。`runEpisode` 入口的 `ClearEditedFiles` / `reviewPrev=nil`(`review.go:313-314`)不再被调用。 | **进 IMPLEMENT 与升层离相时由 `mission.go` 复制这两步**。伪代码与 §十一写明。 |
| R28 | **低:升层后 carry 章程注入未清**,设计修订 turn 仍宣告 locked charter,与 escalate 消息打架。 | 升层动作清掉(或改挂归档+原因,不得标 locked);新锁后再挂回。 |
| R29 | **低:"越界检测和修复都是零 LLM 成本"不实**。修复轮是完整 LLM turn。 | 改为"检测不派 reviewer;修复轮不派实施评审"。独立计数论据不变。 |

### 第 6 轮(用户评审,2026-09-13)

对着 v5 新换的归因权威推演各分支。`S_impl` 方案本身成立,但拆掉了非 git 降级和"空 scope = 结束"在任务里的语义。

| # | 发现 | 处置 |
|---|---|---|
| R30 | **高:非 git 下实施评审跟着硬门一起死**。评审范围改成 `stillDirty ∩ (allow ∪ 伴生)` 后,零值 `S_impl` 上 `changedSince` 恒 nil → reviewScope 恒空。今天的门靠工具记录活着(`review.go:353-355`)。 | **git 可用时 `stillDirty` 当权威;非 git 只关硬门与 S3,评审范围回退 `resolveWorktreePaths(EditedFiles())`**。 |
| R31 | **高:stillDirty 空被当成完成**。门对 `len(scope)==0` 返回 `""`(`review.go:361-363`);循环把 `next==""` 当结束。纯文本 turn 或 revert 光越界文件 → 零实现、零审查、静默 DONE。 | **`gateResult.passed` 区分 pass 与无可审**。空 scope 走 `[mission-idle]`,`maxIdleRounds=2`;只有至少一次 pass 才 `status=done`。§十一:纯文本 turn 不结束任务。 |
| R32 | **中:终态没建模**。文档依赖"未结束"但 `state.json` 只有 phase/rounds。 | **`status: active\|done\|design_failed\|handed_over\|aborted`**。续跑只认 `active`;进入终态立刻落盘;`/mission status` 打印它。 |
| R33 | **中:任务分支 `before` 语义未定**,§5.4 仍每 turn 拍 `before`,与 §5.4.2 的 `S_impl` 权威打架。R27 清空理由已过时。 | **任务分支 `before` 即传 `S_impl`,删循环里的 per-turn 快照**。清空仍做,理由改为卫生 + 非 git 回退。 |
| R34 | **低:续跑期间用户手改会被算到任务头上**,范围门会命令 revert。 | **已知行为,本期不豁免**。§5.5/§十二写明;续跑 UI 带一句。 |
| R35 | **低:"stamp 回到与 S_impl 一致"达不到**。`fileStamp` 含 mtime(`review.go:54-57`),revert 必改 mtime。 | **删掉该分支**。唯一清除路径是 porcelain 消失。常见 `git checkout --` / 删除不受影响。 |

### 第 7 轮(用户评审,2026-09-13)

无阻断/高级别。收尾:对称性遗漏与公式冗余。

| # | 发现 | 处置 |
|---|---|---|
| R36 | **中:终态后章程注入泄漏进普通对话**。R28 只在升层清注入;`done` / `handed_over` / `design_failed` / `aborted` 后 carry 仍挂 locked charter。`r.mission` 何时置空全文没写。 | **`leaveMission`**:任一终态清章程注入、`r.mission=nil`、`reviewPrev=nil`。创建/续跑 active 时挂上;升层只改相。§十一:`status=done` 后下一普通 turn 注入无章程段,`reviewGate` 走普通守卫。 |
| R37 | **低:`design_failed` 写成"不实施"**。升层后设计轮用尽时,worktree 已有未审实施。 | 终态名不变。描述与 UI 区分:初始相未实施;升层后接"实施改动未经审查"。 |
| R38 | **低:`reviewScope = stillDirty ∩ (allow ∪ 伴生)` 冗余**,且把 `.deepai/**` / `go.sum` / `*.lock` 喂给 reviewer。 | **`reviewScope = stillDirty ∩ (scope_files ∪ 测试伴生)`**。豁免只免越界,不进评审。 |
| R39 | **低:R34 只写了"用户手改"**。并行 REPL、编辑器、格式化守护同样进 `stillDirty`。 | §5.5 / §十二-9 / 续跑提示改为"任何外部写者"。无新机制。 |

---

## 九、决策点

| # | 决策点 | 决议 | 依据 |
|---|---|---|---|
| 1 | 默认入口 | **仅 `/mission`**,`mission_on_plan` 关 | C8 |
| 2 | 谁设计 | **主 agent + plan mode** | C9;第 2 轮维持 |
| 3 | 设计评审失败时 | **不实施** | C2 |
| 4 | 设计轮上限 | **初始 3;升层后另计 2** | R4,与 #5 一起重定 |
| 5 | 升层上限 | **1 次;不重置初始计数;升层相用独立额度** | R4 |
| 6 | 范围越界 | **代码硬门,不派 reviewer** | D2;第 2 轮维持 |
| 7 | 越界能否扩 scope | **否**。测试伴生豁免不是扩 scope;反复越界走 S3 升层 | C7、R3 |
| 8 | 章程注入 | **`buildTurnInjection` 尾部 + 磁盘 lock;仅 `r.mission!=nil` 时挂上,终态 `leaveMission` 清掉** | C5、R7、R36 |
| 9 | 与 `review_after_edit` | **任务内强制审;进相拍 `S_impl`、门内拍 `after`;开关只管普通 `runEpisode`** | R18、R33、§5.4.4 |
| 10 | 计划格式 | **自由 markdown + 评审者产出 scope/acceptance**。不强制 YAML frontmatter | 少一条解析面;章程字段由程序消费的 schema 保证 |
| 11 | 设计评审预算/超时 | **复用已解析的 ReplConfig 字段**(150k / 10m,int 分钟) | R2、R8、R12 |
| 12 | 升层信号 | **提示词开口 + S2/S3;`gateResult.escalate` 传回 `runMission`** | R1、R17 |
| 13 | DESIGN 计划路径 | **`AgentConfig.PlanFile` 钉 `design.md`;门只读这一份** | R9 |
| 14 | 谁维持 plan mode | **每 turn 进门前按相强制;IMPLEMENT 禁用 `enter_plan_mode`** | R10、R21 |
| 15 | IMPLEMENT 循环 | **`mission.go` 同形 for,不调用 `runEpisode`** | R17 |
| 16 | `/mission` 无文本 | **只认本会话 `metadata.mission_id`** | R23 |
| 17 | `/clear` | **活动任务先 abort 再清会话** | R24 |
| 18 | 越界权威 | **git 可用时 `changedSince(S_impl)`;非 git 关硬门/S3,评审回退 `EditedFiles`** | R25、R30 |
| 19 | IMPLEMENT 首条 | **`[mission-implement]` 合成消息** | R26 |
| 20 | IMPLEMENT 完成 | **仅 correctness-reviewer pass → `done`;空 scope 是 idle 不是完成** | R31 |
| 21 | 任务终态 | **`active\|done\|design_failed\|handed_over\|aborted`;续跑只认 `active`;离相走 `leaveMission`** | R32、R36 |
| 22 | 任务分支 `before` | **传 `S_impl`,不拍 per-turn 快照** | R33 |

#2 与 #6 第 2 轮明确维持。#8/#21 第 7 轮补 `leaveMission`。#20–#22 第 6 轮新增。

---

## 十、实施计划

| 阶段 | 内容 | 涉及 |
|---|---|---|
| Phase 1 | `DesignReviewResult` + `Issue.Area`/`FaultLayer` + `design-reviewer` + `namedSchemas["design_review"]`;**correctness-reviewer Rule 3a**(有章程才开口);现有四 reviewer 的 strict 回归;无章程时 Rule 3 行为不变的单测 | `pkg/agent/output.go`、`types_config.go`、correctness reviewer 测试 |
| Phase 2 | 任务落盘(brief/state/charter/reviews.jsonl)+ `state.status` 五态 + `metadata.mission_id`;`leaveMission` 清注入并置 `r.mission=nil`;`/mission` `/mission abort` `/mission status`;续跑只认 `status==active` | `pkg/chat/mission.go`、`session.go`、`slashcommands.go`、`session_carry.go` |
| Phase 3 | `AgentConfig.PlanFile` / `DisableEnterPlan`;`enterPlanMode` 有预设则跳过 `initPlanFile`;`runMission` DESIGN 相每 turn 进门前强制 plan mode、传入 `design.md`、首条 `[mission-design 1/3]`;`exit_plan_mode` 任务语义;设计门只读 `design.md`;锁章程时规范化 `scope_files`;`buildTurnInjection` 尾部注入 | `types.go`、`plan.go`、`react.go`、`mission.go`、`session_carry.go`、`promptbuild.go` |
| Phase 4 | `reviewGate` → `gateResult{next,escalate,passed}`;IMPLEMENT 同形 for:**不调用 `runEpisode`**、进相清空 EditedFiles/`reviewPrev`、拍 `S_impl` 作 `before`、首条 `[mission-implement]`、git 时越界用 `changedSince(S_impl)`、`reviewScope` 不含豁免生成物、非 git 评审回退工具记录、空 scope 走 idle 而非 done、终态/升层清 charter 注入 | `review.go`、`mission.go`、`session_carry.go` |
| Phase 5 | `mission_on_plan`、`/clear` abort、metadata 续跑、UI、文档;单测 + mock 集成(见 §十一) | `setup.go`、`repl.go`、本文件 |

每阶段完成后停在未提交状态等待评审,通过后再按本节阶段号提交。

---

## 十一、测试策略

1. **单元**:`isDesignPass`;范围硬门(路径形态统一;`stillDirty = changedSince(S_impl)`;revert 回 porcelain-clean 则退出越界集,`EditedFiles` 仍含该路径也不算越界;**不**把 stamp 相等当清除路径);`reviewScope` 含范围内实现与测试伴生、**不含** `.deepai/**` / `go.sum` / `*.lock`;非 git 时硬门/S3 关、`reviewScope == resolveWorktreePaths(EditedFiles())`;`scopeRound`/`reviewRound`/`idleRound` 分计;`gateResult.passed` 与 `escalate` 真值表;`leaveMission` 后 `r.mission==nil` 且 carry 无章程;进 IMPLEMENT / 升层时 `EditedFiles` 与 `reviewPrev` 被清空;`enterPlanMode` 预设路径;`DisableEnterPlan`;`runEpisode` 在 `escalate==""` 时与改前一致。
2. **集成**:历史含 `[mission-design 1/3]` 与进相后的 `[mission-implement]`;**纯文本 IMPLEMENT turn 不结束任务**(走 idle,`status` 仍 `active`);越界 → revert → 再进门越界集为空且 **S3 不触发**,若此时无范围内改动也不标 `done`;S1 当场升层;升层后的 DESIGN turn 注入不再含 locked charter;**`status=done` 后下一个普通 turn 的注入不含章程段,`reviewGate` 走普通守卫**;升层后设计轮用尽的 `design_failed` 带"实施改动未经审查";两连续 DESIGN turn 写入同一 `design.md`;普通 `runEpisode` 在 `ReviewAfterEdit=false` 时仍不拍快照、不审。
3. **守恒**:`review_after_edit=false` 的普通编辑轮仍不进实施门、仍不拍快照;任务 IMPLEMENT 即使该开关为 false 也拍 `S_impl`/`after` 且进门;DESIGN 相不走实施 `reviewGate`;`/mission` 无文本不拾取其他会话的磁盘任务、也不续跑 `status!=active`;`/clear` 使 `metadata.mission_id` 清空且 `status=aborted`,`r.mission==nil`。
4. **评测**(非本期):设计评审可另做小型计划语料;不阻塞翻默认(反正默认不开任务)。

---

## 十二、风险

1. **设计评审比代码评审软**。没有编译/测试作硬信号,假阳性与假阴性都会更高。缓解:规则 2 要 scenario;规则 4 构造不出必须 pass;`isDesignPass` 要非空章程字段;轮次上限后交人而不是硬开干。
2. **范围硬门误伤实现文件**(测试伴生已排除)。计划漏写一个必改的非测试文件。缓解:S3 两次越界后升层;不在本期做自动扩 scope。
3. **S2 把难修的实现 bug 升成设计**。缓解:只在将交人工时触发,且整次任务只升一层;升层后 2 轮设计若确认章程没问题,会带着同一份 brief 再锁一次,实施相从头计轮次。
4. **章程注入破坏 prompt cache 前缀**。memory 注入已有此问题(ARCHITECTURE_REVIEW §2.2)。缓解:章程段放在与 memory 相同的尾部位置,前缀(系统提示主体)不动。
5. **`mission_on_plan` 与用户以为的"只是想看一眼计划"冲突**。故默认关;显式 `/mission` 才自动闭环。
6. **Phase 3 碰 `plan.go` 的 exit 路径与 `PlanFile` API**。回归必须锁住:无 `PlanFile` 时仍 `initPlanFile`;非任务 `exit_plan_mode` 仍三选一询问。漏测任一则所有 `/plan` 用户受损。
7. **设计失败即停摆**。预算/超时复用已解析的 150k / 10m,避免用已作废阈值把诚实评审打死之后再走停摆路径。
8. **修订轮只口头改、不 `write_plan`**。门读到的仍是旧 `design.md`,可能误 pass 或反复催促。缓解:修订消息强制要求整份重写;门比较内容 hash 是否变化不是本期必做(空文件才催促;非空就审当前磁盘内容)。
9. **任务活跃期间任何外部写者的改动被章程执法 revert(R34/R39)**。`S_impl` 之后主 agent、用户手改、并行 REPL、编辑器、格式化守护进程的 worktree 改动都进 `stillDirty`。已知行为,本期不豁免;续跑 UI 必须提示。豁免是否做留给以后,不在本期开叉。
10. **相起点已 dirty 的文件 revert 后因 mtime 洗不掉(R35)**。与今天 episode 归因同一残差。常见新建/新改再 `git checkout --` 不受影响。不在本期改 `fileStamp`。

---

## 十三、修订日志

| 版本 | 日期 | 变更 |
|---|---|---|
| v1 | 2026-09-13 | 初稿。对照已落地的 `runEpisode`、plan mode、87772b6 教训,给出任务循环、章程锁、两层有界评审、升层与实施计划。§八 12 条自审已并入正文。 |
| v2 | 2026-09-13 | 响应第 2 轮评审 R1–R8:correctness-reviewer Rule 3a + S2/S3 代码升层兜底;设计门复用 150k/10m 与 `reviewMaxToolCalls=20`,取消独立预算/超时键;测试伴生豁免;范围轮与实施评审轮分计;升层后独立 2 轮设计;删未入库外部设计编号,判断内联。决策点 #4/#5 一起重定,其余第 2 轮维持。 |
| v3 | 2026-09-13 | 响应第 3 轮评审 R9–R16。阻断 R9:`AgentConfig.PlanFile` 钉死 `design.md`,`enterPlanMode` 有预设则不新建时间戳文件。高 R10:设计门 pass 后由 `runMission` 置 `r.planMode=false`。高 R11:点名 `review.go:348` 两个条件。中/低 R12–R16:ReplConfig 字段、`buildTurnInjection` 中途重算、`present_file`、Rule 3 引文、本机 glm 配置。新增决策点 #13/#14。 |
| v4 | 2026-09-13 | 响应第 4 轮评审 R17–R24。`reviewGate` 改 `gateResult`;IMPLEMENT 循环在 `mission.go`、不调用 `runEpisode`;无条件拍快照;锁门规范化 `scope_files` 后再做范围差;首条 `[mission-design 1/3]`;每 turn 按相强制 plan mode 且 IMPLEMENT 禁用 `enter_plan_mode`;续跑只认本会话 metadata;`/clear` 先 abort。 |
| v5 | 2026-09-13 | 响应第 5 轮评审 R25–R29。越界权威改为相对 `S_impl` 的当下差集,revert 后退出越界集、S3 不误触发;进 IMPLEMENT 补 `[mission-implement]`;进相/离相清空 `EditedFiles` 与 `reviewPrev`;升层清 charter 注入;修正"修复零 LLM 成本"的表述。 |
| v6 | 2026-09-13 | 响应第 6 轮评审 R30–R35。非 git 评审范围回退工具记录;空 scope 走 idle 而非 done,`gateResult.passed` 才结束;五态 `status`;任务分支 `before=S_impl`;续跑手改已知行为;删"stamp 一致即退出越界集"。 |
| v7 | 2026-09-13 | 响应第 7 轮评审 R36–R39。任一终态走 `leaveMission`(清章程注入、`r.mission=nil`);`design_failed` 区分未实施与升层后未审实施;`reviewScope` 不含豁免生成物;外部写者推广 R34。 |

---

## 十四、实施记录(2026-09-13)

Phase 1–5 全部落地,`go build ./...` 与 `go test ./...` 通过(`pkg/mcp` 的 `TestLoad_RegistersToolsAndReports` / `TestLoadWithServers_ConnectsExtra` 是既有环境失败:t.TempDir 清理只读的 module cache,非本次回归)。

### 落点

| Phase | 文件 |
|---|---|
| 1 | `pkg/agent/output.go`(`DesignReviewResult`、`Issue.Area`/`FaultLayer`)、`pkg/agent/types_config.go`(`design-reviewer` 档案 + `designReviewerSystemPrompt` + `namedSchemas["design_review"]` + correctness Rule 3a)、`pkg/agent/design_reviewer_test.go` |
| 2 | `pkg/chat/mission.go`(目录布局、五态 `status`、章程、`implement.baseline`、`normalizeScopeFiles`)、`pkg/chat/mission_command.go`(`/mission`、`leaveMission`、`attachSessionMission`)、`pkg/chat/mission_test.go` |
| 3 | `pkg/agent/types.go`(`PlanFile` / `DisableEnterPlan` / `DeferPlanApproval`)、`plan.go`、`react.go`、`promptbuild.go`(章程尾部注入)、`session_carry.go`(`SetMissionCharter`)、`pkg/chat/mission_design.go`、`mission_messages.go`、`mission_loop.go` |
| 4 | `pkg/chat/review.go`(`gateResult`、章程块)、`pkg/chat/mission_implement.go`(范围硬门、S1–S3、idle) |
| 5 | `pkg/chat/repl.go`(按相强制 plan mode、`/clear` abort、启动续挂、`mission_on_plan` 升级)、`pkg/commands/setup.go` / `chat.go` |

### 对本文的两处偏离

1. **`design-reviewer` 的 `MaxToolCalls` 用 `defaultReviewerMaxToolCalls`(20),不是 0。** §5.2 写 `MaxToolCalls: 0`,理由是"与另外四个 reviewer profile 相同"。但仓库现状已经变了:`TestReviewerProfiles_CarryAToolCallCap` 要求四个 reviewer 档案都带 20 —— 交互池现在有墙钟,不封顶的 reviewer 遇到墙钟交不出东西。"与另外四个一致"今天的取值就是 20。门侧仍单独传 `reviewMaxToolCalls`,分工不变。

2. **升层回设计后不重拍 `S_impl`,保留原基线。** §5.4 的伪码在"进 IMPLEMENT 相时"拍快照,字面上包括升层后的第二次进相。但重拍会让第一次实施相已经落地的编辑落进新基线 → 对第二次实施评审隐身 → 任务可能以 `done` 收尾而树上留着从未被评审的改动,这正是 §9 #20 / R31 要防的那件事。保留原基线后:新章程覆盖的旧编辑会被评审,不覆盖的会被范围门要求 revert。代码注释写明了这一取舍(`escalateToDesign`)。

### 实现中补的小决定(文档未定义)

- **活动任务拥有下一条输入**(§5.5 原话"下一条用户输入默认续跑",实现补齐):`r.mission != nil` 时,非 slash 输入与 `-c` 的 auto-continue 都走 `runMission`,用户那句话成为该相下一 turn 的输入(这也是本期唯一的"运行中纠偏"通道);`missionReviewGate` **只由 `runImplementPhase` 调用**,不再挂在 `reviewGate` 上。第 1 轮 PR 评审发现的高优先级缺陷:经 `runEpisode` 进这道门时,升层 / `passed` / 终态会被算出来然后丢掉(`runEpisode` 只读 `next`),普通闲聊还会烧掉任务的 idle 额度。
- **设计评审 fail-soft 的终态**:§六-1 只说"不实施",没给 `status`。实现写 `handed_over`(计划留盘、明示未评审),不写 `design_failed` —— 后者的语义是"轮次用尽仍 fail"。但**打断设计评审子代理**不算 fail-soft:裁决什么都没说,任务保持 `active`,下一条消息续跑。
- **两道门都要求显式 `verdict=="pass"`**。§5.2 的 `isDesignPass` 沿用 `isPassVerdict`(issues 为空即 pass);但 Strict schema 下"fail + 空 issues"是完全合法的形状,在实施侧会把未修复的改动写成 `done`,在设计侧会把被否的计划锁成章程。两个 reviewer 提示词都承诺输出字面量 `"pass"`,代价只是多一轮修订。
- **升层额度用尽时 S1 与 S3 同一出口**:reviewer 刚说过问题不在代码,再花修复轮就是花在错误的层上,直接交人工。
- **中断的设计 turn 不计轮次**:`designRound` 移到 `turnErr` 检查之后 —— 与实施相(`ImplementRound` 只在发 fix 时加)一致。
- **`/new` 摘掉 `r.mission` 但不改磁盘状态**(任务留在旧会话的 metadata 里);**`/undo` 把章程重挂到新 carry 上**(整份 carry 被换掉,而任务还活着)。
- **`DeferPlanApproval` 下 `exit_plan_mode` 的 inline `plan` 参数会被写进钉死的 `PlanFile`**:门只认那个文件,否则模型以为提交了、门看到的是空文件,白烧一轮。文件已有内容时不覆盖。
- **`leaveMission` 同时复位 plan mode 相关的 per-turn 覆盖**(`planMode`/`planFile`/`DisableEnterPlan`/`DeferPlanApproval`)。否则在设计相结束的任务会把用户留在只读模式里,而唯一的出口(`exit_plan_mode`)还被指向一个已经不存在的门。
- **非 git 警示从相起点移到门里**:降级真正生效的地方是门,这样中途失去 git(被删、index 锁死)也会被说出来。
- **路径归一化 `workdirRel` 先规范形后原形**:计划点名的新文件、以及连同目录一起被删除的文件都过不了 `EvalSymlinks`,在 macOS(`/var` → `/private/var`)上会被误判成"树外"。
- **`design_failed` 的首句按是否升层分两种写法**(R37):升层后先说"上一份章程下的改动仍在树上且从未通过评审",不再先说"什么都没实施"。
