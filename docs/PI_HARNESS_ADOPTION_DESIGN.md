# Pi Harness 优点吸纳设计(草案 v3)

> 状态:**待评审**,未提交。v1 → v2 经一轮第一性原理自审;v2 → v3 响应第 2 轮(用户)评审意见。发现与处置见 §八。
> 调研对象:[earendil-works/pi](https://github.com/earendil-works/pi)(Mario Zechner 的极简终端 harness,2025-08 首发,现为 Earendil 旗下项目;Terminal-Bench 2.0 验证,Databricks 报告其通过率最高且每轮上下文比 Claude Code 少约 3 倍)。
> 参考资料:pi 仓库 docs(extensions.md / session-format.md / compaction.md / rpc.md)、Zechner 博文 ["What I learned building an opinionated and minimal coding agent"](https://mariozechner.at/posts/2025-11-30-pi-coding-agent/)、HN 讨论。

## 一、背景与目标

pi 与 deepai 是同类项目(终端 agent harness),但哲学不同:pi 极简内核 + TypeScript 扩展承载一切;deepai 功能内置较多(memory、review gate、docx、MCP、plan mode)。本文逐项对照 pi 的设计优点与 deepai 现状,给出**采纳 / 部分采纳 / 不采纳**决策及理由,并把采纳项组织为分阶段实施方案。

目标不是"变成 pi",而是补齐 deepai 在**交互模型**(运行中不可交互)、**可脚本化**(无 headless)、**会话操作**(无分支)、**可扩展性**(插件只有静态发现、无生命周期钩子)四个方向上与 pi 的实质差距,同时保留 deepai 已有的差异化优势(memory、aging、review gate、sandbox)。

## 二、Pi 核心设计要点(调研摘要)

1. **Steering / Follow-up 双队列**:运行中按 Enter 发送 steering 消息——"在当前 assistant 轮的工具调用执行完后送达",不打断在途工作即可纠偏;Alt+Enter 发送 follow-up——排队到"agent 全部工作完成后"。Escape 中断时**把队列中的消息恢复到输入框**,用户打的字永不丢失。
2. **树状会话**:JSONL 会话条目带 `id`/`parentId` 构成树,原地分支(`/tree`、`/fork`、`/clone`);压缩产生 `CompactionEntry`,切分支时生成被放弃分支的摘要(branch summary),上下文不丢。
3. **事件驱动内核 + 可拦截钩子**:loop 发出有序结构化事件(`agent_start`…`tool_execution_end`…);扩展可注册 `tool_call`(**可阻断**)、`tool_result`(**可修改**)、provider 请求前后、input 拦截等钩子。权限门禁、git checkpoint、子代理都是官方示例扩展而非内核功能。
4. **极简工具集与系统提示**:模型可见默认工具只有 read/write/edit/bash 四个;系统提示 <1k token;论点是"模型已被 RL 训练得深刻理解编码 agent,精确控制进入上下文的内容才能得到更好输出",并且"不在 UI 之外偷偷注入任何东西"。其"每轮上下文少 3 倍"来自两处:系统提示极简 **且** 模型可见工具 schema 极少(其批评的 Playwright MCP 一项即 21 工具 13.7k token)。
5. **Headless 全等价**:`--mode json`(事件 JSONL)与 RPC 模式(stdin/stdout 严格 LF 分帧 JSONL,命令集覆盖 `prompt/steer/follow_up/abort/set_model/compact/fork/get_tree/bash` 等),与交互模式功能完全对等,支撑 SDK 嵌入与 evals。
6. **用户可编辑模型注册表**:`~/.pi/agent/models.json` 定义自定义 provider/model,`apiKey` 支持 `"!command"`(shell)、`"$VAR"`(环境变量)、字面量三种解析;含 contextWindow、分层 cost(input/output/cacheRead/cacheWrite)、compat 标志;`/model` 时热重载,无需重启。
7. **刻意不做的清单**(每项都有逃生通道):不做 MCP(上下文污染)、不做内置子代理("黑盒套黑盒")、**不做权限弹窗**(agent 能执行代码后护栏即演戏,应容器化隔离)、不做 plan mode / todo("confuse models",用 markdown 文件替代)、不做后台 bash(用 tmux)。
8. **主屏差分渲染**:不用 alt-screen,组件 `render(width) -> []string`,渲染器 diff 后从首个变更行起重绘,配合同步输出(CSI 2026)无闪烁,**保留终端回滚缓冲**。

## 三、deepai 现状对照(关键论断均已对码核实)

| 维度 | pi | deepai 现状 |
|---|---|---|
| 运行中交互 | steer(Enter)/ follow-up(Alt+Enter)/ Escape 恢复输入 | **无**。`turnStartMsg` 置 `inputVisible=false`(`pkg/chat/tui.go:439-440`),运行中仅 Ctrl+C 中断(`tui.go:534-543` 经 `interruptCh`)、Ctrl+T 任务面板 |
| Agent 生命周期 | 常驻 agent 对象,steer/followUp 注入队列 | Agent **单轮单次使用**(守卫 `pkg/agent/react.go:383-387`),REPL 每轮 `agent.New`,跨轮状态经 `SessionCarry`(`pkg/agent/session_carry.go`)携带 |
| 会话存储 | JSONL 树(id/parentId),原地分支 | SQLite 线性序列(`pkg/chat/session.go:89` `SQLiteSessionStore`),仅有 `/undo`(删尾),**无分支/fork**;sessions 表已有 `metadata` JSON 列(session.go:46),可承载派生关系无需迁移 |
| 压缩 | 持久化 `CompactionEntry` + branch summary | 运行时视图压缩,**从不改写库**(SESSION_DESIGN 设计原则 3);compact.go + aging.go 双层,provider 锚定 token 计量 |
| Headless | json 事件流 + RPC 全等价 | **无**(非 TTY 模式已删除);`AgentEvent` 事件已结构化,具备序列化基础;`ask_clarification` 已有 autonomous 不阻塞模式(`pkg/commands/chat.go:212`)可复用 |
| 钩子/扩展 | 丰富事件面,tool_call 可阻断、tool_result 可修改 | 插件仅静态发现 skills/agents/commands/mcpServers(`pkg/claudeplugin/loader.go`),**无生命周期钩子**;工具执行有唯一入口 `Registry.Execute`(`pkg/tools/registry.go:280`,agent 侧调用点 `pkg/agent/toolexec.go:241`)——钩子有天然安放点 |
| 工具集 | 默认 4 个,`--tools` 白名单 | builtin 17 个(bash/文件系/web 系/docx 4 件套)+ task/skill/ask_clarification/git_auto_commit/present_file/memory/plan 工具 + MCP 发现,合计约 25+;注册表支持 `Clone/Restrict`(registry.go:115/337)但**无面向用户的精简 profile** |
| 模型注册 | models.json 热重载、cost 元数据、`!command` 密钥 | config.yaml `models[]`,支持 per-model BaseURL/APIKeyEnv/ContextWindow、密钥封存(pkg/secret);`/model`(repl.go:1066)仅在既有注册表内切换,**无热重载、无 cost 元数据** |
| 权限 | 无弹窗,靠容器化;项目信任门(trust.json) | 无弹窗,靠 sandbox(Landlock→bwrap→直连降级)+ plan mode 只读子集 —— **与 pi 哲学一致** |
| 上下文透明 | "不在 UI 之外注入任何东西" | memory 以尾部消息注入(`turnInjection`,react.go,保前缀稳定利于 prompt cache)、skill 目录注入系统提示;**无面向用户的上下文构成视图** |

## 四、采纳决策

### 采纳(构成第五节的实施阶段)

| # | 项 | 理由 |
|---|---|---|
| A1 | Headless print/JSON 模式 | 无 headless 意味着无法脚本化、无法接 evals(而 REVIEW_EVAL_DESIGN 正需要基准回路)。`AgentEvent` 已结构化,增量成本低。**排最前**:它同时是后续高风险改动的回归工具(见 §八 C1)。RPC 模式暂缓(见"不采纳")。 |
| A2 | Steering / Follow-up 双队列 + 非破坏性中断 | deepai 最大交互短板。运行中只能干等或 Ctrl+C 全杀;pi 证明"工具批间隙注入纠偏"是低成本高价值点。用户打的字任何路径下不丢失。 |
| A3 | 会话 fork(clone 式分支) | 补齐"从历史某点另开路线"的能力。采用 pi 的 UX(/fork 从某条 user 消息分叉)而非其存储模型:SQLite 下复制前缀到新会话即可,不违反"从不改写历史"原则。可选生成源会话摘要注入新会话(对应 pi 的 branch summary)。 |
| A4 | 工具钩子事件层(内部 Go 接口) | before_tool_call 可阻断 / after_tool_result 可修改,是把"git 检查点、审计日志、外部权限门"等未来需求移出内核的地基;deepai 的 review gate 也可在长期迁移到该层。首期只做进程内 Go 接口 + 内置钩子点,不做外部命令钩子。 |
| A5 | 上下文透明化(/context)+ 默认工具集 profile | `/context` 展示本次请求实际构成(系统提示各段、skill、memory 注入、消息、估算 token)——落实 pi "nothing behind your back"。工具 profile 同时削减工具 schema token(pi 上下文优势的另一半,见 §二.4)。 |
| A6 | 模型注册表小改:`models[]` 段热重载 + cost 元数据 + `!command` 密钥 | 低成本;`/model` 时**仅重读 config 的 `models[]` 段**消除"改模型配置必须重启"(范围收窄理由见 §八 C7);cost 元数据让 `/status` 能报花费。 |

### 部分采纳

- **极简系统提示**:不追求 <1k token(deepai 有 skill 目录、memory 等合理注入),但在 A5 中做一次**系统提示审计**,量化各段 token 占比,砍无效段。pi 的上下文优势来自系统提示 + 工具 schema 两处;deepai 的 aging 已解决第三处(工具结果膨胀),A5 的 profile 对应 schema 一处。
- **树状会话**:采纳 UX(fork),不采纳存储模型(JSONL 树)。SQLite + FTS5 + 运行时压缩是 deepai 的既定架构(SESSION_DESIGN),推倒重来收益不成比例。

### 不采纳(记录理由,避免反复)

| 项 | 理由 |
|---|---|
| 移除 MCP / plan mode / 内置子代理 | pi 的减法哲学成立于"扩展承载一切"的前提;deepai 的 MCP、plan mode、subagent 池已有真实使用且实现质量不差。上下文污染问题由 A5 的透明化与 profile 缓解。 |
| 权限弹窗系统 | 维持现状即是采纳 pi 观点:sandbox 隔离优于弹窗护栏。本文把这一现状**转正为设计决策**,不再视为"相对 Claude Code 的缺失"。 |
| RPC 模式 / SDK | A1 的 JSON 事件流先行;RPC 分帧协议等有嵌入需求(编辑器集成、会话服务器)再立项。 |
| 主屏差分渲染 | 放弃 bubbletea 自研渲染器代价过大;记为远期观察项(bubbletea 亦可探索非 alt-screen 模式)。 |
| TypeScript 式进程内扩展 | Go 无动态加载良途(.so 框架已删除,教训在案);扩展性走 A4 内部钩子 + 既有 claudeplugin 静态发现。 |

## 五、分阶段实施方案

> 顺序依据(§八 C1,措辞经 R1 修正):Phase 1 headless 提供的是**整体回归基线**(evals 可跑、行为可对照);steering 属 TUI/agent 交互路径,headless `-p` 默认不可达,其**确定性回归由 agent 层单测承担**(直接测 SteeringQueue 注入语义),Phase 2 另附 headless steer 入口提供集成覆盖(见 Phase 2)。排序仍然成立:先建测量,再动最高风险部件。

### Phase 1 — Headless JSON 模式(A1)

**改动面**:`pkg/commands`(新 flag)、chat.go 组合根抽取可复用 setup、新薄层 runner(不经 bubbletea)。

- `deepai -p "prompt"`(打印最终文本,退出码反映成败)与 `deepai --output-format json -p ...`(逐行输出 AgentEvent JSONL:turn/text/tool_call/tool_result/usage/end)。stdin 可接管道输入。
- **交互式能力的 headless 语义**:`-p` 隐含 autonomous 模式(`ask_clarification` 不阻塞,chat.go:212 机制已存在);plan 工具不注册;review gate 默认关闭(可 flag 开)。
- 会话照常落库(可 `--no-session` 关闭);严格分帧:每行一个 JSON 对象,LF 结尾,日志一律走 stderr。

**验收**:`echo "..." | deepai -p -` 在 CI 里可跑;JSON 事件流可被 `jq` 逐行解析;REVIEW_EVAL_DESIGN 的基准脚本能以此为入口;与 TUI 路径的行为对照**限定为不触发 `ask_clarification` 的 prompt 抽样**(R3:`-p` 隐含 autonomous、plan 工具不注册、review gate 默认关,三处是已知且有意的分歧点,对照时排除或标注)。

### Phase 2 — Steering / Follow-up 队列与非破坏性中断(A2)

**改动面**:`pkg/agent`(注入点)、`pkg/chat`(TUI 输入常驻 + 队列)。

- Agent 增加线程安全的 `SteeringQueue`(REPL 持有引用,构造时传入)。**注入点唯一**:每个工具批执行完成、下一次 LLM 调用之前,Run 检查队列,非空则把 steering 消息作为 user 消息追加进本轮 messages 再继续循环。禁止在流式接收中注入。单次使用契约不变——steering 是"向进行中的 Run 喂消息",不是复用 Agent。
- **无工具批时的语义**(§八 C2):若模型直接产出最终文本结束而队列非空,Run 正常结束,REPL 将未送达的 steering 消息立即作为下一轮输入自动发起——与 pi "deliver after the current assistant turn finishes" 语义等价。
- **持久化**(§八 C3):steering 消息随 Run 的 `result.Messages` 按注入位置自然落库(repl.go:806-809 已按序持久化新消息),不需要独立 AppendMessage 路径;需要的只是它出现在 messages 切片的正确位置。
- Follow-up 在 REPL 层实现:运行中以 Alt+Enter 提交则入 REPL 队列,turn 结束(含 steering 追加轮)后自动作为下一轮输入。
- TUI:`agentActive` 期间输入框**保持可见可输入**;Enter=steer,Alt+Enter=follow-up;Ctrl+C 中断时,未送达的 steering/follow-up 文本恢复到输入框。**开放问题**:是否引入 Escape 作为第二中断键——Esc 现已绑定"清除补全建议"(tui.go:591)与任务面板退出(tui.go:943),需评审时定夺键位方案。
- 与 auto-continue 的交互:恢复中断会话时队列必为空(队列不持久化,属会话内瞬态)。
- **headless steer 入口**(R1,pi RPC steer 的极简版):`-p` 模式下 stdin 首段为 prompt,其后每行作为一条 steering 消息入队。时序天然非确定,故它服务于集成/评测覆盖;确定性验证由下述单测承担。
- **UI 状态提示**(R6):`agentActive` 期间 Enter 语义从"发送"变为"steer",输入框须有可见状态变化(如前缀/占位符改为 `steer ›`),与 Esc 键位问题一并在评审会定夺具体形式。

**验收**,拆为两层(R2):
- *自动化(agent/REPL 层单测)*:① 队列非空时,steering 消息恰好注入在"工具批结果之后、下一次 LLM 调用之前"的位置,`result.Messages` 顺序与模型实际所见一致;② 无工具批即结束且队列非空时,REPL 自动以队列内容发起下一轮;③ 落库顺序与 `result.Messages` 一致(复用 repl.go:806-809 路径的回归测试);④ Ctrl+C 中断路径下队列内容完整返还(数据层断言)。
- *人工(TUI 键位与呈现)*:运行中 Enter=steer 生效且有状态提示;Alt+Enter=follow-up;Ctrl+C 后输入框内可见未送达文本。

### Phase 3 — 会话 fork(A3)

**改动面**:`pkg/chat/session.go`(store 方法)、REPL 命令。

- `/fork [seq|交互选择某条 user 消息]`:复制该点之前的全部消息到新会话(`metadata.forked_from` 记录源会话与分叉点,复用现有 metadata 列,无需迁移),切换过去;原会话不动。
- `deepai session fork <id> [--at seq]` 子命令对等。
- **memory 作用域**(§八 C5):不复制源会话的 per-session memory——memory 是派生数据,workdir 级 memory 天然共享;此取舍写入 MULTI_AGENT/SESSION 相关文档。
- 可选(评审时定):fork 时用 LLM 生成"源会话自分叉点以来的摘要"注入新会话首部,对应 pi 的 branch summary。

**验收**:从历史任一 user 消息分叉出新会话且原会话完好;`session list` 能看到派生关系。

### Phase 4 — 工具钩子事件层(A4)

**改动面**:`pkg/tools/registry.go`(`Execute` 是唯一执行入口,已核实:agent 侧唯一调用点 toolexec.go:241)、`pkg/agent` 装配。

- 定义 `ToolHook` 接口:`BeforeToolCall(ctx, call) (block bool, reason string)`、`AfterToolResult(ctx, call, result) (modified result)`;注册于 Registry,按注册序同步执行(pi 的"listeners are awaited sequentially"语义)。并行工具批中 `Execute` 在多 goroutine 并发调用,**钩子实现必须线程安全**,契约写入接口文档。
- 内置首批应用验证接口:审计日志钩子(JSONL 记录所有工具调用)、可选 git 检查点钩子(每次 write/edit 前自动快照,对应 pi 官方示例扩展)。
- 明确非目标:本阶段不暴露给外部插件、不做外部命令钩子。

**验收**:钩子可阻断一次 bash 调用并让模型收到可理解的拒绝消息;审计日志完整覆盖并行工具批(无丢失、无交错损坏)。

### Phase 5 — 上下文透明化与工具 profile(A5 + 部分采纳项 + A6)

- `/context`:打印本次请求组成分解(系统提示分段、skill 目录/已加载 skill、memory 注入、消息数、各部分 token 及占比)。token 为锚定估算值并如实标注(§八 C9)。
- **前置重构**(R4,已对码核实):系统提示当前是单一拼接字符串——`BuildSystemPrompt()`(promptbuild.go:50)组装基础提示/文件操作规则/工具建议/委派目录/plan 文本后即拼平,`AppendSystemPrompt`(react.go:309-317)再追加 skill 目录等。`/context` 分段展示需先把组装改为带标签分段(`[]promptSegment{name, text}`,请求时 join,展示时留结构),各追加点改为具名分段。此重构不改变最终请求字节(join 结果与现状逐字节相同,可断言),但属 Phase 5 的显性工作量。
- 系统提示审计:量化现有各段占比,提出删改清单(结论记入本文档修订)。
- 工具 profile:config.yaml 增加 `tools.profile: minimal|standard|full` 与组开关;**默认 `standard` = 现状全集,不改变任何既有用户的默认行为**(§八 C8),`minimal` 纯 opt-in。
- A6 顺带:`/model` 触发**仅 `models[]` 段**重读;`models[]` 增加可选 `cost` 与 `api_key_cmd` 字段。**cost 展示范围收窄**(R5,已对码核实:usage 仅有 TUI 内存中的单轮 `lastUsage`,tui.go:265,无任何持久化):本设计只做 ① 每轮 stats line 附本轮花费(由该轮 usage × cost 即时计算)、② REPL 进程内的会话累计(内存累加)。**跨重启的持久化累计花费不在范围内**——它需要 usage 落库与 schema 变更,如有需求单独立项。

**验收**:`/context` 的分段结构与实际请求一致(对照 pkg/proxy 调试日志抽样);minimal profile 下模型可见工具 ≤ 10 个;standard 下工具集与改动前逐一相同。

### 依赖与顺序

Phase 1 先行(回归工具);Phase 2 价值最高但风险最高,借 Phase 1 的 evals 兜底;Phase 3、4 互相独立可并行评审;Phase 5 收尾。每阶段完成后停在未提交状态等待评审,评审通过后按本文档节号提交。

## 六、风险

1. **Phase 2 的并发面**:向运行中的 Run 注入消息触碰 react.go 最复杂区域(工具批状态、compact 锚点、事件通道);注入点必须严格限定在"批间隙"单点。缓解:agent 层单测直接覆盖注入语义(Phase 2 验收自动化项),Phase 1 evals 提供整体回归;headless steer 入口补集成覆盖。
2. **headless 与 TUI 的装配分叉**:chat.go 组合根(~350 行)抽取时易引入行为漂移,需以现有 TUI 路径为准做对照测试。
3. **钩子性能与并发**:同步钩子在并行工具批中被并发调用,钩子自身的锁竞争可能拖慢批执行;首批内置钩子(审计、检查点)须做无锁或细锁实现并压测。
4. **fork 的摘要注入(可选项)**:LLM 生成摘要引入额外延迟与成本,且摘要质量影响新分支;默认关闭、显式 flag 开启。

## 七、修订日志

| 版本 | 日期 | 变更 |
|---|---|---|
| v1 | 2026-08-20 | 初稿:pi 调研摘要、现状对照、六项采纳决策、五阶段方案。 |
| v2 | 2026-08-20 | 第一性原理自审(§八):阶段重排(headless 提至首位);steering 补无工具批语义、落库风险反转;headless 补交互式工具语义;fork 补 memory 作用域决策;钩子安放点从推测转为已核实;A6 热重载范围收窄;profile 默认值定案;/context 验收放宽;pi 上下文优势归因修正;Esc 键位降为开放问题;工具计数给出依据。 |
| v3 | 2026-08-20 | 响应第 2 轮(用户)评审(§八 R1-R6):修正 Phase 1/2 依赖论证(headless 是整体基线,steering 确定性回归靠 agent 层单测)并为 Phase 2 增加 headless steer 入口;Phase 2 验收拆自动化/人工两层;Phase 1 对照验收排除 clarification 分歧;Phase 5 补提示组装分段化前置重构(已核实为拼接字符串);A6 cost 展示收窄为本轮 + 进程内累计,持久化累计出范围;Phase 2 补 steer 模式 UI 状态提示。 |

## 八、对抗式审查记录

### 第 1 轮(自审,2026-08-20)

| # | 发现 | 处置 |
|---|---|---|
| C1 | **阶段排序自相矛盾**:v1 把风险最高的 steering(触碰 react.go 并发面)排第一,而当时不存在任何自动回归手段;同文档又承认 headless 是 evals 的前提。第一性原理:先建测量,再动最危险部件。 | **采纳,重排**:headless 提为 Phase 1,steering 降为 Phase 2 并显式依赖前者的回归基线。 |
| C2 | **steering 语义缺口**:若模型无工具调用直接答完,"工具批间隙"注入点永不触发,steering 消息滞留队列——v1 未定义此路径。 | **补入 Phase 2**:Run 结束后 REPL 将未送达 steering 自动作为下一轮输入,与 pi 语义对齐。 |
| C3 | **v1 风险 2 被夸大(事实错误)**:v1 称 steering 落库需要"时序图"防 seq 乱序;实际 REPL 在 Run 返回后按 `result.Messages` 顺序统一持久化(repl.go:806-809),steering 只要 append 在 messages 正确位置即自然有序。 | **反转并简化**:删除该风险项,Phase 2 改为声明复用现有持久化路径。 |
| C4 | **headless 未定义交互式工具行为**:`ask_clarification` 在无人值守下会挂起。核实发现 autonomous 模式已存在(chat.go:212)。 | **补入 Phase 1**:`-p` 隐含 autonomous;plan 工具不注册;review gate 默认关。 |
| C5 | **fork 与 per-session memory 耦合未定义**:新会话拿不到源会话的 session 级 memory,是复制还是放弃? | **定案**:不复制(memory 为派生数据,workdir 级共享已覆盖),写入 Phase 3。 |
| C6 | **v1 钩子安放点是推测**:未核实 agent 是否统一经 Registry 执行工具。核实:`Registry.Execute`(registry.go:280)是唯一入口,agent 侧调用点 toolexec.go:241。 | **转正**:Phase 4 论据从推测升级为已核实事实,并补并发契约。 |
| C7 | **A6 热重载范围过宽**:整体重读 config 会使工具/skill/MCP 装配与运行中状态不一致(组合根只跑一次)。 | **收窄**:`/model` 仅重读 `models[]` 段。 |
| C8 | **工具 profile 默认值留白是隐患**:若默认改为 minimal,等于对既有用户的隐性破坏性变更。 | **定案**:默认 `standard` = 现状全集;minimal 纯 opt-in;验收加"standard 与改动前逐一相同"。 |
| C9 | **/context 验收标准过强**:"与请求实际字节一致"不可达——token 计量本身是锚定估算(react.go)。 | **放宽**:分段结构一致(对照 proxy 日志),token 标注为估算。 |
| C10 | **fork 优先级质疑**:使用频率未知,是否值得排进前三?反证:/undo 已存在且被用,fork 是同一需求("回到过去")的自然推广;且实现成本低(复用 metadata 列,session.go:46)。 | **维持**,但排序本已在 headless/steering 之后,无需调整。 |
| C11 | **pi "3x 上下文优势"归因不完整(事实错误)**:v1 归因于系统提示;实际工具 schema(尤其 MCP)同为大头,pi 自己举的例子就是 Playwright MCP 13.7k token。 | **修正 §二.4 与部分采纳节**;此归因同时加强 A5 profile 的价值论证。 |
| C12 | **Esc 键位冲突**:v1 直接写"Ctrl+C/Escape 中断恢复文本",未查现有绑定;Esc 已用于清除补全建议(tui.go:591)与任务面板(tui.go:943)。 | **降级为开放问题**:Ctrl+C 为确定方案,Esc 待评审定夺。 |
| C13 | **"约 25+ 工具"无依据**:v1 数字未经清点。清点:builtin 注册 17 个具名工具 + task/skill/ask_clarification/git_auto_commit/present_file/memory/plan 系 + MCP 动态发现。 | **补依据**于 §三对照表。 |

### 第 2 轮(用户评审,2026-08-20)

| # | 发现 | 处置 |
|---|---|---|
| R1 | **Phase 1/2 依赖论证有裂缝**:steering 是 TUI 交互路径,headless `-p` 不可达,"evals 兜住 Phase 2"不成立。 | **两个修法都采纳**:§五顺序依据改述为"headless = 整体回归基线,steering 确定性回归 = agent 层单测";同时 Phase 2 增加 headless steer 入口(stdin 后续行入队,pi RPC steer 极简版)提供集成覆盖,并明确其时序非确定、不承担确定性验证。 |
| R2 | **Phase 2 验收全为人工场景,无客观判据**。 | **拆两层**:自动化单测四项(注入位置、无工具批自动续轮、落库顺序、中断返还队列),TUI 键位与呈现留人工。 |
| R3 | **Phase 1 验收自相矛盾**:`-p` 隐含 autonomous,凡触发 `ask_clarification` 的 prompt 与 TUI 路径必然分叉。 | **限定对照范围**:排除 clarification prompt,并把三处有意分歧(autonomous、无 plan 工具、review gate 关)显式标注。 |
| R4 | **/context 分段展示的前置工作量未列**:若提示是拼接字符串需先重构。对码核实:确为单一字符串(BuildSystemPrompt promptbuild.go:50 + AppendSystemPrompt react.go:309-317 追加)。 | **补入 Phase 5**:组装改带标签分段的前置重构,附"join 结果与现状逐字节相同"的守恒断言。 |
| R5 | **A6 cost 越权**:/status 报累计花费需要 usage 跨轮累计与落库,不只是 config 字段。对码核实:usage 仅单轮内存值(tui.go:265),无持久化。 | **收窄**:只做本轮花费 + REPL 进程内累计;持久化累计出范围、需求出现时单独立项。 |
| R6 | **steer 模式下 Enter 语义切换无 UI 提示**,用户分不清当前 Enter 含义。 | **补充设计项与人工验收项**:agentActive 期间输入框前缀/占位符可见变化(具体形式与 Esc 键位一并评审会定夺)。 |
