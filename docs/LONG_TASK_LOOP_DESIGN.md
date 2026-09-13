# 长任务闭环(Mission Loop)设计 — 设计 → 评审 → 实施 → 评审

> 状态:**待评审**,未提交。v1 含一轮第一性原理自审,发现与处置见 §八。
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

- 任务循环是 `runEpisode` **外侧**的一个有界状态机(`pkg/chat/mission.go` 的 `runMission`),`runTurn` 本身不改;
- 实施评审**直接调用**现有 `reviewGate` / `dispatchReview`,不复制一份;
- 设计评审复用现有 subagent 池 + `task` 执行链路 + `ParseOutput`;
- 每个设计修订轮、每个实施轮都是一个普通 turn,持久化/记忆调度/事件渲染全部走现成路径。

---

## 二、问题定义:长任务如何"跑偏"

跑偏不是一种感觉,是四类可观测失败。本设计的全部机制都对着这四类:

| # | 跑偏形态 | 典型症状 | 本设计的对策 |
|---|---|---|---|
| D1 | **目标漂移** | 压缩/多轮修订之后,agent 开始做"对话里最近提到的事",而不是用户最初要的事 | 章程锁定原始 brief,每轮与每次评审只对照章程,不对照最新闲聊 |
| D2 | **范围膨胀** | 修一个函数变成顺手重构邻包 | 章程锁 `scope_files`;实施门用代码做硬范围检查,越界不耗评审 token |
| D3 | **计划-实现错位** | 设计写了 A,代码做了 B,两边各自自洽 | 实施评审 prompt 带章程的 acceptance;未满足的准则是结构化 issue,不是散文 |
| D4 | **设计本身错了还继续堆代码** | 实施评审反复打回同一处,根因在接口选错 | 实施裁决可标 `fault_layer=design`,有界升回设计阶段,而不是无限修代码 |

次要但必须处理的失败:

| # | 形态 | 对策 |
|---|---|---|
| D5 | 自证偏差 | 设计评审与实施评审都是独立子代理,看不到实现者推理(沿用对抗审查的信息隔离) |
| D6 | 审查空转 | 每层硬编码轮数上限;升层也有上限 |
| D7 | 压缩失忆 | 章程在磁盘 + `SessionCarry` 注入,不把"唯一副本"放在可被 compact 的消息里 |
| D8 | 用户逐步批准把"自动"打断 | 任务循环内 `exit_plan_mode` 不再弹 Yes/Revise/Cancel,改由设计评审门接管 |

---

## 三、现状与差距

| 能力 | 现状 | 差距 |
|---|---|---|
| 只读设计阶段 | plan mode:`enter_plan_mode` / `write_plan` / `exit_plan_mode`;计划落 `.deepai/plans/<ts>.md`(`plan.go`) | ⚠️ `exit_plan_mode` 在有 `UserInteraction` 时**阻塞等人批准**(`plan.go:230-271`);非交互才自动通过。自动闭环会被这道人闸打断 |
| 设计产出角色 | `architect` / `product-manager` 子代理存在,无 Strict schema(M5-4 已撤非程序消费的 schema) | ⚠️ 角色在,但**没有代码读取**它们的产出做门禁。types_config.go:388-396 写明:schema 只有被程序消费才配活 |
| 设计评审 | `arch-reviewer` 审的是**代码 diff 的架构**,不是计划文档 | ❌ 缺"对照 brief 审一份设计稿"的 reviewer |
| 实施→评审→修复 | `runEpisode` + `correctness-reviewer`,`maxReviewRounds=2`,fail-soft,快照归因 | ✅ 直接复用,作为任务循环的 IMPLEMENT 相 |
| 范围锁定 | 实施审查范围 = 本轮归因文件,对照的是 episode 起始用户句 | ❌ 不对照设计范围;越界没有硬门 |
| 升回设计 | 实施审查 2 轮后交人工,不会重开设计 | ❌ 缺 `fault_layer` |
| 长任务状态 | 会话线性消息 + 易失的 `SessionCarry` | ❌ 中断后续跑没有"停在设计还是实施"的落盘状态 |
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
   │  turn: PlanMode=true(只读+write_plan) │
   │  门: design-reviewer 审计划文档     │
   │  fail 且未达上限 → 合成修订消息,仍 DESIGN
   │  fail 且达上限 → 呈给用户,任务结束(不实施)
   │  pass → 写 charter.lock.json,进 IMPLEMENT
   └────────────┬─────────────────────┘
                │
                ▼
   ┌──────────── IMPLEMENT 相 ─────────┐
   │  直接调用现有 reviewGate / runEpisode 语义
   │  额外:章程注入 + 范围硬门 + fault_layer
   │  实施评审 fail → 修复轮(现有 maxReviewRounds)
   │  fault_layer=design 且升层次数未尽
   │       → 回 DESIGN(章程降为草案,带升级原因)
   │  pass → 任务结束
   └──────────────────────────────────┘
```

两层循环都是"turn 结束后面的门",和现在的 `runEpisode` 同构。评审次数是**门的循环次数**,不是编排引擎的阶段表。

```
DESIGN ──审──▶ DESIGN(修订) ──审──▶ IMPLEMENT
                                      │
                                      ▼
                                 实施评审
                                      │
                       ┌── pass ──────┴── fail ──┐
                       ▼                         ▼
                     DONE              fault_layer=design?
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
| 显式 | `/mission [文本]`。有文本 = 新任务;无文本且磁盘上有未完成任务 = 续跑 | 唯一默认入口 |
| 升级 | 配置 `mission_on_plan: true` 时,普通 episode 里主 agent 调用了 `enter_plan_mode`,该 turn 结束后把**剩余工作**升级为任务(DESIGN 已在进行) | **关** |
| 普通 | 不进 `runMission`,仍走今天的 `runEpisode` | 缺省路径,行为零变化 |

不按启发式(长度、"复杂"、模型自报)自动开任务——误开的成本是一次设计评审 + 可能的实施审查,和 `review_after_edit` 首发默认关是同一理由。

`/mission abort` 结束当前任务(不回滚已落地的编辑,与实施审查"第 3 次 fail 不回滚"一致)。`/mission status` 打印相、轮次、章程摘要。

### 5.2 DESIGN 相:复用 plan mode,接管批准权

**谁来设计**:主 agent,强制 `PlanMode=true`。不派 `architect` 子代理。理由:主 agent 持有用户澄清与会话;子代理从零起跑且无记忆(`subagent.go` 不传 `MemoryService`);orchestrator 把活丢给孤立 coder 的路已经失败过。

**工具集**:现状 plan mode 白名单(只读文件工具 + `write_plan` + `exit_plan_mode` + `ask_clarification`),**仍然没有 `task`**,防止借 coder 绕过只读。

**`exit_plan_mode` 在任务内的语义**(对码:`plan.go:224-282` 在 `ui != nil` 时 `AskQuestion` 三选一):

```
若 ctx 带 mission 且相 == DESIGN:
    不询问用户、不退出 plan mode
    返回:"计划已提交独立设计评审。继续留在 plan mode。"
否则:
    保持今天的行为(交互批准 / 非交互自动通过)
```

门不看"是否调用了 exit_plan_mode",看**计划文件是否非空**。agent 只 `write_plan` 不 exit 也一样进门;空计划则合成催促消息、计一轮、仍 DESIGN。

**设计评审者拿到什么**(信息隔离,镜像实施审查 §4.4):

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
    MaxToolCalls: 0,
    Temperature:  0.2,
    OutputSchema: FromStruct[DesignReviewResult](WithStrict(true), WithMaxRetries(1)),
}
```

- **不给 bash**:设计稿没有可编译的硬信号;给 bash 只会诱使 reviewer 去"顺便验证"而碰树。只读工具够核对计划是否引用了真实标识符。
- **不复用 `arch-reviewer`**:那条提示词的 scope 是"THIS change"(代码 diff),和"对照 brief 审计划"不是同一份考纲。

提示词核心约束(与 correctness-reviewer 同构,只换对象):

```
You are an independent adversarial design reviewer.
You receive the original brief and a plan document. You do NOT see
the author's reasoning.

Pass only if all of the following hold:
1. The plan solves the brief — not a nearby or larger problem.
2. In-scope files are named as they exist (or will exist) in the repo;
   out-of-scope is explicit.
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
```

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

**有界性**:`maxDesignRounds = 3`(常量)。第 3 次仍 fail → 把 issues 呈给用户,**不自动开工**。1 轮不够修订后重审;4 轮在模型卡住时明显烧钱。设计比改一行代码更需要一次来回,所以比实施的 2 略宽。

### 5.3 章程锁定:防跑偏的唯一源

设计门 pass 时写入 `.deepai/missions/<id>/charter.lock.json`,此后 IMPLEMENT 相只认这一份:

```go
type Charter struct {
    MissionID    string   `json:"mission_id"`
    Brief        string   `json:"brief"`         // 任务起始句,永不改
    DesignHash   string   `json:"design_hash"`   // sha256(design.md)
    ScopeFiles   []string `json:"scope_files"`   // 相对 workdir,已规范化
    Acceptance   []string `json:"acceptance"`
    LockedAt     time.Time `json:"locked_at"`
    Escalation   int      `json:"escalation"`    // 已从实施升回设计的次数
}
```

同时复制 `design.md` 快照为 `design.locked.md`(审查与 `-c` 续跑对照用)。磁盘上的 `write_plan` 文件可以继续改,但**门与注入读的是 locked 副本**,直到下一次设计 pass 换锁。

**注入路径**(D7):不把章程只追加成一条 user 消息——compact/aging 会摘要或丢掉它。走与 memory 相同的 per-turn 注入:

- `SessionCarry` 增 `charter *Charter`(或渲染好的只读文本 + hash);
- `buildTurnInjection` 增加具名段 `mission-charter`(PI 设计 A5 的分段重构若未落地,先用现有尾部注入,段名写进本设计,避免第二次拼接);
- 实施评审 prompt 的"原始需求"从 `initialRequest` **改为章程**:brief + acceptance + scope_files + design.locked.md。`runEpisode` 的 `initialRequest` 参数在任务内传 brief,`buildReviewPrompt` 增加可选 charter 块。

压缩之后模型仍能看见章程,因为注入发生在每次 LLM 请求前,不依赖消息列表里那份。

### 5.4 IMPLEMENT 相:复用 `reviewGate`,加两道钉

实施相**不写第二套 episode**。`runMission` 在 DESIGN pass 之后调用与 `runEpisode` 相同的"turn → reviewGate → 可能的修复轮"循环。改动收在三处,全部是加法:

1. **章程注入**(§5.3)。
2. **范围硬门**(D2),放在 `dispatchReview` 之前,零 LLM 成本:

```
edited = 本轮归因文件(已有 editedFiles ∪ 快照差集)
allow  = charter.scope_files ∪ 范围豁免
越界   = edited \ allow
若越界非空:
    不派 reviewer
    合成 scope-violation issues(每条含 scenario:"计划未点名此文件")
    计入实施评审 fail,走修复轮
```

范围豁免(硬编码,不配置):`.deepai/**`、`go.sum`、`go.work.sum`、`*.lock`。豁免的是生成物与任务元数据,不是"agent 觉得相关的邻文件"。

**发现必须改章程外的文件**时,正道是实施评审给 `fault_layer=design`(或主 agent 在修复轮里明确反驳并等待升层),不是静默扩 scope。代码侧不提供 `scope_files +=` API。

3. **`fault_layer` 升层**(D4):

`reviewGate` 在 `!isPassVerdict` 时扫描 issues:任意一条 `FaultLayer=="design"`(大小写不敏感)且 `escalation < maxDesignEscalations` → 不合成修复消息,返回升层信号。`runMission` 删 `charter.lock.json`(保留 `charter.vN.json` 归档)、`planMode=true`、合成设计修订消息(含实施 issues 与"原章程何处走不通")、`escalation++`。

`maxDesignEscalations = 1`(常量)。实施阶段发现设计错了,给一次重做设计的机会;再错交人工。升层不重置 `maxDesignRounds` 计数器——升回后的设计评审仍最多 3 轮,避免"升层 × 设计轮"乘积爆炸。

实施评审轮次保持现有 `maxReviewRounds = 2`,不另开配置。

**与 `review_after_edit` 的关系**:任务循环的 IMPLEMENT 相**强制走审查门**,忽略 `review_after_edit` 开关。开关只约束普通 `runEpisode`。任务内不会派两次 correctness-reviewer。

### 5.5 落盘布局与续跑

```
.deepai/missions/<id>/
  brief.md              # 起始用户句,只读
  design.md             # 当前计划(可与 plan.go 的 write_plan 文件是同一路径,或任务创建时把 planFile 指过来)
  design.locked.md      # 最近一次 pass 的计划快照
  charter.lock.json     # 有则 IMPLEMENT,无则 DESIGN
  charter.v<n>.json     # 升层归档
  state.json            # {phase, design_round, implement_round, escalation}
  reviews.jsonl         # 每次门的裁决,审计用
```

`id` 用 `20060102-150405` + 4 位随机后缀,写入当前会话 `metadata.mission_id`(sessions 表已有 `metadata` JSON 列,SESSION_DESIGN / PI 设计 A3 已依赖它,无需迁移)。

`-c` 续接:REPL 启动时若 metadata 指向未结束任务,下一条用户输入默认续跑该任务(UI 提示一行);用户发全新 `/mission` 则归档旧任务为 `aborted`。Ctrl+C 打断 turn → 与今天 episode 一样结束当轮、不审查残缺编辑;任务状态留在磁盘,下一条输入续相。

`state.json` 是唯一相源。不把相存进 `SessionCarry` 当权威——carry 不落库,崩溃后续跑会丢相。

### 5.6 合成消息(持久化,带可辨识前缀)

沿用对抗审查的选择:合成消息以 user 角色入历史,否则续接后 assistant 在回答一条不存在的消息。

```
[mission-design-review round 1/3] An independent design review of your
plan found the following issues. For each: revise the plan, or state
explicitly why it is not a real problem.

1. [completeness] — <message>
   failure scenario: <scenario>
...

[mission-scope round 1/2] These files are outside the locked charter
scope. Either revert them, or the next implementation review must set
fault_layer=design and justify a charter change.

- pkg/unexpected/foo.go

[mission-escalate 1/1] Implementation review concluded the charter
itself is wrong. Re-enter design. Locked charter has been archived.
...
```

### 5.7 配置与手动入口

```yaml
# ~/.deepai/config.yaml
mission_on_plan: false          # 缺省关;true 时 enter_plan_mode 升级为任务
mission_token_budget: 30000     # 单次设计评审子代理预算;0/缺省 → 30k;负值 → 不限
                                # (与 review_token_budget 实施偏离同一 resolver)
mission_timeout: 5m             # 单次设计评审超时
```

不把 `maxDesignRounds` / `maxDesignEscalations` 做成配置——防止配成无限(对抗审查对 `maxReviewRounds` 的同一决定)。

斜杠命令:`/mission`、`/mission abort`、`/mission status`。不接受自由子命令当"给任务的额外指令";运行中纠偏等 PI 设计的 steering(记录为 §七)。

### 5.8 UI

复用现有 `ui.Info` 与 subagent 进度块,不扩 `ReplUI`:

- DESIGN 相开始:`mission: design phase (round i/3)`
- 设计评审 pass/fail:与实施审查同款一行
- 锁章程:`mission: charter locked, N files, M acceptance criteria`
- 升层:`mission: escalating to design (1/1)`
- 范围硬门命中:列出越界文件
- fail-soft / Ctrl+C:明确写"本次未经设计评审"或"实施改动未经审查",沿用对抗审查的警示措辞,不让未审被误认为已审

---

## 六、降级与安全边界

1. **fail-soft 总原则**:设计评审链路上任何失败(超时/挂掉/schema 最终失败/池不可用)→ **不实施**,黄色警示,把计划留给用户。这与实施审查相反(实施审查 fail-soft 是放行已落地的编辑)。设计尚未改业务代码,放行等于跳过整段闭环;停住更便宜也更安全。
2. **设计评审写树**:无 bash,只读工具。仍做审查前后 `git status` 快照;树变则丢弃裁决、按失败处理(不实施)。非 git 目录与实施审查一样明示盲区。
3. **空计划 / 空章程字段**:计一轮 fail,不开工。
4. **大计划**:计划文件已有 64KiB 上限(`plan.go:151`)。设计评审 prompt 超 64KiB 则截断并标注,reviewer 可用 `read_file` 自取全文。
5. **可中断**:全程挂 turn ctx,Ctrl+C 结束当轮;任务状态留盘。
6. **不碰 Gateway / 已删除的 `-q`**:只做 REPL。与对抗审查 §六-6、§十一决策点 7 对齐。
7. **成本封顶**:设计评审走 `mission_token_budget`;实施评审沿用现有 `review_token_budget`。两相共享父回合剩余预算(现有 `RemainingTokenBudgetFromContext`)。

---

## 七、非本期(记录,不实现)

1. **Steering 中途纠偏**:依赖 PI 设计 Phase 2 的双队列。本期任务循环是 turn 之间的门,运行中不可注入。用户只能 Ctrl+C 后 `/mission` 续跑并追加一句,或 abort。
2. **产品经理相**:不把 brief 再经 `product-manager` 洗一遍。歧义靠 DESIGN 相的 `ask_clarification`。多一道 LLM 相而没有程序消费其 schema,违反"schema 只有代码读才配活"。
3. **强制 coder 子代理实施**:主 agent 自己改。孤立 coder 是 orchestrator 失败模式之一。
4. **多审投票 / 跨模型**:沿用对抗审查 §七,等 REVIEW_EVAL 基线。设计评审可另开一份更小的 eval(计划语料),不阻塞本期。
5. **验收命令硬门**(`go test` 退出码):有价值,但是第二条实施门。先让章程 acceptance 进 correctness-reviewer prompt;硬门作为 IMPLEMENT 相增强,避免本期同时发明两种终止条件。
6. **RPC / headless `-p --mission`**:等 PI Phase 1 headless 落地后再接线。

---

## 八、对抗式自审(第 1 轮,2026-09-13)

| # | 发现 | 处置 |
|---|---|---|
| C1 | **再造编排层的诱惑**。阶段表 + 角色注册 + 黑板是 87772b6 的原样。 | **采纳**:只有 `runMission` 一个 for + 两相枚举;实施相调用现有 gate,不复制。 |
| C2 | **实施 fail-soft 放行 vs 设计 fail-soft 放行语义相反**。照抄实施审查会让设计评审一挂就直接改代码,闭环名存实亡。 | **定案**:设计失败/不可用 → 不实施;实施失败/不可用 → 放行已编辑(已落地,停不住)。§六-1 写明。 |
| C3 | **`exit_plan_mode` 人闸会破坏"自动"**。对码 `plan.go:230-271`。 | **补入 §5.2**:任务 DESIGN 内不询问、不退出,由门接管。 |
| C4 | **空 `scope_files` 的 pass 会废掉范围硬门**(放行一切或放行虚无)。 | **`isDesignPass` 代码强制非空**,不信任模型的 verdict 字符串。 |
| C5 | **章程若只活在消息里,compact 后必漂**(D1/D7)。SESSION_DESIGN 原则 3:压缩不改库,但运行时视图会丢。 | **磁盘 lock + turnInjection**,消息里的合成前缀只是给人看的副本。 |
| C6 | **升层 × 设计轮乘积**。若升层重置设计计数,最坏 `maxEscalations * maxDesignRounds * (1+maxReviewRounds)` 次 LLM 相。 | **升层不重置设计轮次**;`maxDesignEscalations=1`。 |
| C7 | **范围硬门过死**:实施时才发现必须改未点名文件。 | **不提供静默扩 scope**;正道是 `fault_layer=design`。豁免表只含生成物。 |
| C8 | **默认开等于拿存量用户当测试集**(对抗审查 §八-1 同构,且更贵:多一整段设计相)。 | **只经 `/mission` 进入**;`mission_on_plan` 默认关。 |
| C9 | **派 architect 子代理做设计看起来干净,实则丢上下文**。 | **主 agent + plan mode**。architect 类型保持手动 `task` 可用,不进本循环。 |
| C10 | **`DesignReviewResult` 与 `ReviewResult` 是否合并**。合并会让实施评审也"必须"填 scope_files,污染现有 gate。 | **独立 struct + 独立 namedSchema**;`Issue` 只加 omitempty 字段。 |
| C11 | **`mission_on_plan` 升级时机**:`enter_plan_mode` 发生在 turn 中,升级若立即切循环会重入 `runTurn`。 | **当 turn 结束后再升级**,下一拍进 `runMission` 的 DESIGN 门(计划多半已写出)。不拆 `runTurn`。 |
| C12 | **续跑权威放 SessionCarry 会在崩溃后丢相**。 | **`state.json` 为相的权威**;carry 只缓存章程渲染。 |

---

## 九、决策点(供本轮评审拍板)

| # | 决策点 | 倾向 | 依据 |
|---|---|---|---|
| 1 | 默认入口 | **仅 `/mission`**,`mission_on_plan` 关 | C8 |
| 2 | 谁设计 | **主 agent + plan mode** | C9 |
| 3 | 设计评审失败时 | **不实施** | C2 |
| 4 | 设计轮上限 | **3** | §5.2 |
| 5 | 升层上限 | **1,且不重置设计轮** | C6 |
| 6 | 范围越界 | **代码硬门,不派 reviewer** | D2;省 token |
| 7 | 越界能否扩 scope | **否,只能升层** | C7 |
| 8 | 章程注入 | **turnInjection + 磁盘 lock** | C5 |
| 9 | 与 `review_after_edit` | **任务内强制审;开关只管普通 episode** | §5.4 |
| 10 | 计划格式 | **自由 markdown + 评审者产出 scope/acceptance**。不强制 YAML frontmatter | 少一条解析面;章程字段由程序消费的 schema 保证 |

---

## 十、实施计划

| 阶段 | 内容 | 涉及 |
|---|---|---|
| Phase 1 | `DesignReviewResult` + `Issue.Area`/`FaultLayer` + `design-reviewer` + `namedSchemas["design_review"]`;现有四 reviewer 的 strict 回归 | `pkg/agent/output.go`、`types_config.go` |
| Phase 2 | 任务落盘(brief/state/charter/reviews.jsonl)+ `metadata.mission_id`;`/mission` `/mission abort` `/mission status` | `pkg/chat/mission.go`、`session.go`、`slashcommands.go` |
| Phase 3 | `runMission` DESIGN 相:强制 plan mode、`exit_plan_mode` 任务语义、设计门、`isDesignPass`、章程锁定、注入 | `plan.go`、`mission.go`、`session_carry.go`、`promptbuild.go` |
| Phase 4 | IMPLEMENT 相接现有 `reviewGate`:章程块进 `buildReviewPrompt`、范围硬门、`fault_layer` 升层、轮次/升层上限 | `review.go`、`mission.go` |
| Phase 5 | 配置 resolver、UI 信息行、续跑、文档;单测 + mock 集成(见 §十一) | `setup.go`、`repl.go`、本文件 |

每阶段停在未提交、等评审,通过后按节号提交——与 PI 设计相同的纪律。

---

## 十一、测试策略

1. **单元**:`isDesignPass` 真值表(verdict/issues/空 scope/空 acceptance);范围硬门(越界、豁免、规范化后的重复路径);升层条件(fault_layer、次数用尽);`exit_plan_mode` 在 mission ctx / 非 mission ctx 的分支;state.json 相迁移;charter hash 与 locked 副本一致。
2. **集成**(mock LLM,沿用 `pkg/chat` 现有模式):设计 pass→实施 pass;设计 fail→修订→pass→实施;设计 3 次 fail→不实施;实施 fail→修复→pass;实施 `fault_layer=design`→回 DESIGN→再锁→实施;设计评审超时不实施;范围越界不派 reviewer;`-c` 后续跑停在 DESIGN / IMPLEMENT;Ctrl+C 后 state 仍在;普通 `runEpisode` 行为不变(无 mission 时零分叉)。
3. **守恒**:`review_after_edit=false` 的普通编辑轮仍不进实施门;任务内即使该开关为 false 也进实施门。
4. **评测**(非本期):设计评审可另做小型计划语料;不阻塞翻默认(反正默认不开任务)。

---

## 十二、风险

1. **设计评审比代码评审软**。没有编译/测试作硬信号,假阳性与假阴性都会更高。缓解:规则 2 要 scenario;规则 4 构造不出必须 pass;`isDesignPass` 要非空章程字段;3 轮上限后交人而不是硬开干。
2. **范围硬门误伤**。计划漏写一个必改文件。缓解:升层一次;主 agent 可在实施评审反驳。不在本期做自动扩 scope。
3. **章程注入破坏 prompt cache 前缀**。memory 注入已有此问题(ARCHITECTURE_REVIEW §2.2)。缓解:章程段放在与 memory 相同的尾部位置,前缀(系统提示主体)不动;不把章程塞进系统提示头部。
4. **`mission_on_plan` 与用户以为的"只是想看一眼计划"冲突**。故默认关;显式 `/mission` 才自动闭环。
5. **Phase 3 碰 `plan.go` 的 exit 路径**。回归必须锁住非任务路径的三选一询问,避免把所有 plan mode 都变成自动。

---

## 十三、修订日志

| 版本 | 日期 | 变更 |
|---|---|---|
| v1 | 2026-09-13 | 初稿。对照已落地的 `runEpisode`、plan mode、87772b6 教训与 PI/对抗审查两份设计的纪律,给出任务循环、章程锁、两层有界评审、升层与实施计划。§八 12 条自审已并入正文。 |
