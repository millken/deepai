# 对抗式自审(Adversarial Self-Review)设计文档

> 在 agent 完成代码修复/新功能后,由**独立上下文的审查子代理**对改动做对抗式审查,裁决不通过时把问题**有界回注**给主 agent 修复,直到通过或达到轮数上限。
>
> **核心原则**:触发是代码保证的(不靠模型自觉),审查是独立上下文的(不让模型给自己打分),裁决是结构化的(不解析自然语言),循环是有界的(不会无限 review-fix),失败是软着陆的(审查环节永远不卡死主流程)。
>
> **版本**:v3(终审定稿)。r1 评审修正 3 处事实错误、补齐 4 个设计缺口并落定全部决策点;r2 终审反转 B4 论据(沙箱后端未接线,防线为零)、补 untracked 新文件 diff 盲区、确认多审投票并发路径已通。修订记录见文末。

---

## 一、背景:为什么两个"显然的方案"都不行

### 1.1 纯提示词驱动 —— 已被判定脆弱

在 system prompt 里要求主 agent "改完代码后自己派 reviewer" 零成本,但链路能否走完取决于模型发挥。`docs/AUTONOMOUS_MULTI_AGENT.md` 缺失能力清单 #2 的原话:**"协调全靠主模型即兴决定调谁……脆弱、无结构保障"**;#4 进一步指出:reviewer 类型存在,但 **"无机制自动跑 implement→review→fix→重审直到通过"**。

### 1.2 独立编排引擎 —— 已被实测证伪

commit `87772b6`(2026-06-09,"实测编排功能不可用,整体移除")删除了 `pkg/orchestrator/`(implement→verify→review→fix 循环、多裁判投票 `MajorityReview`、blackboard)。教训:在 ReAct 循环**之外**另建一层编排状态机,与现有会话/事件/持久化链路脱节,不可维护。

### 1.3 本方案的定位

同样的 implement→review→fix 语义,但**寄生在 REPL 现有的 turn 循环上**,不新建编排层:

- 审查循环是 `runTurn` 调用点外侧的一个有界 for 循环(§4.4),`runTurn` 本身不改;
- 审查者复用现有 subagent 池 + `task` 执行链路(`SubagentExecutor`);
- 裁决复用现有 `ReviewResult` schema + 严格校验管道(`pkg/agent/output.go`);
- 每个修复轮就是一个普通 turn,持久化/记忆调度/事件渲染全部走现成路径。

---

## 二、现状与差距

| 能力 | 现状 | 差距 |
|---|---|---|
| 审查者子代理 | `security-reviewer` / `arch-reviewer`(只读工具集)、`perf-reviewer`(只读 + `bash`),见 `types_config.go:187-213`;提示词已是对抗式措辞 | ❌ 缺**正确性**维度的 reviewer(现有三个是安全/架构/性能) |
| 结构化裁决 | `ReviewResult{Agent, Verdict, Summary, Issues[]}`(`output.go:181-186`)+ `FromStruct`(`output.go:35`,基于 `google/jsonschema-go` 推断)+ `WithStrict(true)`/`WithMaxRetries(1)` 绑定 | ⚠️ `Issue{Severity, File, Line, Message, Suggestion}` 无失败场景字段,需扩展(见 §4.3) |
| 子代理执行 | `Pool.StartTask/Wait`(**无本地并发上限**,`pool.go:25` 注释明确是有意为之)、`SubagentExecutor.Execute`、schema 校验重试、fail-soft 前缀(`subagent.go:510`) | ✅ 直接复用 |
| 改动文件注入 | `SubagentConfig.ContextFiles` → `buildContextFilesBlock`(`subagent_context.go`):单文件 64KiB **截断**,总量超 256KiB 则**直接报错、任务失败**(`subagent_context.go:82-85`) | ⚠️ 总量超限=整个审查 fail-soft 放行,大改动需要降级路径(见 §六-3) |
| 文件变更追踪 | **无** —— 没有任何机制记录一轮改了哪些文件 | ❌ 需新增(见 §4.1) |
| 自动触发 | **无** —— 事件系统只做观察,无 PostToolUse 派发(skill hooks 声明了但未接线) | ❌ 需新增(见 §4.2) |
| 回注机制 | **无** | ❌ 需新增(见 §4.4) |
| 配置开关 | `commands.Config` 有布尔开关先例 | ❌ 需新增 `review_after_edit`(默认关,见 §八-1) |

---

## 三、总体架构

```
用户输入 ──▶ 审查 episode(review.go,外层有界循环)
                │
                ├─ 记录 git status 快照 S0(git 目录时)
                ▼
            runTurn(普通一轮,内部不感知审查)
                │  工具结果处理点记录 edit_file/write_file 成功路径
                ▼
            gate(episode 循环内,turn 之间)
                │
        本轮无编辑? ──是──▶ episode 结束(零开销)
                │否
                ▼
        改动归因:editedFiles ∪ (git status S1 − S0)   ← 捕获 bash 间接编辑
                ▼
        派审查子代理(经现有 subPool,同步等待,Ctrl+C 可跳过)
          agent_type: correctness-reviewer
          context_files: 归因文件(超预算则降级 diff-only)
          prompt: episode 起始的用户需求 + git diff -- <归因文件>
                  (untracked 新文件以 git diff --no-index 拼入)
                │
        审查前后再取快照,树被 reviewer 改动 ──▶ 丢弃裁决,警示,放行
                ▼
        ParseOutput[ReviewResult]
                │
      verdict=pass ──▶ episode 结束,控制权交还用户
      verdict=fail
                │
        round < maxReviewRounds? ──否──▶ 未决 issues 呈给用户,人工裁决
                │是
                ▼
        合成修复消息(持久化,带可辨识前缀)──▶ 下一个 runTurn(修复轮)──▶ 回到 gate
```

审查失败路径(超时/子代理挂掉/schema 解析失败/上下文超限/池不可用)一律**放行并向用户警示"本次改动未经审查"**,绝不阻断主流程。

---

## 四、详细设计

### 4.1 改动归因:哪些文件算"这轮 agent 改的"

三个信息源,取长补短:

| 来源 | 机制 | 角色 |
|---|---|---|
| A. 事件流提取 | 从 `AgentEventToolCallStart` 参数抠 `file_path` | ❌ **否决**:`emit()` 满缓冲即丢弃(react.go:1081 注释),事件流有损,漏一个编辑=漏一次审查,触发器不能建在有损通道上 |
| B. SessionCarry 工具记录 | 在工具结果处理点(`react.go:1013` 附近的串行路径 + `toolexec.go` `handleResult` 并行路径)把 `edit_file`/`write_file` **成功**结果的路径记入 `SessionCarry.editedFiles` | ✅ **主记录**:同步路径无丢失;REPL 轮循环串行,符合 SessionCarry 单 goroutine 契约;跨 Run 存活,修复轮的新编辑自然并入 |
| C. turn 边界 git 快照 | episode 内每个 turn 开始前取 `git status --porcelain` 快照 S0,gate 时取 S1;S1−S0 的新增/变更文件归因为本轮改动 | ✅ **补充记录**:捕获 bash 间接编辑(`go fmt`、`sed -i`、脚本写文件)——工具记录对此是盲的;非 git 目录退化为仅 B,并在启用功能时警示一次盲区 |

**审查范围 = B ∪ C**。快照基线取在 turn 开始前,所以用户在两轮之间的手工改动落在 S0 里、不会被卷入(这正是不用裸 `git diff` 做触发依据的原因)。已知残余:agent 运行期间用户在外部编辑器改文件会被误归因到本轮 —— REPL 阻塞期间的并行编辑属罕见场景,接受并记录。

`SessionCarry` 新增:

```go
// editedFiles 累积自上次审查通过以来 edit_file/write_file 成功触及的
// 绝对路径(去重)。审查 pass 或用户新输入到来时清空。
// 遵循 SessionCarry 既有的单 goroutine 契约,不加锁,永不传给子代理。
editedFiles map[string]struct{}
```

### 4.2 触发点与执行模型

**位置**:审查逻辑不进 `runTurn` 内部,而在其调用点外侧(§4.4 的 episode 循环)。turnErr 非 nil(用户 Ctrl+C、超时)不触发审查 —— 被中断的轮次改动未完成,审查无意义。

**同步阻塞,而非异步**。memory refine 是异步的(`ScheduleRefine`),但审查不能照抄:review-fix 回注需要重新进入 agent 循环,而 SessionCarry/会话消息都要求 REPL goroutine 串行访问;异步审查若与用户下一轮输入并发,会破坏 carry 契约且产生"审查的是已过期代码"竞态。代价是用户在编辑轮后要等审查完成 —— 用现有 spinner UI 显示 "adversarial review in progress",且 **Ctrl+C 可跳过审查**(ctx 取消 → 走 fail-soft 放行路径,不误伤已完成的编辑)。

**gate 前置条件**(全部满足才触发):

1. `review_after_edit` 配置开启;
2. 归因文件集(§4.1)非空;
3. 非 plan-mode 轮(plan mode 只读,理论上无编辑,防御性跳过);
4. `round < maxReviewRounds`。

### 4.3 新增 `correctness-reviewer` 内置类型 + Issue 扩展

现有三个 reviewer 覆盖安全/架构/性能,唯独缺"这段代码逻辑对不对"。新增:

```go
AgentTypeCorrectnessReviewer: {
    Type:         AgentTypeCorrectnessReviewer,
    Name:         "Correctness Reviewer",
    Description:  "Adversarially reviews code changes for logic errors, edge cases, and broken behavior.",
    SystemPrompt: correctnessReviewerSystemPrompt,
    DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "bash"},
    MaxToolCalls: 0,
    Temperature:  0.2,
},
```

- **给 `bash`**:与 `perf-reviewer` 相同的先例(`types_config.go:211`)。正确性指控最有力的证据是"测试/编译真的挂了",允许 reviewer 跑 `go build` / `go test` / 复现脚本。注意 **bash 目前没有任何沙箱**(见 §4.4 防线现状),但给 reviewer bash 并不构成权限升级 —— 主 agent 的 bash 同样无沙箱;reviewer 特有的风险(写树污染审查回环)由 §4.4 的快照防线兜底,且快照是**唯一**硬防线。
- **绑定 `ReviewResult` schema**:在 `types_config.go` 的 `init()` 里与现有三个 reviewer 一致注册 `FromStruct[ReviewResult](WithStrict(true), WithMaxRetries(1))`。

**Issue 扩展**(r1 评审 A3):失败场景是对抗设计的核心,必须有结构化落点,不能靠回注时解析 Message 文本:

```go
type Issue struct {
    Severity   string `json:"severity"`
    File       string `json:"file"`
    Line       int    `json:"line"`
    Message    string `json:"message"`
    // Scenario 是可复现的失败场景(具体输入/状态 → 具体错误行为)。
    // correctness-reviewer 必填(提示词约束);其余 reviewer 可留空。
    Scenario   string `json:"scenario,omitempty"`
    Suggestion string `json:"suggestion,omitempty"`
}
```

对现有三个 reviewer 零影响已确认:`google/jsonschema-go` 的推断规则是带 `omitempty`/`omitzero` 的字段不进 `Required`(infer.go:342-344),strict 校验对缺省该字段的输出照常通过;schema prompt 里多出的可选字段对安全/架构/性能审查同样有益。(`Suggestion` 顺手补 `omitempty`,语义本就是可选。)

提示词草案(与现有 reviewer 三条规则同构,追加对抗性约束):

```
You are an independent adversarial correctness reviewer. Your job is to
try to BREAK the change you are given, not to approve it.

You receive: the original task description, the diff of the change, and
the changed files. You do NOT see the implementer's reasoning — judge
only what the code actually does.

Focus on: logic errors, unhandled edge cases (empty/nil/zero/boundary),
off-by-one, error-path behavior, concurrency hazards introduced by the
change, and whether the change actually satisfies the stated task.

Rules:
1. Do not assume code intent is correct — verify it.
2. Every issue you report MUST include a concrete failure scenario in
   the "scenario" field: specific input or state → specific wrong output
   or behavior. An issue without a reproducible scenario does not count
   — do not report vague concerns.
3. You may use bash to compile or run tests to substantiate an issue,
   but you MUST NOT modify, create, or delete any file in the project —
   you are a reviewer, not a fixer.
4. If you cannot construct a failure scenario, output verdict "pass" —
   do not invent issues, and do not fail a change for style or taste.
5. Output your findings as structured JSON matching the ReviewResult schema.
```

第 2 条是对抗式设计的核心:**给不出可复现失败场景的疑点不算数**。它同时压制两个方向的失败模式 —— reviewer 编造问题刷存在感(假阳性),和 reviewer 泛泛而谈无法指导修复(不可行动)。

### 4.4 审查 episode:执行与回注

**实现形态(r1 评审 B1 决议):外层有界循环,不递归、不拆 runTurn。**

```go
// pkg/chat/review.go(示意)
func (r *ChatRepl) runEpisode(ctx context.Context, userInput string) error {
    initialRequest := userInput            // 固定锚点:所有轮次的审查都以它为需求基准
    input := userInput
    for round := 0; ; round++ {
        snapBefore := gitStatusSnapshot()  // §4.1-C 的 S0
        if err := r.runTurn(ctx, input); err != nil {
            return err                     // Ctrl+C/超时:episode 直接结束,不审查
        }
        scope := r.reviewScope(snapBefore) // editedFiles ∪ 快照差集
        if len(scope) == 0 {
            return nil                     // 纯问答轮零开销
        }
        verdict, ok := r.runReview(ctx, initialRequest, scope) // ok=false: fail-soft 放行
        if !ok || verdict.Verdict == "pass" {
            r.carry.ClearEditedFiles()
            return nil
        }
        if round+1 >= maxReviewRounds {
            r.presentUnresolvedIssues(verdict) // 人工裁决,不回滚
            return nil
        }
        input = synthesizeFixMessage(round+1, verdict) // 下一轮输入
    }
}
```

由此落定 B1 的四个连带问题:

- **runTurn 不改、不重入**:每个修复轮是一个完整的普通 turn。持久化每轮发生(崩溃安全,`-c` 续接完整);`r.turn` 正常递增,memory refine 由现有 `memoryScheduleFor` 节流,修复轮多排几次调度是可接受的既有语义,不做特殊抑制。
- **轮数计数器是循环局部变量**,不进 SessionCarry。Ctrl+C 打断修复轮 → `runTurn` 返错 → episode 结束;用户下一条输入开启全新 episode,round 归零。`editedFiles` 的清空时机不变(审查 pass / 用户新输入 / `/clear`)—— 被打断后用户新输入到来时,残留的 editedFiles 随之清空,旧账翻篇。
- **合成消息持久化入会话历史**,以 user 角色、带 `[adversarial-review round N/M]` 前缀。`-c` 续接时它会作为 user 消息重放 —— 这是有意选择:不入历史则续接后的会话里,修复轮的 assistant 响应在回应一条不存在的消息,上下文断裂;前缀保证人和模型都能辨识其来源。
- **审查的"原始需求"锚定 episode 起始的用户消息**(`initialRequest`),所有轮次不变。round 2 的 reviewer 看到的是:初始需求 + 累积改动的 diff,而非上一条合成消息。

**审查者拿到什么**(刻意设计的信息隔离):

| 给 | 不给 | 理由 |
|---|---|---|
| episode 起始的用户需求 | 主 agent 的思考/工具调用轨迹 | 不被实现者的思路带偏 —— 这是"对抗"的前提;实现者认为对的推理,正是要被独立验证的对象 |
| `git diff -- <归因文件>`(**范围限定**,r1 评审 B2:裸 `git diff` 会卷入用户自己的未提交改动,修复轮可能去"修"用户的代码);**untracked 新文件单独处理**(r2 终审 N1,见下) | 整个会话历史 | 聚焦本次改动;控制 token 成本 |
| `context_files`: 归因文件全文 | — | 提供 diff 周边上下文;超预算降级见 §六-3 |

**untracked 新文件的增量视角(r2 终审 N1)**:agent 用 `write_file` 新建、未经 `git add` 的文件,`git diff` 与 `git diff HEAD -- <files>` 都不显示 —— 而新建文件恰是新功能最典型的产物。处理:对归因集中的 untracked 文件逐个用 `git diff --no-index /dev/null <file>` 生成标准 new-file diff 拼入,并在 prompt 标注 "new file"。**否决 `git add -N`(intent-to-add)方案**:它变更用户的 git 索引状态(`git status` 输出随之改变),审查功能不应有任何 git 状态副作用 —— 与快照防线"审查不改变工作区"的原则同构。归因侧不受此盲区影响:§4.1-C 的 `git status --porcelain` 本身就列 untracked 文件。

已知残余:若 agent 恰好改了用户本就有未提交改动的文件,范围限定 diff 仍含两者混合 —— 无法在文件内区分作者,接受并记录。

**回注消息格式**:

```
[adversarial-review round 1/2] An independent correctness review of your
changes found the following issues. For each: either fix it, or state
explicitly why it is not a real problem.

1. [high] pkg/foo/bar.go:42 — <message>
   failure scenario: <scenario>          ← 来自 Issue.Scenario 结构化字段
   suggestion: <suggestion>
...
```

要求"逐条修复**或说明为何不改**"而不是"必须全改":reviewer 也会犯错,给主 agent 反驳通道;反驳随修复轮的 diff 一起进入重审,由 reviewer 裁断是否接受。

**有界性**:`maxReviewRounds = 2`(常量,非配置 —— 防止配置成无限)。两轮后仍 fail,把未决 issues 原样渲染给用户,状态标注"审查未通过,需人工裁决",**不再自动修复、不回滚**(§八-6)。

**reviewer 写树防线(r1 评审 B4,r2 终审修正论据)**:防线现状是**零沙箱**,而非"沙箱限制不足"—— 事实链(r2 核实):`bash` 工具直通 `sandbox.ExecDirect`(`builtin/bash.go:32`),而 `ExecDirect` 就是裸 `exec.CommandContext("sh","-c",cmd)`(`sandbox.go:479`),无 landlock、无 bwrap,cwd 即项目根;`Agent.sandbox` / `SubagentExecutor.sandbox` 只存储透传,全仓零方法调用 —— landlock/bwrap 后端是**已实现但未接线的死代码路径**。因此 reviewer 的 bash 对项目文件有完全写能力,提示词规则 3 只是软约束,**快照比对是唯一硬防线**:审查子代理运行前后各取一次 `git status --porcelain`,若工作树在审查期间发生变化 —— 无论是恶意"顺手修"还是脚本副作用 —— **丢弃本轮裁决,按审查失败走 fail-soft 放行**,黄色警示列出被改文件,不自动回滚(避免破坏,交用户处置)。`go build`/`go test` 的缓存写在 GOCACHE,不落工作树,不会误伤。非 git 目录连这道防线也没有,随 §4.1-C 的盲区警示一并明示。(把沙箱接进 bash 执行路径是独立于本设计的工程债,不在本期范围。)

### 4.5 配置与手动入口

```yaml
# ~/.deepai/config.yaml
review_after_edit: true      # 默认关闭(缺省 false),显式 opt-in;基线达标后再翻默认(§八-1)
review_token_budget: 30000   # 单次审查子代理 token 预算,0 = 不限
review_timeout: 5m           # 单次审查超时
```

**`/review` slash command**(`pkg/chat/slashcommands.go` 静态表 + `handleSlashCommand` 派发):手动触发一次审查。无参数时审查当前归因文件集;为空则回退 `git diff --name-only HEAD` 的文件集(手动模式下用户明确要求,卷入自己的改动符合预期)。这让未开启自动审查的用户(默认状态)也能按需审查 —— 首发版本的主要入口。

### 4.6 UI 呈现

复用现有 subagent 进度块(TaskEvent 事件流已回传到 TUI):审查子代理以普通 task 形式显示进度。裁决结果增加专用渲染:pass 显示一行绿色确认;fail 显示 issues 列表 + "entering fix round N/2"。以下情形显示黄色警示行,让用户明确知道**这次改动没有经过审查**:Ctrl+C 跳过、fail-soft 放行、上下文超限降级失败、reviewer 写树导致裁决丢弃、非 git 目录的间接编辑盲区(启用时一次性提示)。

---

## 五、对抗性保证清单

| 威胁 | 对策 |
|---|---|
| 自证偏差:模型给自己的代码打分 | reviewer 是独立子代理,全新上下文,看不到实现者的推理轨迹(§4.4 信息隔离) |
| reviewer 被"哄":实现者在代码注释里写"此处已验证正确" | 提示词规则 1"Do not assume code intent is correct";规则 2 要求 `scenario` 字段给出失败场景,注释不构成场景 |
| reviewer 假阳性刷存在感 | 规则 2(无场景不算数)+ 规则 4(构造不出场景必须 pass)+ 主 agent 的反驳通道(§4.4) |
| reviewer 用 bash 篡改被审代码 | **无沙箱兜底**(bash 走裸 `ExecDirect`,landlock/bwrap 未接线 —— r2 终审核实,§4.4);提示词规则 3 仅软约束,唯一硬防线是审查前后 `git status` 快照比对:树变化即丢弃裁决、警示、放行;非 git 目录连硬检测也没有,明示为已知接受的风险 |
| agent 用 bash 间接改代码绕过触发 | 归因 = 工具记录 ∪ turn 边界 git 快照差集(§4.1-C);非 git 目录退化为仅工具记录,盲区明示 |
| 审查递归:reviewer 再派 reviewer | 现有 `filterTaskTool` 已保证子代理无 `task` 工具,嵌套深度封顶为 1 |
| review-fix 无限循环 | `maxReviewRounds = 2` 硬编码;第 3 次 fail 交人工 |
| 同源盲区:实现与审查同一个模型,犯同样的错 | 本期不解决;`SubagentConfig.Model` 已支持覆盖,留作 §7 增强(跨提供商审查) |

---

## 六、降级与安全边界

1. **fail-soft 总原则**:审查链路上任何失败(子代理超时/挂掉/schema 最终解析失败/池不可用/裁决被快照防线丢弃)→ 放行 + 黄色警示,不阻断、不重试超过内建次数。先例:`subagent.go` 的 `outputSchemaWarningPrefix` 降级。
2. **零开销路径**:纯问答轮(归因文件集为空)完全不进 gate。
3. **大改动降级阶梯**(r1 评审勘误:`buildContextFilesBlock` 总量超 256KiB 是**直接报错、任务失败**而非截断,`subagent_context.go:82-85` —— 若不预检,大改动的审查会静默 fail-soft,等于最需要审查的改动反而没审):gate 在 `StartTask` 前预检归因文件总字节数。(a) 未超限:context_files 全文 + 范围限定 diff;(b) 超 256KiB 预算:降级为 **diff-only 审查**,不带 context_files,prompt 注明"文件全文因体量省略,可用 read_file 自取"(reviewer 有只读工具,可按需读)—— 此路径下 untracked 新文件的 `--no-index` 拼入(§4.4 N1)尤其关键,否则它们在 (b) 档完全不可见;(c) diff 本身也超阈值(200KiB):不派审查,显式警示"改动过大未审查,建议 /review 分批"。单文件 64KiB 截断仍存在于 (a) 路径,截断标记由现有机制注入。
4. **成本封顶**:`review_token_budget` + 现有 `RemainingTokenBudgetFromContext` 折算进父预算。
5. **可中断**:审查与修复轮全程挂在 turn ctx 上,Ctrl+C 立即结束 episode 并走放行路径;已完成的编辑不受影响。
6. **不碰 Gateway 路径**:本期只做 REPL。HTTP 网关的会话模型不同(无阻塞式交互约定),留待后续评估。

---

## 七、非本期的增强方向(记录,不实现)

1. **多审投票**:同一 diff 并发派 2-3 个独立 reviewer,多数 fail 才回注(被删 orchestrator 的 `MajorityReview` 中唯一值得回收的部分)。并发路径**现在就是通的**(r2 终审 N2 补强):池无本地并发上限(`pool.go:25` 注释明确是有意设计,`chat.go:365` `NewSubagentPool(subExecutor, 0)`),且 `task` 工具本身 `ParallelSafe: true`(`tools/subagent.go:48`),同批多个 task 调用即并发执行,无排队惩罚。不进本期的理由是成本与两项设计功课:(a) "多数裁决"阈值需要 §十-3 的检出率/假阳性率基线来标定;(b) 同一处注释同时警示**子代理共享工作树与 git 索引** —— N 个带 bash 的 reviewer 并发跑测试可能在文件系统层互相干扰,且 §4.4 的快照防线在并发审查下无法把树变化归因到某一个 reviewer,防线语义需要重新设计(如并发期间任何树变即全体弃权)。基线建立后为**第一优先增强**。
2. **跨模型审查**:`review_model` 配置项,用不同提供商的模型做 reviewer,消解同源盲区。管道已支持(`SubagentConfig.Model`),只差配置面。与 #1 组合(N 个不同模型投票)是终态形态。
3. **YAML 自定义 reviewer**:`AgentTypeConfig.OutputSchema` 目前是 `yaml:"-"`,项目自定义 agent 无法进入结构化裁决管道。给 `yamlAgentConfig` 加 `output_schema: review` 枚举字段后,`.deepai/agents/*.yaml` 即可定义领域专属对抗审查者。
4. **多维度并审**:correctness + security 双 reviewer 并发(同 #1,基础设施已就绪,等基线)。
5. **PostToolUse hook 派发**:skill 系统声明了 `PreToolUse`/`PostToolUse` 但从未派发(`skill/hooks.go:16-20` vs `executor.go`),接线后可支持"编辑特定 glob 时即刻触发轻量检查"这类更细粒度策略 —— 与本设计正交,不混入。

---

## 八、决策点决议(r1 评审拍板)

| # | 决策点 | 决议 | 依据 |
|---|---|---|---|
| 1 | 默认开关 | **(b) 首发默认关**,`review_after_edit: true` 显式 opt-in | r1 评审:与 `memory_auto_refine_disabled` 的类比不成立 —— 那是异步廉价后台任务,本方案是同步阻塞 + 最坏 3×30k 审查 token + 2 个完整修复轮的主 agent 开销;且 §十-3 基线未建立,默认开等于拿存量用户当测试集。**翻默认的条件**:对抗性评测基线达标(检出率/假阳性率有数)后再改为默认开 |
| 2 | 回注轮上限 | **2 轮** | 1 轮不给修复后重审的机会;3 轮在模型能力不足时浪费显著 |
| 3 | 审查同步性 | **同步阻塞** | 异步与 SessionCarry 串行契约冲突,且存在"审查已过期代码"竞态(§4.2) |
| 4 | reviewer 的 bash | **保留 bash**,前置条件是 §4.4 快照防线与本期同步落地 | 跑测试是正确性指控最硬的证据,perf-reviewer 有先例。r2 终审推翻了"沙箱兜底"这一原始论据(沙箱后端未接线,防线为零),但决议不变,理由重述:(1) reviewer 拿 bash 不是权限升级 —— 主 agent 的 bash 同样无沙箱,拒给 reviewer bash 并不降低系统整体暴露面;(2) reviewer 特有的真实危害是副作用污染 diff 回环 + 违反"审查只读"语义,这两者恰好都被快照防线覆盖(检测到即弃裁决)。故防线不是增强而是本期必做;非 git 目录无硬检测,如实标注为已知接受的风险 |
| 5 | 审查范围 | **归因文件增量**(工具记录 ∪ 快照差集),diff 限定 `git diff -- <归因文件>` | 全量 diff 会反复审查用户自己的未提交改动(B2),与"审 agent 的产出"目标不符 |
| 6 | 第 3 次 fail 的行为 | **呈给用户人工裁决,不回滚** | 回滚是破坏性动作,且改动可能大部分正确;人工裁决保守且信息完整 |
| 7 | plan-mode / `-q` 单次模式 | **REPL 先行**,`-q` 列入 Phase 5 后评估 | `-q` 无交互通道,fail 后只能打印,价值打折但仍可做 |

---

## 九、实施计划

| 阶段 | 内容 | 涉及文件 |
|---|---|---|
| Phase 1 | `SessionCarry.editedFiles` + 记录点(串行/并行两条工具路径)+ turn 边界 git 快照工具函数 | `pkg/agent/session_carry.go`、`react.go`、`toolexec.go`、`pkg/chat/review.go` |
| Phase 2 | `Issue.Scenario` 字段 + `correctness-reviewer` 类型 + schema 绑定(验证现有三 reviewer 的 strict 校验不受影响) | `pkg/agent/output.go`、`types_config.go` |
| Phase 3 | episode 循环 + gate + 归因合并 + 降级阶梯 + 回注 + reviewer 写树快照防线 | `pkg/chat/review.go`、`repl.go`(调用点改造) |
| Phase 4 | 配置项 + `/review` 命令 + UI 渲染(含各警示路径) | `pkg/commands/setup.go`、`pkg/chat/slashcommands.go`、TUI |
| Phase 5 | 文档 + 集成测试 + 对抗性评测基线;评估 `-q` 模式接入 | `README.md`、`docs/` |

## 十、测试策略

1. **单元**:editedFiles 记录(成功/失败编辑、并行批次、跨修复轮累积、清空时机);快照差集归因(bash 间接编辑、非 git 目录退化);diff 组装(范围限定、untracked 文件 `--no-index` 拼入且不触碰索引、混合场景);gate 前置条件矩阵;降级阶梯三档分支;回注消息格式(含 Scenario 字段渲染);`maxReviewRounds` 边界;合成消息前缀与持久化。
2. **集成**(mock LLM,沿用 `pkg/agent` 现有 mock 模式):pass 直通、fail→fix→pass、fail→fix→fail→人工呈现、reviewer 超时 fail-soft、schema 解析失败 fail-soft、上下文超限降级、reviewer 写树→裁决丢弃、Ctrl+C 结束 episode、`-c` 续接后合成消息重放的辨识。
3. **对抗性评测基线**(人工/脚本化,非 CI):植入已知 bug 的 diff 集,统计 reviewer 检出率与假阳性率,作为提示词迭代基线,亦是 §八-1 翻默认开与 §七-1 投票阈值标定的前置条件。

---

## 十一、实施状态与偏离记录(2026-08-16,Phase 1-5 完成)

| 阶段 | 提交 | 内容 |
|---|---|---|
| 设计 | `f0a78bd` | 本文档 v3 |
| Phase 1 | `0b59424` | editedFiles 归因 + turn 边界快照 |
| Phase 2 | `9efdd7e` | `Issue.Scenario` + `correctness-reviewer` |
| Phase 3 | `682468f` | episode 循环 + gate + 派发 + 防线 |
| Phase 4 | `2cb5e20` | 配置接线 + `/review` 命令 |
| Phase 5 | (本次提交) | README/设计文档更新、`-q` 评估关闭 |

**实施对设计的偏离**(均已在代码注释中就地说明):

1. **§4.5 `review_token_budget` 语义**:设计写"0 = 不限",但 YAML 缺省即 0,会让默认值变成不限、与示例的 30k 矛盾。实施改为:0/缺省 → 30k 默认,**负值 → 不限**(`resolveReviewTokenBudget`)。
2. **§4.4 记录点补漏**:并行批次致命中断时尾部结果走 `appendRemaining` 而不经 `handleResult`,但那些编辑真实执行过。`appendRemaining` 签名扩为 `(calls, results)` 并同样记录,否则致命批次的编辑漏审。
3. **episode 覆盖面大于设计**:除主输入循环外,续聊输入、启动 auto-continue、文件型 slash command 的 body 轮也路由进 episode——都是可能产生编辑的用户发起轮。
4. **§4.5 `/review` 空范围回退**:设计写回退 `git diff --name-only HEAD` 文件集,实施改用快照 dirty 集(含 untracked)——HEAD diff 口径漏掉新建文件,与手动审查意图不符。
5. **§4.6 UI**:渲染走现有 `ui.Info` 通道(全部警示路径已覆盖,含 "changes are unreviewed" 字样与修复轮播报);彩色专用渲染需扩 `ReplUI` 接口,未做。
6. **裁决判定放宽**:`verdict=="pass"` **或 issues 为空**均按 pass——fail 却零 issue 的裁决无法指导修复。

**决策点 7 关闭**:`-q` 单次模式在实施时已被上游整体废弃(`repl.go` 对 `Query != ""` 直接报错 "no longer supported"),不存在需要接入的路径。Gateway 路径维持 §六-6 的"本期不碰"。

**遗留工作**(非代码):§十-3 对抗性评测基线(植入 bug 的 diff 集 + 检出率/假阳性率统计)需真实 LLM 离线跑,是 §八-1 翻默认开与 §七-1 多审投票阈值标定的前置条件,另行安排。

## 修订记录

- **v3(2026-08-16,终审定稿)**:r2 终审修订 ——
  - **B4 论据反转(比 r1 判断更严重)**:防线不是"landlock 授权过宽",而是**零接线** —— `bash` 直通 `sandbox.ExecDirect` = 裸 `exec.CommandContext("sh","-c",cmd)`(`sandbox.go:479`);`Agent.sandbox`/`SubagentExecutor.sandbox` 仅存储透传,全仓零方法调用;landlock/bwrap 是死代码路径。§4.3/§4.4/§五/§八-4 相应改写:快照比对是唯一硬防线;决策点 4 决议不变但论据重建(reviewer bash 非权限升级,真实危害是副作用污染回环 + 违反只读语义,均被快照覆盖)。
  - **N1**:untracked 新文件不出现在 `git diff` 中,而新建文件是新功能最典型产物。采用 `git diff --no-index /dev/null <file>` 逐个拼入 new-file diff;否决 `git add -N`(变更用户索引,违反"审查零副作用"原则)。归因侧不受影响(`git status --porcelain` 本就列 untracked)。
  - **N2(A2 加强)**:`task` 工具 `ParallelSafe: true`(`tools/subagent.go:48`),多审投票并发路径现在就是通的;§七-1 补充共享工作树/git 索引警示 —— 并发审查下快照防线无法归因树变化,防线语义需重设计,列为投票增强的前置功课。
- **v2(2026-08-16)**:r1 评审(15/18 事实核对通过)修订 ——
  - 事实修正:§二 reviewer 工具集(perf 含 bash,非全只读);§七-1 池并发论断反转(`NewSubagentPool(subExecutor, 0)` 两参、`pool.go:25` 明确无本地并发上限,投票无需先提并发,优先级上调);`Issue` 无场景字段(新增 `Scenario`,`omitempty` 不进 Required 已对 `google/jsonschema-go` infer.go:342-344 核实);`FromStruct` 位于 output.go:35;`ReviewResult` 含 `Agent` 字段;context_files 总量超限是任务失败而非截断(新增 §六-3 降级阶梯);`review_policy` 更名实际符号 `MajorityReview`。
  - 设计补齐:B1 修复轮形态(外层有界循环,runTurn 不改;计数器循环局部;合成消息持久化带前缀;需求锚定 episode 起始消息);B2 diff 范围限定 `git diff -- <归因文件>`;B3 bash 间接编辑盲区(turn 边界快照归因,非 git 明示);B4 reviewer 写树防线(审查前后快照比对,树变即弃裁决)。
  - 决策落定:§八 全部 7 项(1 改为首发默认关,4 以 B4 防线为前置,余按原倾向)。
- **v1(2026-08-16)**:初稿。设计基于 main @ 297f258。文中行号为快照,实施时以符号名为准。
