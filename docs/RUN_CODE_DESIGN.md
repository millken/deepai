# run_code：脚本化工具编排 — 设计稿（供评审）

> 状态：**草案 v2，未实施**。v2 响应 PR #45 评审（High ×2、Medium ×6、§12 四问已定），见文末「修订记录」。日期 2026-11-16。
> 目标：新增一个 `run_code` 工具，让模型用一段 JavaScript 在一次工具调用里编排多次现有工具调用（检索 → 过滤 → 读取 → 修改 → 验证），**只把脚本的返回值送回上下文**。
> 不在本文范围：符号图 / callers-callees 索引（另立设计）、持久化 REPL 会话、多租户隔离。
> 背景材料：一份关于 Claude Code Code Mode 的外部讨论记录（未入库）。本文只把它当作问题线索，方案按 deepai 现有代码重新推导；两者的差异见 §11。

---

## 0. 结论（一句话）

注册一个 `run_code` 工具：脚本运行在 **QuickJS-ng（编译为 WASM，经 wazero 执行，零 CGO）** 里，全局对象 `tools` 是**调用方 agent 当前生效的工具集的镜像**——`await tools.grep({...})` 的参数就是 `grep` 自己的 schema，执行路径就是 `Registry.Execute`。脚本**没有任何额外能力**（无 fs / net / env / 子进程），能做的事与模型逐个调用工具完全相同，只是省掉了中间的模型往返和中间结果进上下文。

三条不变量：

1. **能力等价**：脚本可见的工具 = 调用时刻 agent 的 `a.tools`（含 plan mode / 子代理 allowlist 的限制）减去排除集（§4.3）。不引入新的权限概念。
2. **零新接口面**：不为脚本另起一套 `code.grep / code.read` 包装 API；工具名、参数、返回都直接复用现有定义，schema 不重复描述给模型。
3. **单次无状态**：每次 `run_code` 新建 JS context，跑完即弃；副作用全部记录在调用日志（journal）里随结果返回。

---

## 1. 问题

### 1.1 往返成本

典型的多步任务（「找出所有忽略 `Close()` 返回值的调用点，逐个看上下文，判断是否需要改」）在今天的 ReAct 循环里是：

```
grep → (模型) → read_file ×N → (模型) → 判断 → edit_file ×M → (模型) → bash go test → (模型)
```

每个 `(模型)` 都是一次完整请求：重发全部历史、等首 token、产出下一步调用。两类成本叠加：

| 成本 | 来源 | 现有缓解 |
|---|---|---|
| 时延 | 每步一次模型往返；串行 | 同一轮多个 `ParallelSafe` 调用可并发（`partitionToolCalls`，`pkg/agent/toolexec.go:705`），但**依赖前一步结果的调用无法并发** |
| 输入 token | 中间结果（grep 的几十条命中、整段 read）全部进历史，之后每轮重发 | offload（`toolexec.go:91`，超阈值落盘 + `recall_output`）、aging、compact |
| 输出 token | 模型逐条复述调用参数 | hashline 编辑减少 `old_string` 复述 |

现有缓解都作用在「结果已经进上下文之后」。`run_code` 处理的是更上游的问题：**模型只需要过滤后的结论时，中间结果就不该进上下文**。

### 1.2 适用与不适用

适用（模型能事先写出完整控制流的任务）：

- 检索后过滤：grep 出 80 处，只有满足某条件的 6 处值得看
- 同构批量改动：同一模式的 rename / 参数调整跨十几个文件
- 依赖链式查询：A 的结果决定查 B，B 决定读 C
- 改完立即验证：edit → `go build` / `go test`，失败信息才回传

不适用（这类任务应继续用原生工具）：

- 单步查询：一次 grep / 一次 read
- 每一步都需要模型判断的探索（读完才知道下一步往哪走）
- 需要中途问用户的任务

---

## 2. 现状约束（已对码核实）

设计依赖以下事实，任何一条变化都要回来改本文：

| # | 事实 | 位置 | 对设计的影响 |
|---|---|---|---|
| F1 | 工具接口是 `Handler(ctx, ToolCall) (ToolResult, error)`；`ToolResult` 有文本 `Content` 和结构化 `Data` | `pkg/models/message.go:212-238` | 脚本拿到的返回值 = `{text, data}`，无需解析文本 |
| F2 | 结构化结果的位置不统一：`grep` 在 `Data.matches`（Go 结构体切片 `[]grepMatch`，不是 map）；`bash` 的 JSON `{stdout, stderr, exit_code}` 在 `Content`、`Data` 为空；`glob` 的 JSON **数组**在 `Content`；`read_file` 的 `Data` 只有 `start_line`，hashline 前缀只在文本里 | `grep.go:158`、`bash.go:65`、`file.go:304`、`file.go:246` | 返回值转换必须对 `Data` 做 JSON 往返，并在 `Data` 为空时解析任意 JSON 值（§4.4） |
| F2b | `bash` **非零退出仍是 `completed`**，只有超时才置 `Status=failed` | `bash.go:66-72` | 「命令失败」不能映射为 reject，否则 `go test` 失败会被脚本当作成功（§4.4） |
| F2c | `edit_file` / `write_file` 的成功目标由 `recordEditedFile` 记入会话，mission 评审门按 `EditedFiles()` 取评审范围；该记账**只在 `toolBatchState` 层**做 | `session_carry.go:204`、`toolexec.go:471/613`、`mission_implement.go:294` | 嵌套调用不经过这一层，写入必须由 `run_code` 的结果转发（§4.5） |
| F2d | 顶层并发上限 `defaultMaxToolConcurrency = 10`，可由 `DEEPAI_MAX_TOOL_CONCURRENCY` 覆盖 | `react.go:45-58` | 脚本内复用同一限额，不另设常量（§5） |
| F3 | **没有逐调用审批层**；`pkg/hook` 只做通知分发与远程控制 | `pkg/hook/hook.go`、`control.go` | 不存在「脚本绕过审批」问题；但未来若加审批层，**必须加在 `Registry.Execute`**，而非 agent 的批处理层（§8 R4） |
| F4 | `bash` 走 `sandbox.ExecDirect`，即**无隔离**的 `sh -c` | `bash.go:32`、`sandbox.go:469` | `run_code` 不比今天更危险，也**不应宣称**提供隔离 |
| F5 | plan mode 通过**替换 `a.tools`** 为只读子集实现 | `plan.go:21`（`planToolNames`）、`plan.go:41-57` | 脚本必须在**调用时**取 `a.tools`，不能在注册时闭包捕获注册表，否则绕过 plan mode |
| F6 | `runOneTool` 在调用前往 ctx 注入 sandbox / threadID / readTracker / contextWindow / userInteraction | `toolexec.go:285-299` | 嵌套调用继承同一 ctx，read gate、hashline 校验对脚本内的 `edit_file` 照常生效 |
| F7 | `skill` / `todo_write` 的效果由 `toolBatchState.handleResult` 应用到 agent 状态，不是 handler 自身 | `toolexec.go:410-435` | 脚本内调用这两个工具结果会被**静默丢弃**——必须排除 |
| F8 | 子代理 usage 汇总（`addSubagentUsage`）与预算下传（`WithRemainingTokenBudget`）只在顶层调用路径上做 | `toolexec.go:27`、`registry.go:80` | `task` 必须排除，否则子代理 token 不计账 |
| F9 | 超阈值结果的 offload 在顶层 `handleResult` 里统一做 | `toolexec.go:439` | `run_code` 自身结果自动享受 offload，不需要自建 |
| F10 | 全仓零 CGO（`modernc.org/sqlite` 即为此选择），Makefile 用默认 `go build` | `go.mod`、`Makefile` | 排除 cgo 版 QuickJS / v8go |
| F11 | `code_map` 已提供文件级符号与签名（`depth=symbols/api`） | `codemap.go:590` | v1 不需要新建符号索引 |
| F12 | 工具注册点：CLI `pkg/commands/chat.go:414/440`；bot 服务 `cmd/deepai-bot/runtime.go:382` | — | 两处分别控制开关；bot 默认不开 |

---

## 3. 引擎选型

### 3.1 候选

| 方案 | 语法 | CGO | 隔离 | 中断 / 内存上限 | 结论 |
|---|---|---|---|---|---|
| **QuickJS-ng via `fastschema/qjs`（WASM + wazero）** | ES2023，原生 async/await | 无 | WASM 线性内存，天然无 fs/net（WASI 能力不配即无） | wazero 支持 ctx 取消关闭模块、限制内存页 | **推荐** |
| goja（纯 Go） | ES5.1 + 部分 ES6，async 支持有限 | 无 | 只暴露显式注入的对象 | `vm.Interrupt`；无内存硬上限 | 备选（spike 失败时） |
| QuickJS cgo 绑定 | ES2020+ | **有** | 同进程 C 堆 | 引用计数手动 Free，线程亲和 | 否：破坏 F10 |
| 子进程 node / deno | 完整 | 无（外部依赖） | 进程级 | 好 | 否：用户机器不一定有；冷启动大 |

选 QuickJS-ng/WASM 的理由排序：

1. **模型写对 JS 的概率**是第一指标。模型默认写 `const`/箭头函数/解构/可选链/`async-await`/`Promise.all`，goja 在 async 与部分新语法上的缺口会变成「脚本报错 → 模型重写」的额外往返，直接抵消收益。
2. 零 CGO，交叉编译与 `make install` 流程不变。
3. 隔离边界是 WASM 沙箱而不是「我们记得不注入什么」——默认就拿不到宿主任何东西。
4. 中断与内存上限由 wazero 在宿主侧强制，不依赖 JS 引擎配合。

### 3.2 抽象层

引擎藏在一个小接口后面，其余代码不感知：

```go
// pkg/runcode/engine.go
type HostCall func(ctx context.Context, tool string, args map[string]any) (HostResult, error)

type Engine interface {
	// Run 执行一段已包装的脚本。host 是唯一的出口；Run 返回脚本 return 值的 JSON。
	Run(ctx context.Context, src string, host HostCall, lim Limits) (RunOutput, error)
}
```

`fastschema/qjs` 实现为 `qjsEngine`，为**唯一引擎**（评审已定）。goja 只在 spike 不通过时作为另一份设计稿重写本文，不并行维护——双引擎意味着模型面对两套语法能力，提示词无法写准。

### 3.3 Spike 必须回答的问题（Phase 0 的 go/no-go）

| # | 问题 | 通过标准 |
|---|---|---|
| S1 | 冷启动：开启 wazero 编译缓存后，新建 runtime + context 的耗时 | p50 < 30 ms（相对一次模型往返可忽略） |
| S2 | 异步宿主函数：`SetAsyncFunc` 能否支撑 §5 的「排空-批量」调度（宿主拿到一批待决 promise，再统一 resolve） | 能写出 `Promise.all` 三路 grep 并发执行的用例；**且宿主函数只登记请求、立即把控制权交还 VM，工具在微任务队列排空之后才执行**——宿主函数内同步执行工具会把 VM goroutine 堵死，`Promise.all` 退化为串行 |
| S3 | 死循环 `while(true){}` 能否被 ctx 超时终止，宿主 goroutine 不泄漏 | 超时后 100 ms 内返回 |
| S4 | 内存上限：分配 1 GiB 字符串是否得到可捕获错误而非宿主 OOM | 限 256 MiB 时报错退出 |
| S5 | 二进制体积增量 | 记录数值（预期数 MB，接受） |

S2 不通过时的降级：宿主函数改为同步，额外提供 `tools.all([[name, args], ...])` 批量原语由 Go 侧并发——API 形状变了，提示词跟着变，但其余设计不动。

---

## 4. 工具契约

### 4.1 `run_code` 定义

```go
models.Tool{
	Name:   "run_code",
	Groups: []string{"builtin"},
	ParallelSafe: false, // 脚本可能含写操作，按最保守处理
	InputSchema: {
		"script":         string,  // 必填
		"timeout_sec":    number,  // 默认 120，上限 600
		"max_tool_calls": number,  // 默认 50，上限 200
	},
}
```

**描述（模型看到的全部说明，刻意写短）**：

> Run a JavaScript (ES2023) script that calls your other tools, and get back only what the script returns. Every tool you can call directly is available as `await tools.<name>(args)` with exactly the same arguments as its own schema. It resolves to `{status, text, data}`: `text` is what the tool would have shown you; `data` is its structured result as plain JSON, or null (grep → `data.matches[]` with `file/line/content/hash`; glob → array of paths; bash → `{stdout, stderr, exit_code}`).
> **A command that runs but fails does not throw**: check `data.exit_code` for bash. A call rejects only when the tool itself failed (bad arguments, file not found, timeout); the Error carries `.text` with the tool's output.
> To edit by hash after a search, use `data.matches[i].line` + `.hash` from grep. After `read_file`, hashes exist only in `text` as `N:hhhhhh` line prefixes — parse them from there.
> Use top-level `await` and `return`. Use `Promise.all` for independent read-only calls. `console.log` output is returned but capped at 8 KB. There is no fs, network, or require — only `tools`. Not available inside scripts: run_code, task, todo_write, skill, ask_clarification, present_file, plan-mode tools.
> Use this when a task needs several dependent tool calls whose intermediate results you do not need to see (search-then-filter, the same edit across many files, edit-then-test). For a single lookup, or when each step needs your judgment, call tools directly.

关键取舍：**不在描述里重复任何工具的 schema**。模型已经在工具列表里看到每个工具的定义，"同名同参"一句话即可。省 token，也杜绝镜像 API 与真实 schema 漂移。描述里只写 schema 表达不了的两件事：**命令失败不等于调用失败**（F2b），以及**hash 在哪个字段**（F2）——这两处写错，脚本会静默地走错分支。

### 4.2 脚本包装

宿主把脚本包成：

```js
(async () => {
<script>
})()
```

使顶层 `await` / `return` 可用。错误栈里的行号减 1 后再报告给模型。

### 4.3 可见工具集

```
visible = activeRegistry(ctx) − excluded
```

`activeRegistry` 由 `runOneTool` 在调用时注入 ctx（新增 `tools.WithDispatch` / `DispatchFromContext`，与 `WithReadTracker` 同一模式），注入值为 `tools.Dispatch{Registry: a.tools, MaxConcurrency: a.maxToolConcurrency}`——并发上限与顶层同源（F2d）。这保证：

- 子代理的 `tools:` allowlist 自动生效（`yaml_loader.go:200` 的只读默认集照常限制）
- plan mode 下 `a.tools` 已是只读集，若将来把 `run_code` 放进 `planToolNames`，脚本自然只读（§9 Phase 3）
- `run_code` 本身受 allowlist 控制：不在某个 agent 类型 `tools:` 里，就不可用

**排除集**及理由（原则：效果由 agent 循环而非 handler 落地的工具，或需要人参与的工具）：

| 工具 | 理由 |
|---|---|
| `run_code` | 禁止递归 |
| `task`、`invoke_acp_agent` | F8：usage / 预算只在顶层计账；且子代理时长不受脚本超时约束 |
| `todo_write`、`skill` | F7：结果需 `handleResult` 应用，嵌套调用会被静默丢弃 |
| `enter_plan_mode`、`exit_plan_mode`、`write_plan` | 脚本执行中切换 `a.tools` 会让后续调用的能力集不确定 |
| `ask_clarification` | 脚本不应阻塞等人；需要提问就 return，让模型问 |
| `present_file` | 只注册 UI 产物；脚本结束后模型再调一次即可。Phase 2 有 `ParentID` 之前，脚本中途的展示没有落点（评审已定：v1 排除） |

排除集定义为一个常量切片。判定标准不是「`handleResult` 的 switch 里有没有它」，而是「**效果不在 handler 内、而在 `toolBatchState` 里落地**的工具」：每个这样的工具要么排除，要么在 §4.5 写明转发哪一段副作用。测试见 §10。

### 4.4 宿主调用路径

```
JS: await tools.grep({pattern: "..."})
 → HostCall("grep", args)
   1. name ∈ visible ? 否 → reject ToolError("tool not available in run_code: grep")
   2. calls++ > max_tool_calls ? → reject，且之后所有调用都 reject
   3. 只登记请求并把控制权交还 VM（S2）；真正执行发生在排空之后的批次里（§5）
   4. 执行前先写 journal 条目 status=running
   5. registry.Execute(scriptCtx, ToolCall{ID: "<runcodeID>.<n>", Name, Arguments})   // 含 schema 校验
      scriptCtx 继承 run_code 的 ctx（sandbox/readTracker/threadID/window 全部沿用），带脚本预算的 deadline（§6.2）
   6. 更新 journal 条目为最终状态；转换返回值 → resolve / reject
```

返回值转换：

```
data = jsonRoundTrip(result.Data)            Data 非空时：json.Marshal → json.Unmarshal 成 any，
                                             去掉 Go 类型（[]grepMatch → [{file,line,content,hash}]）
                                             并丢弃宿主内部标记键（models.ToolDataOffloaded / ToolDataNoOffload）
     ∨ json.Unmarshal(result.Content) 成 any  Data 为空且 Content 是任意合法 JSON 值时（bash 对象、glob 数组）
     ∨ null

Execute 返回 err，或 result.Status == failed：
    reject(ToolError{tool, message: result.Error || err, text: result.Content, data})
否则（含 bash 非零退出）：
    resolve({status: result.Status, text: result.Content, data})
```

「工具失败」与「命令失败」严格分开：`reject` 只来自工具层的失败（F2b），这样 `const r = await tools.bash({command: "go test ./..."}); if (r.data.exit_code !== 0) return {failed: r.data.stderr}` 才是脚本的惯用写法，§1.2「改完立即验证、失败信息才回传」才成立。`reject` 时保留 `text`，模型拿到的错误与原生调用时看到的一致。

按 F6，脚本内 `edit_file` 仍受 read gate 约束：文件在本会话里没读过（或读后被改过）照样报 `StaleReadError`。脚本作者可以先 `await tools.read_file(...)` 再改——这与原生路径的要求一致，不开后门。

### 4.5 顶层副作用的转发

嵌套调用**只走 `Registry.Execute`，不进 `toolBatchState.handleResult`**：后者会对每个嵌套结果做 offload、写入消息历史、发 UI 事件，正是 `run_code` 要避免的。但 `handleResult` 里有些副作用不是「展示」而是「记账」，丢了会让下游判断出错。逐项处理：

| `toolBatchState` 中的副作用 | 触发工具 | 处理 |
|---|---|---|
| `applySkillResult` / `applyTodoResult`（`toolexec.go:432-435`） | `skill`、`todo_write` | 排除（§4.3） |
| `addSubagentUsage`（`toolexec.go:27`） | `task`、`invoke_acp_agent` | 排除（§4.3） |
| `recordEditedFile`（`toolexec.go:471/613`）→ mission 评审范围 | `edit_file`、`write_file` | **转发**：见下 |
| offload、消息历史、metrics、UI 事件 | 全部 | 不对嵌套结果做；只对 `run_code` 自身结果做（F9）；嵌套调用的度量走 §7.3 |

转发 `recordEditedFile`：`run_code` 结果的 `Data["edited_paths"]` 列出脚本内 `edit_file` / `write_file` 中状态为 **completed 或 interrupted** 的目标路径（failed 的编辑没有产生改动，与 `recordEditedFile` 现行规则一致；interrupted 可能已落盘，宁可多评不可漏评）。`recordEditedFile` 增加一个分支：`result.ToolName == "run_code"` 时遍历 `edited_paths` 逐个 `RecordEditedFile`，**不看 `run_code` 自身的 Status**——脚本 failed 时前面的写入同样已落盘，同样要进评审。

已知盲区与顶层一致：通过 `bash`（`sed -i`、`go generate`）产生的改动今天也不进 `EditedFiles()`，本设计不扩大也不修复它；`writes:` 摘要（§6.3）会把这类调用列出来。

---

## 5. 调度：排空-批量

QuickJS context 只能被一个 goroutine 驱动。并发靠「批量」而不是「多线程进 VM」：

```
loop:
  运行 JS 微任务直到队列空            // 脚本推进到所有分支都在 await 宿主调用
  pending = 本轮新发起的宿主调用（按发起顺序）
  if pending 为空: 脚本结束 → break
  把 pending 切段：连续的 ParallelSafe 调用为一段并发执行（上限 = Dispatch.MaxConcurrency，与顶层同一限额），非 ParallelSafe 调用单独成段串行
  按段执行，收齐结果后按原顺序 resolve/reject
```

切段规则与顶层 `partitionToolCalls`（`toolexec.go:705`）**相同**。模型在顶层与脚本内面对同一套并发语义：只读调用可并发，写调用按发起顺序串行、彼此可见。

```js
// 三路 grep 在同一批里并发；edit 按顺序串行
const [a, b, c] = await Promise.all([
  tools.grep({pattern: "Close\\(\\)$", type: "go"}),
  tools.grep({pattern: "defer .*Close\\(\\)", type: "go"}),
  tools.grep({pattern: "_ = .*Close\\(\\)", type: "go"}),
]);
```

---

## 6. 限制与失败语义

### 6.1 限额

| 维度 | 默认 | 上限 | 强制方 |
|---|---|---|---|
| 墙钟 | 120 s | 600 s | `scriptCtx` deadline → wazero 关闭模块（§6.2） |
| 工具调用数 | 50 | 200 | HostCall 计数 |
| 单批并发 | 10 | `DEEPAI_MAX_TOOL_CONCURRENCY` | 调度器，复用顶层限额（F2d） |
| JS 内存 | 256 MiB | — | wazero 内存页上限 |
| console 输出 | 8 KiB | — | 超出截断，标注 `[log truncated]` |
| return 值 | 1 MiB 硬上限 | — | 超出报错；1 MiB 以内但超 offload 阈值的走现有 offload（F9） |

嵌套工具各自的输出上限（bash 50 KB、grep `max_results`）照常生效，脚本拿到的就是这些已截断的结果。

### 6.2 失败分类

| 情况 | `run_code` 结果 | 给模型的内容 |
|---|---|---|
| 正常 return | completed | return 值 + logs + 调用摘要 |
| 工具 reject 且被 catch | completed | 同上（是否算失败由脚本决定） |
| 未捕获异常 / 语法错误 | **failed** | 错误 + 行号 + 已完成调用的 journal（**含已落盘的写操作**） |
| 脚本预算超时 / 调用数超限 / 内存超限 | **failed** | 原因 + journal |
| 父 ctx 被取消（用户 Ctrl-C、轮次中断） | **cancelled** | 沿用顶层取消路径；journal 仍随结果返回 |

**超时与取消分开判定**。handler 内建子 ctx：

```go
scriptCtx, cancel := context.WithTimeout(parentCtx, budget)
defer cancel()
// 嵌套工具与 wazero 模块都用 scriptCtx
...
switch {
case parentCtx.Err() != nil:            // 用户/上层取消 → cancelled
case errors.Is(scriptCtx.Err(), context.DeadlineExceeded): // 脚本预算耗尽 → failed + journal
}
```

先检查父 ctx：两者同时到期时按「取消」处理，不把用户中断误报为脚本超时。

**部分副作用必须显式告知**。journal 条目在 `Execute` **开始前**就以 `running` 写入（§4.4 第 4 步），结束时更新为最终状态；脚本因超时、取消或异常终止时，所有仍是 `running` 的条目标为 **`interrupted`**。`writes:` 摘要列出所有**已开始执行**的非 ParallelSafe 调用及其状态（completed / failed / interrupted）——超时被杀的 `bash` 与执行中被中断的 `edit_file` 可能已经落盘，必须出现在这里。模型据此判断当前磁盘状态，不会误以为「脚本失败 = 什么都没发生」。

### 6.3 输出格式

`Content`（进上下文）：

```
run_code ok — 9 tool calls in 2.4s (grep×3, read_file×4, edit_file×1, bash×1)
writes: edit_file pkg/tools/builtin/grep.go [completed]; bash "go test ./pkg/tools/..." [completed]
result:
{"fixed": 1, "skipped": [{"file": "...", "line": 88, "why": "..."}], "tests": "ok"}
logs:
checked 14 candidates
```

`Data`（不进上下文，供 UI / 指标）：

```go
{
  "script":  "<原文>",
  "journal": [{"seq":1,"tool":"grep","args_preview":"…≤200B","status":"completed","ms":41,"bytes":3120,"write":false}, …],
                                   // status ∈ running | completed | failed | interrupted
  "edited_paths": ["/abs/pkg/tools/builtin/grep.go"],   // §4.5，顶层 recordEditedFile 消费
  "return_bytes": 412,
  "log_bytes": 22,
}
```

---

## 7. 启用与可见性

### 7.1 开关

- 环境变量 `DEEPAI_RUN_CODE=1`（遵循 `config_env.go` 现有 `DEEPAI_*` 约定）或配置项 `run_code: true`，**默认关**
- 启用后在 `pkg/commands/chat.go` 注册；**bot 服务（`cmd/deepai-bot/runtime.go`）不注册**，直到单独评估多租户场景
- **首批只给 CLI 主代理**（评审已定）。任何子代理类型的 `tools:` 默认都不含 `run_code`。reviewer 类代理不进首批：它的工作是逐步判断并引用 `file:line`，属于 §1.2 的「不适用」类；是否给其他类型开放，留到 Phase 3 按 eval 结果单独决定

### 7.2 用户可见性

脚本里做的写操作不能只藏在 Data 里。分两步：

- **Phase 1**：TUI 渲染 `run_code` 时展示脚本前几行 + 调用摘要行（`writes:` 一行始终可见）；完整脚本与 journal 可展开查看
- **Phase 2**：嵌套调用作为 `run_code` 的子事件发出（`ToolCallEvent` 增加 `ParentID`），TUI 缩进显示，与子代理可见性（`SUBAGENT_VISIBILITY_DESIGN.md`）同一套展示层

### 7.3 指标

开启 `DEEPAI_TOKEN_METRICS` 时，每次 `run_code` 记录：脚本长度、嵌套调用数与分布、墙钟、return 字节数、**嵌套结果总字节数**（= 没进上下文的字节数，是本功能收益的直接度量）。

---

## 8. 风险

| # | 风险 | 缓解 |
|---|---|---|
| R1 | 模型对单步查询也用 `run_code`，多一层包装反而更慢 | 描述明确劝阻；指标看「嵌套调用数 = 1」的占比，过高就收紧描述 |
| R2 | 脚本写错，报错 → 重写 → 再报错 | 错误带准确行号与 journal；失败两次后描述提示回退原生工具；ES2023 降低语法错误率（§3.1） |
| R3 | 批量写操作一次落多个文件，用户事后才看到 | 可见性**只靠** `writes:` 摘要（§6.3、§7.2）始终可见；mission 评审门经 `edited_paths` 覆盖脚本内的文件编辑（§4.5）。没有其他兜底：`git_auto_commit` 是模型按需另行调用的工具，coder 提示词明确要求不自动提交（`types_config.go:80`），不能算作保护 |
| R4 | 未来加入逐调用审批 / PreToolUse 钩子，或新的结果记账（同 `recordEditedFile` 一类）时，若挂在 `toolBatchState` 层，脚本路径会绕过 | **设计约束写入本文**：逐调用的策略层挂在 `Registry.Execute`（或 `runOneTool` 注入的统一执行器）上，顶层与嵌套共用；新增的 `toolBatchState` 级副作用必须同时更新 §4.5 的表，§10 的回归测试会因此失败提醒 |
| R5 | `fastschema/qjs` 是第三方库，维护或兼容性有风险 | 引擎藏在 `Engine` 接口后（§3.2）；spike 记录 S1–S5 基线，升级时复跑 |
| R6 | 嵌套调用不经过顶层的 breaker（`toolCallBreaker`），脚本里的失败循环不被熔断 | 调用数上限 + 墙钟兜底；脚本是一次性的，不存在跨轮的失败累积 |
| R7 | 嵌套结果不经 aging / offload，大结果驻留 JS 堆 | 内存上限 256 MiB；工具自身已有输出上限 |

---

## 9. 分期

**Phase 0 — 引擎 spike（go/no-go）**
在 `pkg/runcode` 下用 `fastschema/qjs` 跑通 §3.3 的 S1–S5，结论与数值写回本文。不通过则按 §3.3 降级或换 goja，并修订 §4.1 描述。

**Phase 1 — MVP**
- `pkg/runcode`：Engine、调度器（§5）、限额（§6.1）、journal、返回值转换
- `pkg/tools`：`Dispatch` / `WithDispatch` / `DispatchFromContext`；`runOneTool` 注入
- `run_code` 工具定义与排除集；`DEEPAI_RUN_CODE` 开关；CLI 主代理注册
- `recordEditedFile` 增加 `run_code` 分支，消费 `edited_paths`（§4.5）
- TUI：脚本预览 + 调用摘要 + `writes:` 行

**Phase 2 — 可见性与度量**
嵌套子事件（`ParentID`）；§7.3 指标；会话记录里保存脚本原文便于复盘。

**Phase 3 — 评估与扩展**
- 在 `eval/` 选一组任务，A/B 对比开关：完成率、模型轮数、输入 token、墙钟。任务集**必须覆盖非快乐路径**：
  - 快乐路径：批量 rename、检索后过滤、改完跑测试
  - **写到一半失败**：脚本在第 k 次写入后失败，下一轮模型须根据 journal / `writes:` 正确恢复（不重复已完成的写，不遗漏未完成的）
  - **结论必须带 `file:line`**：检查模型是否因中间结果不在上下文而丢失可引用的位置
- 默认开启的判据（评审已定，**三条同时满足**）：完成率不降；输入 token 下降 ≥ 25%；模型轮数不升
- 按结果再单独决定：其他代理类型是否带 `run_code`；放进 `planToolNames`（只读集自动成立）；是否需要跨调用持久状态

---

## 10. 测试

| 层 | 用例 |
|---|---|
| 能力等价 | 受限注册表下调用未授权工具 → reject；plan mode 注册表下 `edit_file` 不可见；排除集每项都 reject |
| 调用路径 | 嵌套 `edit_file` 未读文件 → StaleReadError；嵌套调用 ID 带父前缀；schema 校验错误原样回传 |
| 返回值契约 | `bash` 退出码 1 → resolve，`data.exit_code === 1`、`status === "completed"`；`bash` 超时 → reject，`.text` 非空；`grep` → `data.matches[0]` 是含 `file/line/content/hash` 的普通对象；`glob` → `data` 是数组；`read_file` → `data.start_line` 存在且 `text` 含 `N:hhhhhh` 前缀；内部标记键不出现在 `data` |
| 副作用转发 | 脚本内成功的 `edit_file` / `write_file` 出现在 `EditedFiles()`；脚本在写入后抛异常（failed）时同样出现；被超时打断的写入（interrupted）出现；failed 的 `edit_file` 不出现 |
| 调度 | `Promise.all` 三个只读调用同批并发（计时 / 计数断言）；两个写调用按发起顺序执行，第二个能看到第一个的效果；读写混合时切段与 `partitionToolCalls` 一致（同一输入表驱动两边） |
| 限额 | 死循环超时；调用数超限后后续调用全部 reject；内存超限；log 截断；return 超 1 MiB 报错；并发上限随 `DEEPAI_MAX_TOOL_CONCURRENCY` 变化 |
| 失败语义 | 第 3 次调用后抛异常 → failed，journal 含前两次写操作；脚本预算到期时正在跑的 `bash` → journal 为 interrupted 且出现在 `writes:`；父 ctx 取消 → cancelled（不是 failed）；父子同时到期 → cancelled |
| 输出 | Content 格式 golden；大 return 走 offload 后 `recall_output` 可取回 |
| 回归防护 | 表驱动列出「效果在 `toolBatchState` 而非 handler 内落地」的工具及其副作用（`skill`/`todo_write` → apply*Result；`task`/`invoke_acp_agent` → `addSubagentUsage`；`edit_file`/`write_file` → `recordEditedFile`）。断言每项要么在排除集，要么在 §4.5 的转发表里。表本身维护在 `toolBatchState` 旁边，新增分支不更新表即测试失败 |

---

## 11. 与讨论稿的差异

讨论稿给出了方向，但多处建立在对 deepai 的错误假设上。本文的取舍：

| 讨论稿 | 本文 | 原因 |
|---|---|---|
| 另造 `code.grep / code.read / code.diff …` 包装 API | 直接镜像现有工具（`tools.<name>`，同名同参） | 不重复 schema；不漂移；子代理 allowlist 自动生效 |
| 经 `internal/agent/approval` 审批门 | 不存在该层；以「能力等价」为边界，并约束未来策略层的挂载点（R4） | F3 |
| `code.exec` 走 `pkg/sandbox` 的 bwrap 隔离 | 嵌套 `bash` 与顶层相同（`ExecDirect`），不宣称隔离 | F4 |
| goja，或 cgo 版 QuickJS + 进程内/进程外双模 | QuickJS-ng on wazero，单一引擎 | F10；双引擎让提示词写不准 |
| 自建 `code.parallel` 并发原语 | 原生 `Promise.all` + 排空-批量调度，并发语义与顶层一致 | 模型已会写；不引入第二套语义 |
| run_code 内自建 64 KiB offload | 复用顶层 offload | F9 |
| 同期引入 tree-sitter 符号库与 callers/callees | 不在本文；v1 用现有 `code_map` | F11；两件事可独立评估收益 |
| 新增 `tool_presentation: native\|code\|both` 配置 | 不加；`run_code` 只是一个工具名，用现有 `tools:` allowlist 控制 | 少一个概念 |

---

## 12. 已定决策（PR #45 评审）

1. **引擎**：QuickJS-ng/WASM（`fastschema/qjs`）为唯一引擎。S2 增加「宿主函数交还控制权、排空后执行」的通过标准（§3.3）。goja 只作为 spike 失败后的另稿，不并行维护。
2. **首批范围**：只给 CLI 主代理，默认关。reviewer 不进首批（§7.1）。
3. **`present_file`**：v1 继续排除（§4.3）。
4. **默认开启判据**：三条同时满足——完成率不降、输入 token 降 ≥ 25%、轮数不升；eval 含「写到一半失败后恢复」与「结论带 `file:line`」两类（§9）。

不在本文范围（评审确认）：符号索引、bot 多租户、持久 REPL、`fastschema/qjs` 的供应链细节。S1–S5 的数值待 spike 填写。

---

## 修订记录

- v1（2026-11-16）：初稿。
- v2（2026-11-16）：响应 PR #45 评审。
  - **H1 返回值契约**：§2 补 F2/F2b（结构化结果位置不统一、bash 非零退出仍为 completed）；§4.1 描述改为 `{status, text, data}`，写明「命令失败不 reject、看 `data.exit_code`」；§4.4 对 `Data` 做 JSON 往返，`Data` 为空时解析任意 JSON 值，reject 仅限工具层失败并保留 `text`。
  - **H2 评审门漏账**：§2 补 F2c；新增 §4.5，嵌套调用不进 `handleResult`，由 `run_code` 结果的 `edited_paths` 转发给 `recordEditedFile`，脚本 failed 时同样记账；§6.2 journal 在执行前记 `running`，终止时标 `interrupted` 并列入 `writes:`。
  - M1 取消：§6.2 用 `WithTimeout` 子 ctx 区分「脚本超时 → failed」与「父 ctx 取消 → cancelled」。
  - M2 排除集测试：§4.3 / §10 判据改为「效果在 `toolBatchState` 落地的工具」。
  - M3 R3：去掉 `git_auto_commit` 兜底的说法。
  - M4 hashline：§4.1 写清 grep 的 hash 在 `data.matches[].hash`，`read_file` 的 hash 只在文本前缀。
  - M5 并发：§5 / §6.1 复用 `maxToolConcurrency`（默认 10，`DEEPAI_MAX_TOOL_CONCURRENCY`）。
  - M6 范围：§7.1 去掉 reviewer 首批候选。
  - §12 四问改为已定决策。
