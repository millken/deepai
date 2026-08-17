# 对抗式自审评测基线(Review Eval Baseline)设计文档

> 用植入已知 bug 的 diff 语料离线评测 `correctness-reviewer`,产出**检出率 / 假阳性率 / 场景合规率 / 写树违规率 / 成本**五组可复现数字。它是 `ADVERSARIAL_REVIEW_DESIGN.md` §八-1(翻默认开)与 §七-1(多审投票阈值)明确声明的前置条件,也是此后每次 reviewer 提示词迭代的回归标尺。
>
> **核心原则**:评测输入与生产输入**同源**(同一套 diff/prompt 组装代码,不是"长得像");ground truth 对 reviewer **物理不可达**(不在它能读到的任何路径上);裁定有**人工复核通道**(自动匹配解决不了"报了个真 bug 但不是植入的那个")。
>
> **版本**:v1(草案,供 review)。

---

## 一、目标、指标与非目标

### 1.1 要回答的三个问题

1. **能不能翻默认开**(§八-1):当前 reviewer 对典型正确性 bug 的检出率、对正确代码的误伤率,是否好到值得让所有编辑轮默认多等一次审查?
2. **提示词改动是变好还是变坏**:规则 2(无场景不算数)/规则 4(构造不出场景必须 pass)的措辞迭代,需要回归数字而非感觉。
3. **投票值不值得**(§七-1):单审的逐 case 稳定度(3 次重复里命中几次)直接决定多数裁决能榨出多少增益。

### 1.2 指标定义

| 指标 | 定义 | 对应生产语义 |
|---|---|---|
| **检出率**(主指标,逐 bug) | 植入 bug 被命中的比例:verdict=fail 且 ≥1 issue 命中该 bug(命中规则见 §四);按 bug 类别分桶 | gate 拦下坏改动的能力 |
| **case 级检出**(辅) | bug case 中 "verdict=fail 且 ≥1 命中" 的比例 | 一次审查至少抓到点什么的概率 |
| **假阳性率** | clean case 中被判 fail(按生产的 `isPassVerdict` 同款判定)的比例 | 好改动被无谓打回修复轮的概率 |
| **场景合规率** | 全部所报 issue 中 `scenario` 非空的比例 | 提示词规则 2 的服从度 |
| **写树违规率** | 审查期间 case worktree 变脏的 case 比例(复用生产快照防线的判定) | 提示词规则 3 的服从度;生产端此时裁决会被丢弃 |
| **成本** | 每次审查 token 用量(task 结果 `Data["subagent_usage"]`)与墙钟耗时 | 翻默认开的代价面 |

**稳定性**:每 case 重复 `--runs` 次(默认 3),逐 case 报告命中稳定度(3/3、2/3、1/3)。稳定度分布是 §七-1 投票收益的直接输入:大量 2/3 case 意味着 3 审多数裁决增益显著;全是 3/3 或 0/3 则投票只是烧钱。

### 1.3 非目标

- **不进 CI**:真实 LLM 成本 + 非确定性,按需手动跑。
- **不是通用 benchmark**:语料贴着本项目的 reviewer 提示词 focus 清单设计,不追求跨工具可比。
- **本期只评 `correctness-reviewer` 单审**:harness 预留 `--agent-type` 与 `--voters` 参数位给跨类型/投票扩展,不实现(§六)。

---

## 二、语料设计

### 2.1 case 格式

```
eval/review-cases/
  <case-id>/
    manifest.yaml     # ground truth,永不进入 reviewer 可达路径
    base/             # 变更前的完整文件树(将被 commit 为基线)
    change/           # 变更后的文件覆盖(含新文件;整树覆盖语义)
```

```yaml
# manifest.yaml
id: offbyone-slice-window
task: "实现滑动窗口平均值函数"        # 作为 reviewer 的"原始需求"锚点
expect: fail                          # fail | pass
bugs:                                 # expect: pass 时为空
  - file: window.go
    lines: [23, 27]                   # 命中判定的行区间
    class: off-by-one                 # 仅用于分桶,不参与自动匹配
    note: "右边界少减一,窗口末元素重复计入"
deletes: []                           # change 相对 base 要删除的文件(可选)
tags: [go, has-test]
adjudications: []                     # 人工复核结论回写处(见 §四)
```

**change 用 after-tree 覆盖而非 patch 文件**:runner 反正要在临时 git 仓库里算 diff(与生产同一条 `git diff` 路径),patch 应用是多余的脆弱环节;删除文件用 `deletes` 列表显式声明。

### 2.2 bug 类别 —— 与 reviewer 提示词 focus 清单一一对应

语料类别**照抄** `correctnessReviewerSystemPrompt` 的 Focus 行,保证"考纲即教纲":

| 类别 | 首期数量 | 示例形态 |
|---|---|---|
| `logic` | 4 | 条件取反、错误的短路、状态机漏迁移 |
| `edge-case` | 4 | nil/空集合/零值/边界长度未处理 |
| `off-by-one` | 4 | 区间端点、循环边界、切片窗口 |
| `error-path` | 4 | 吞错、错误分支资源未释放、err 变量遮蔽 |
| `concurrency` | 4 | 无锁共享写、WaitGroup 计数错、channel 泄漏 |
| `task-mismatch` | 4 | 代码本身自洽但没做需求要的事(如需求要"去重后排序",实现只排序) |
| **clean**(期望 pass) | 8 | 见 §2.3 |

首期 **24 bug case + 8 clean case = 32**。语言首期 **Go-only**(项目自身生态;reviewer 可 `go build`/`go test`),约半数 case 带可运行测试(`has-test` 标签),给规则 3 的 bash 验证路径留发挥空间——检出率可按 has-test 分桶,量化"能跑测试"值多少检出。

### 2.3 clean case 的对抗性设计

假阳性率的可信度取决于 clean case 是否"看起来可疑但正确"。全是平凡正确代码,FP 率必然虚低。要求每个 clean case 至少含一个**诱饵**:

- 有意为之的非对称边界(`<` 与 `<=` 混用但正确);
- 看似漏掉、实为上游已保证的 nil 检查(base 树中有调用方约束注释);
- 非显然但正确的并发(不可变数据的无锁共享读)。

诱饵直接对标提示词规则 4:"构造不出失败场景必须 pass,不得因风格或口味 fail"。

### 2.4 防污染(ground truth 不可达)

runner 每 case 的物化流程:`mktemp`(在项目仓库**之外**)→ `git init` → 拷入 `base/` → commit → 覆盖 `change/` 文件、执行 `deletes`(全部留在工作区,不 commit)。**manifest.yaml 从不进入临时仓库**;reviewer 的 bash cwd 是临时仓库根,`read_file`/`grep` 也够不到语料目录。语料在 deepai 仓库里公开存放不构成泄漏——除非有朝一日模型训练数据包含本仓库,届时语料需要轮换,这一风险记录在案即可。

---

## 三、执行器设计

### 3.1 入口与组装

新 cobra 子命令(沿用 `topLevel.AddCommand` 惯例):

```
deepai eval review [--cases eval/review-cases] [--runs 3] [--filter tag]
                   [--out eval/results] [--model alias] [--budget N] [--timeout Nm]
```

子代理栈**照抄** `registerChatTools` 的组装(`pkg/commands/chat.go:357-369`):`NewSubagentExecutor(modelRegistry, registry, nil)` + `NewSubagentPool(subExecutor, 0)` + `tools.TaskTool` —— 与 REPL 同一条派发链,schema 校验重试、usage 回传全部一致。eval 的 registry 只注册 reviewer 工具集所需(只读文件工具 + bash)+ task,不带 web/git 写类工具。budget/timeout 缺省取生产默认(30k / 5m),保证测的是生产形态。

### 3.2 两条硬约束(实施前已核实)

1. **cwd 逐 case 切换,顺序执行**:bash 走 `sandbox.ExecDirect` = 裸 `exec.CommandContext`,**不设 `cmd.Dir`**,cwd 即进程 cwd;`SubagentExecutor.WithWorkDir` 只影响 agent 类型解析(`subagent.go:89`),不影响工具 cwd。因此 runner 必须在派发前 `os.Chdir(caseWorktree)`、结束后切回,case 之间**串行**——这也顺带匹配了"逐 case 快照测写树违规"的需要,并发下无法归因(与 ADVERSARIAL_REVIEW_DESIGN.md §七-1 记录的同一约束)。
2. **组装同源**:diff/prompt 组装必须调用生产代码,不得复制。`buildReviewDiff` / `buildReviewPrompt` / `takeWorktreeSnapshot` / 降级阶梯常量目前是 `pkg/chat` 未导出符号 → 处置见决策点 1(倾向:原地导出,最小改动;抽 `pkg/review` 包留给确有第三个消费者时)。

### 3.3 单 case 流程

```
物化临时仓库(§2.4)
→ S0 = takeWorktreeSnapshot          # 此处全部 dirty 文件即 scope(评测里改动全是"agent 的")
→ diff = BuildReviewDiff(scope)       # 同生产:范围限定 + --no-index + 降级阶梯
→ prompt = BuildReviewPrompt(manifest.task, diff, ...)
→ os.Chdir(worktree)
→ task 派发(agent_type=correctness-reviewer, context_files=scope, budget, timeout)
→ os.Chdir 回
→ S1 快照:S1≠S0 ⇒ 写树违规(记录,该次结果仍参与统计但单独标记)
→ ParseOutput[ReviewResult];解析失败记为 invalid-run(单列,不算 pass 也不算 fail)
→ 记录 verdict/issues/usage/duration
```

### 3.4 与生产的偏差声明

评测跳过 REPL 的 episode/gate 外壳(carry 归因、修复轮、Ctrl+C),只测 **"给定改动 → reviewer 裁决"** 这一环。这是刻意的:基线要隔离度量 reviewer 本身;gate 管线的正确性已由 `pkg/chat` 的 mock 测试覆盖。

---

## 四、判定与匹配

### 4.1 自动判定

- **verdict 层**:`expect: pass` 用生产同款 `isPassVerdict`(pass 或零 issue);`expect: fail` 要求 verdict=fail 且 ≥1 命中。
- **issue 命中**:`issue.File` 与植入 bug 文件路径归一后相等(相对临时仓库根比较;容忍 reviewer 只报 basename,当且仅当 basename 在 scope 内唯一)且 `issue.Line ∈ [start−5, end+5]`。
- **near-miss**:同文件、线外 → 单列清单,不计命中,供人工复核。
- **类别不参与匹配**:LLM 对 bug 的归类措辞不稳定,manifest 的 `class` 只用于分桶统计。

### 4.2 人工复核通道(adjudications)

自动匹配有两类必然误判,必须给人工出口:

1. **clean case 里报了真 bug**:语料自身有缺陷,这不是假阳性——复核后修语料,该 case 该轮作废;
2. **bug case 里报了另一个真 bug**:检出率不该扣分——复核后либо把它补进 manifest `bugs`,либо记 adjudication。

复核结论写回 manifest:

```yaml
adjudications:
  - run: "2026-08-20-claude-sonnet-5/offbyone-slice-window/2"
    verdict: hit          # hit | true-positive-unplanted | corpus-defect
    note: "报的是 27 行下一格的真实 nil 解引用,补录为 bugs[1]"
```

重跑时 adjudication 覆盖自动判定。summary 明确分列"自动数字"与"复核修正后数字",防止悄悄美化。

---

## 五、输出与存放

```
eval/results/<date>-<model-alias>/
  runs.jsonl        # 逐次明细(gitignore)
  summary.md        # 提交入库:总表 + 分桶 + 稳定度 + near-miss/FP 清单
  summary.json      # 提交入库:机器可读,供跨基线对比
```

`runs.jsonl` 每行:case、run 序号、verdict、issues(含 scenario)、命中/near-miss 明细、写树违规、tokens、duration、model、**prompt 指纹**。

**prompt 指纹**:`correctnessReviewerSystemPrompt` 的 sha256 前 8 位记入每条结果与 summary 头部。提示词一改,指纹即变——杜绝"拿旧基线给新提示词背书"。BuildReviewPrompt 模板变更同理纳入指纹(两段拼接后取 hash)。

---

## 六、与后续工作的接口

| 后续 | 本设计预留 |
|---|---|
| §八-1 翻默认开 | 达标线草案(决策点 6):高危四类(logic/edge-case/off-by-one/error-path)逐 bug 检出 ≥70%、FP ≤10%、场景合规 ≥95%、写树违规 = 0。达标后改 `review_after_edit` 缺省并更新两份设计文档 |
| §七-1 投票阈值 | 稳定度分布直接给出投票增益上限;harness 预留 `--voters N`(不实现):同 case 并发 N 审多数裁决 —— 注意届时写树归因需按 §七-1 的"全体弃权"语义重设计 |
| §七-2 跨模型审查 | `--model` 已参数化;跨模型对比 = 换 alias 重跑,summary.json 可直接 diff |
| 提示词迭代 | 指纹 + 提交入库的 summary 构成回归线 |

---

## 七、实施计划

| 阶段 | 内容 | 产出 |
|---|---|---|
| P1 | harness:导出组装函数(决策点 1)、manifest loader、物化/chdir/快照/匹配/报告;**用 fake task tool 测 harness 全链路**(物化正确性、防污染、命中/near-miss/adjudication、指纹) | `pkg/commands/review_eval.go` 等 + 单测 |
| P2 | 语料 32 case(§2.2 配额),每 case 过一遍"base 可编译、change 可编译、bug 确实存在(has-test 的跑测试证伪)"的自检脚本 | `eval/review-cases/` |
| P3 | 真实模型首跑(runs=3),人工复核 near-miss/FP,adjudication 回写,summary 提交;更新 ADVERSARIAL_REVIEW_DESIGN.md §十一遗留项与 §八-1 状态 | `eval/results/<date>-<model>/` |

## 八、开放决策点(供评审拍板)

| # | 决策点 | 选项 | 倾向 |
|---|---|---|---|
| 1 | 组装同源的实现 | (a) 导出 `pkg/chat` 的 BuildReviewDiff/BuildReviewPrompt/快照;(b) 抽独立 `pkg/review` 包 | **(a)**。最小改动,语义同源已满足;抽包留给出现第三个消费者时 |
| 2 | 命中行容差 | ±3 / ±5 / ±10 | **±5**。太紧惩罚合理的报告位置差异(报在调用点 vs 定义点),太松让命中失去意义;near-miss 通道兜底 |
| 3 | 主指标粒度 | 逐 bug vs 逐 case | **逐 bug**(case 级为辅)。一 case 多 bug 时 case 级会掩盖漏检 |
| 4 | 重复次数 | 1 / 3 / 5 | **3**。1 测不了稳定度,5 的成本对首期语料规模不划算 |
| 5 | 结果入库策略 | summary 提交 + raw gitignore? | **是**。summary 是基线锚点必须可追溯;raw 明细大且含模型输出全文 |
| 6 | 达标线数字 | §六草案 | 检出 ≥70%(高危四类)/ FP ≤10% / 场景合规 ≥95% / 写树违规 =0;concurrency 与 task-mismatch 首期只观测不设线(预期这两类最难) |
| 7 | 语料语言 | Go-only vs 混合 | **Go-only 首期**。reviewer 的 bash 验证在 Go 生态最顺;混语料等指标体系稳定后再扩 |
| 8 | invalid-run 处理 | 计入 FP/漏检 vs 单列 | **单列**。解析失败是管线问题不是判断问题,混进指标会污染两头;但 invalid 率本身入 summary,持续偏高就是 schema/提示词的病 |

---

## 修订记录

- **v1(2026-08-16)**:初稿。基于 main @ 51c245a;已核实的实施约束:bash cwd = 进程 cwd(`ExecDirect` 不设 `cmd.Dir`)、`WithWorkDir` 仅影响 agent 类型解析(`subagent.go:43-48,89`)、task 结果用量在 `Data["subagent_usage"]`(`tools/subagent.go:193`)、子命令注册走 `topLevel.AddCommand`(`pkg/commands/commands.go`)。
