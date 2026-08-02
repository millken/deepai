# 自治多 Agent 协同 — 现状评估与设计笔记

> **⚠️ 2026-08 勘误**：§9 描述的 orchestrator（`pkg/orchestrator`、implement_task/design_task/build_task、黑板、多评委投票、`MaxAgentCalls`）已于 `87772b6`（2026-06-09，"实测编排功能不可用"）**整体删除**，§9 全节不再反映现状。另：§3 所述"CLI 并发为 1"现为 4；§4-#7 所述"coder 含 task 可嵌套"不成立（task 恒被剥离，深度封顶 1）。当前路线与最新评估见 [ARCHITECTURE_REVIEW.md](ARCHITECTURE_REVIEW.md)。

> 目标场景:**给定一个任务,多个 agent 从讨论/设计开始,经规划、实施、自我验证,直到最终完成,全程无人干预。**
>
> 本文基于 2026-06 的代码现状评估 deepai 距离该目标的差距,并给出分阶段的设计建议。结论先行:deepai 目前处于"主管 + 专家委派"阶段,要达到"团队自治协同"还缺一个**编排层**。

---

## 1. 目标能力分解

把"从讨论到落地全自治"拆成阶段,逐段看 deepai 的成色:

| 阶段 | 含义 | 现状 |
|---|---|---|
| 讨论 / 设计 | 多角色就方案多轮往返、形成共识 | ❌ 不支持(无 peer 通信) |
| 规划 | 产出可执行计划 | 🟡 有 plan 模式,但需用户批准 |
| 实施 | 按计划修改代码 | ✅ coder 子 agent 可做 |
| 自我验证 | 实现→评审→修复直到通过 | ❌ 无自动闭环(reviewer 角色存在但不自动循环) |
| 完成 / 交付 | 自动提交、汇总 | ✅ coder 自动 git_auto_commit |
| 全程无干预 | 不停下来等人 | 🟡 autonomous 模式可不阻塞,但靠"猜"而非澄清 |

---

## 2. 现状:已经做到的(地基)

deepai 已具备**角色化层级委派**的完整链路:

- **13 个内置 agent 类型** — researcher / architect / coder / product-manager / security-reviewer / arch-reviewer / perf-reviewer / analyst / frontend / ui-designer / news / bash / general-purpose,各自有定制 system prompt、工具白名单、温度。见 [pkg/agent/types_config.go](pkg/agent/types_config.go#L11-L23)。
- **层级委派** — 主 agent 通过 `task` 工具生成指定类型的子 agent,传 prompt,拿回最终结果。见 [pkg/tools/subagent.go](pkg/tools/subagent.go#L17)。
- **工具按角色收敛** — reviewer 只读;coder 拿到 edit/bash/git。见 [pkg/agent/subagent.go](pkg/agent/subagent.go#L72-L83)。
- **结构化输出** — 每个 agent 类型可带 `OutputSchema`,子 agent 返回 JSON。见 [pkg/agent/subagent.go](pkg/agent/subagent.go#L96-L99)。
- **无人值守开关** — autonomous 模式让 `ask_clarification` 非阻塞,agent 不会停下来等人。见 [pkg/commands/chat.go](pkg/commands/chat.go)。
- **plan 模式** — 只读探索 → 写 plan → 提交审批。见 [pkg/agent/plan.go](pkg/agent/plan.go)。
- **coder 自动提交** — 完成后调用 git_auto_commit。

适合的活:有明确主线、子任务弱耦合的场景,如"调研→报告""实现一个明确功能并自测提交"。

---

## 3. 核心架构事实(决定了上限)

deepai 的多 agent 是 **严格层级、阻塞式、fire-and-forget**:

```
主 agent --task--> 子 agent (单条 prompt 从零起跑) --文本结果--> 主 agent
```

具体表现(均经代码核实):

- **子 agent 从零起跑**:只带一条 human prompt,**无对话历史、无记忆服务**(构建子 agent 的 `AgentConfig` 根本没传 `MemoryService`),见 [pkg/agent/subagent.go](pkg/agent/subagent.go#L101-L137)。
- **委派是阻塞的**:`task` 工具 `StartTask` 后立即 `Wait` 到子 agent 结束,父无法中途监督/纠偏。见 [pkg/tools/subagent.go](pkg/tools/subagent.go#L48-L61)。
- **返回是纯文本**:子 agent 返回 `FinalOutput` 字符串,父需自行解析/信任。
- **父是唯一整合者**:agent 之间只共享沙箱文件系统,无共享上下文/黑板。
- **CLI 并发为 1**:`NewSubagentPool(subExecutor, 1, 0)`,子 agent 串行执行。见 [pkg/commands/chat.go](pkg/commands/chat.go#L238)。

> ⚠️ 文档勘误:`docs/MULTI_AGENT.md` 声称存在 "Environment 发布/订阅消息总线,agent 间异步通信"。**代码中并无此实现**(`pkg/agent`、`pkg/subagent` 无任何 Publish/Subscribe/MessageBus)。该段为陈旧/愿景文档,不代表现状。

---

## 4. 缺失能力清单(从"委派"到"协同自治")

| # | 缺失能力 | 现状 | 对目标场景的影响 |
|---|---|---|---|
| 1 | **真正的讨论/辩论** | 父→子单向,子返回即结束;无 peer 通信、无多轮往返、无共享黑板 | "从讨论开始"做不到;只能由父 agent 串行调用、手工把输出当 prompt 喂下去 |
| 2 | **确定性编排引擎** | 协调全靠主模型即兴决定调谁,无声明式 discuss→plan→implement→review→fix 流水线 | 链路能否走完取决于主模型发挥,脆弱、无结构保障 |
| 3 | **共享工作区/黑板** | 仅共享沙箱文件;子 agent 各自从空白起跑,无共享上下文/记忆 | agent 无法在演进中的共识上叠加 |
| 4 | **验证/批判闭环** | reviewer 类型存在,但无机制自动跑 implement→review→fix→重审直到通过 | "自我验证到完成"需主 agent 每次手工编排 |
| 5 | **真并发** | CLI 写死并发 1 | "团队同时协作"实为排队 |
| 6 | **异步/可监督委派** | `task` 阻塞到子结束 | 长任务无法动态调整、打断 |
| 7 | **全局预算与递归护栏** | coder 含 `task` 可嵌套,但无深度限制、无跨 agent 树的 token/成本预算 | 无人值守时成本可能失控、递归可能跑飞 |
| 8 | **"无干预"硬伤** | plan 的 `exit_plan_mode` 要求用户批准;autonomous 遇歧义靠猜而非澄清 | 与"全程不干预"冲突;歧义靠猜累积质量风险 |
| 9 | **团队级状态追踪** | 无共享 todo/任务图、无子任务依赖、无"谁在干什么";父 context 是唯一状态且会被压缩 | 大任务失去全局视图 |

---

## 5. 前置条件:单 agent 的"无人值守稳定性"

多 agent 自治的前提是单 agent 能长时间稳定无人值守跑。本项目近期修复的若干 bug 恰是此类拦路虎,记录于此以示"自治"对可靠性的依赖:

- 流式 usage 不上报 → token 预算永不增长(无人值守成本无界)+ 压缩估算失效。
- ctrl+c / 取消后丢失上下文,续跑从头开始。
- 校验熔断器被批次内成功工具绕过 → 死循环。
- 瞬时网络错误(带内 SSE error 事件)不重试,直接停下。

仍待处理(见审计清单):会话内存与 DB 不同步(`/undo` 删错行)、记忆 flushVersion 竞态、虚拟路径越权等。**建议:在投入大规模多 agent 自治前,先清掉这些可靠性欠债。**

---

## 6. 建议的演进路线

把"委派"升级为"协同自治",关键是补**编排层**。分四阶段,按性价比排序:

### 阶段 A(最高优先):实现-验证-修复闭环
最小、最实用的协同单元。固化一个循环:
```
coder 实现 → 跑 build/test → reviewer 评审 → 若有问题回到 coder 修复 → 重审,直到通过或达上限
```
- 复用现有 coder + *-reviewer 角色与 OutputSchema(评审产出结构化 issues)。
- 终止条件:测试通过且评审 verdict=pass,或达到轮次/预算上限。
- 不需要 peer 通信,纯靠"阶段 + 循环 + 结构化交接",落地成本低、立竿见影。

### 阶段 B:轻量编排原语
一个能声明 `[阶段] → [每阶段 fan-out 哪些角色] → [gather + 验证] → [不过则回到某阶段]` 的工作流引擎(参考成熟 agent harness 的 Workflow 概念):
- 确定性控制流(循环/条件/fan-out),而非靠主模型即兴。
- 每阶段产物作为下阶段输入,带结构化契约(OutputSchema 强制)。
- 跨 agent 树的**全局 token/成本预算**与并发上限。

### 阶段 C:共享上下文/黑板
- 给子 agent 注入共享的只读上下文(任务目标、已达成的决策、相关文件摘要),不再每次从零起跑。
- 一个 append-only 的"决策记录/任务图",作为团队共享状态(替代仅靠父 context)。

### 阶段 D:真正的讨论/辩论
- 多角色就同一议题多轮往返(如 architect 提案 → coder 质疑可行性 → 修订),由编排层调度轮次与收敛条件。
- 这是"从讨论开始"的核心,但依赖 A/B/C 先就位,最后做。

---

## 7. 阶段 A 的接口草图(供讨论)

不引入新框架,在现有 subagent pool 上加一个可复用的循环编排函数(伪代码):

```
ImplementVerifyFix(ctx, taskPrompt, opts) Result:
    for round in 1..opts.MaxRounds:
        impl   = task(agent_type="coder",   prompt=taskPrompt + context)
        test   = bash("go build ./... && go test ./...")     # 或项目自定义校验命令
        review = task(agent_type="<reviewer>", prompt=diff + test输出,
                      OutputSchema=ReviewVerdict{verdict, issues[]})
        if test.ok and review.verdict == "pass":
            return Done(impl, round)
        context += 结构化的 test 失败 + review.issues   # 作为下一轮 coder 的输入
    return GaveUp(lastState, reason)
```

关键设计点:
- **终止可判定**:依赖客观信号(测试退出码)+ 结构化评审 verdict,而非主模型自述"完成"。
- **预算**:`opts` 带轮次上限与 token 预算,超出即停并汇报(对接已修复的 usage 上报)。
- **可恢复**:每轮状态可持久化,支持中断后续跑(对接会话持久化)。
- **先串行**:阶段 A 不需要并发;并发留给阶段 B 的 fan-out。

---

## 9. 实现状态:阶段 A 已落地(2026-06)

实现-验证-修复闭环已作为第一个原型实现:

- **核心循环(纯逻辑,可测)**:[pkg/orchestrator/orchestrator.go](pkg/orchestrator/orchestrator.go) — `Run(ctx, Config, taskPrompt, SubagentRunner, Verifier, Differ)`。终止条件 = 验证命令退出码 0 **且** 评审 verdict=pass;否则把验证输出 + 评审 issues 作为反馈喂给下一轮 coder,直到通过或达 `MaxRounds`。verdict 解析容忍 prose 包裹的 JSON 与字符串内嵌套花括号。
- **真实适配器 + 工具**:[pkg/tools/orchestrate.go](pkg/tools/orchestrate.go) — `implement_task` 工具。`poolRunner` 用现有 subagent pool 跑 coder/reviewer;`cmdVerifier` 用 `sandbox.ExecDirect` 跑 `verify_command`(退出码判定);`gitDiffer` 用 `git diff` 取改动给评审。已在 CLI 注册([pkg/commands/chat.go](pkg/commands/chat.go))。
- **测试**:[pkg/orchestrator/orchestrator_test.go](pkg/orchestrator/orchestrator_test.go) 用 fake 覆盖收敛、循环修复、达上限放弃、验证失败即使评审通过也不算完成、无验证器仅靠评审、coder 报错冒泡、verdict 容错解析。

**评审客观性增强(已落地)**:
- 客观锚点 = `verify_command` 退出码;`Result.Verified` 仅在验证真跑过且通过时为 true,仅评审完成会标注 UNVERIFIED;`require_verification` 可设为硬闸门。
- 对抗式评审(默认找问题、判 fail)+ 证据绑定(issue 必须 file:line)+ 空 diff 直接判 fail。
- **多评委投票**(#4):`reviewers` 可配多个角色(arch/security/perf-reviewer),`review_policy` 取 unanimous(默认,任一否决即 fail)或 majority。降单评委方差。
- **独立评审模型**(#6):`review_model` 让评审用与 coder 不同的 model,去自评偏差;经 `SubagentConfig.Model` 按 agent 类型路由。

**阶段 B 部分落地(编排原语)**:
- **并发 fan-out**:多评委面板现在并发执行(`fanOutReviews`,有界并发,结果按评委顺序索引保证确定性),CLI subagent pool 并发上限从 1 提到 4 以真正并行(coder 仍串行,只读评审并行,安全)。
- **全局 agent-call 预算**(#B2 雏形):`Config.MaxAgentCalls` / 工具参数 `max_agent_calls` 在回合边界强制——只启动预算够跑完整轮(coder + 全部评委)的回合,超出即停并在 `Result.AgentCalls`/reason 汇报。为无人值守提供确定性成本上限。

**设计面板(第二种编排形态,已落地)**:
- `orchestrator.Design` + `design_task` 工具:多个 proposer 子 agent **并行**从不同角度(简单/健壮/最小改动/性能)起草方案,再由 judge 子 agent 批判并**综合出一份最终计划**。这是"从讨论/设计"的前半段,与 implement-verify-fix 是不同的形态(generate→judge→synthesize),可与 implement_task 组合:design_task 出计划 → implement_task 落地。只读,产物有序、judge 解析失败时回退原文。

**端到端串联(已落地)**:
- `orchestrator.Build` + `build_task` 工具:把设计面板与 implement-verify-fix **确定性地**接成一步——design 综合出的 plan 自动、完整地喂进实现阶段(coder 提示同时含原任务与 vetted plan),一次调用跑完"讨论→设计→实现→验证→修复"。这是阶段 B"声明式流水线"的一个具体两阶段实例,也是"全程不干预"最贴近的形态。design 阶段失败则不进入实现阶段。

**如何触发**:
- **手动(确定性)**:slash 命令 `/design <任务>`、`/implement <任务> [-- 验证命令]`、`/build <任务> [-- 验证命令]` 直接调对应工具,绕过模型的工具选择;`-- ` 之后是可选的 shell 验证命令(如 `/build 加缓存 -- go build ./... && go test ./...`)。Ctrl+C 可中断。
- **自动(软)**:主 agent 的系统提示里有一句引导,对"较大、自包含的实现任务优先用 build_task/design_task/implement_task"——提高自动选用概率,但不保证;只加在主 agent(子 agent 用各自 profile 提示,且不持有这些工具)。

**共享黑板(阶段 C 的务实版,已落地)**:
- `Blackboard`(plan + 跨轮累积的决策/笔记)贯穿一次编排运行,**注入每个子 agent 的提示**。具体收益:`build_task` 里 design 商定的 plan 现在**同时进入 coder 和 reviewer**——reviewer 据此判断,并被明确告知"不要把 plan 里的有意决策当 bug 否掉"(此前 reviewer 只看 task+diff,会误杀设计选择);修复循环里每轮的 review 摘要作为笔记累积,后续轮次的 coder 能看到"已商定/已要求"的历史,不再只拿最近一轮反馈。
- 务实点:黑板由编排器从各阶段产物**确定性地维护**(plan 来自 design、笔记来自 review 摘要),子 agent 只读不直接写——避免了"自由写黑板"的膨胀与混乱风险。真正的多 agent 互写/辩论(阶段 D)仍未做。

仍**尚未做**:跨 agent 树的全局 **token** 预算(目前是按调用次数,非 token)、每轮状态持久化/可恢复、把多形态抽成可任意声明的通用流水线引擎、子 agent 互写黑板/辩论(阶段 D)。客观性的硬锚点仍是 `verify_command`——评审已是"挑剔+多评委+可异model"的强化主观判断,但不等于客观真理。

## 8. 一句话结论

deepai 把"角色化专家委派"做得不错,但"团队从讨论到落地全自治"还差一个**编排层**(确定性工作流 + 共享上下文 + 验证闭环 + 全局预算)。**第一步建议做阶段 A 的"实现-验证-修复闭环"**——它用现有角色就能让 coder 与 reviewer 真正协同,而不只是被串行调用,且为后续阶段铺好结构。
