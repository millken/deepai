# Refine 功能设计文档

> 在现有 memory 提取链路（`ExtractUpdate` → `Merge`）之上，增加 auto-refine review gate、手动 `/refine`、rollback、refinement 历史。
>
> 参考实现：`prime-agent` 的 `packages/coding-agent/src/core/refinement/refinement.ts`。
>
> **核心原则**：不引入第二个产 Fact 的 LLM 提取器。refine 复用现有 `Extractor.ExtractUpdate`，只在外层加控制逻辑（gate / 触发 / 历史 / rollback）。

---

## 一、背景：prime-agent 的 refine 机制

### 核心思想

一个"持续学习"子系统：agent 在对话过程中把可复用经验**提炼成结构化持久状态**，注入后续 system prompt，从而在 token history 之外积累行为改进。

### 三层机制

| 层 | 触发 | 作用 |
|---|---|---|
| 手动 `/refine` | 用户 slash command 或 agent 调用 `refine.run` | LLM 分析对话轨迹 → 产出 JSON edits → 应用到 harness state |
| 自动 auto-refine | 每 N 轮 / compaction 时 | 先跑轻量 review gate（`shouldRefine?`），通过才执行 |
| rollback | `refine.run(rollbackId)` | 每个 refine 结果记录 before 快照，可逆向回滚 |

### 数据流

```
对话轨迹 + 当前 state + 历史
  → planRefinement (LLM, 产出 JSON proposal)
  → applyRefinementProposal (校验 + 应用 create/update/delete)
  → saveHarnessState (原子写 JSON)
  → formatHarnessStateForPrompt → 注入 system prompt
```

### 关键设计点

1. **plan/apply 分离** — LLM 调用耗时长，apply 前重读文件避免并发冲突
2. **JSON 容错解析** — 处理截断、fenced code、brace slicing
3. **原子写入** — tmp + rename，保留权限
4. **local/global 双作用域** — local 绑定会话，global 跨会话

---

## 二、deepai 现状与差距

### 已有能力（`pkg/memory`）

| prime-agent | deepai 现状 | 差距 |
|---|---|---|
| memory 条目提取 | `Service.UpdateWith` + `LLMClient.ExtractUpdate`（`llm.go`）→ LLM 提取 Fact | ✅ 已有 |
| 注入 prompt | `buildTurnInjection`（`promptbuild.go:125`）→ `InjectWithContext` | ✅ 已有 |
| Fact 存储 | `Document.Facts []Fact`，SQLite `memory_facts` 表（`sqlite.go:66`） | ✅ 已有 |
| 异步提取调度 | REPL 轮结束调度（`repl.go:780-785`，session + user scope 两次）+ compaction flush（`compact.go:127`） | ✅ 已有 |
| agent 写 memory | `memory` builtin tool：`add_fact`/`replace_fact`/`remove_fact`（`tools/builtin/memory.go:57`） | ✅ 已有 |
| **auto-refine gate** | 无 | ❌ |
| **手动 `/refine`** | 无 | ❌ |
| **rollback** | 无 | ❌ |
| **refinement 历史** | 无 | ❌ |

**结论**：deepai 的 memory 已覆盖 refine 的"提取+注入"核心链路。缺的全是**控制层**能力（gate、手动触发、rollback、历史），不是内容层。因此本方案围绕现有提取器构建，不引入第二个产 Fact 的 prompt。

---

## 三、移植范围

**在现有 memory 提取链路上增加控制层**，不引入新提取器、不引入新条目分类：

1. ✅ auto-refine review gate（**取代**现有无条件节流，见 §4.4）
2. ✅ 手动 `/refine` slash command（同步触发一次提取，显示结果）
3. ✅ rollback（基于穷举判定表 + 内容指纹 + 只覆盖内容字段，见 §4.4 流程 C）
4. ✅ refinement 历史记录
5. ❌ 不引入第二个产 Fact 的 LLM prompt（复用 `ExtractUpdate`）
6. ❌ 不引入 prompt/skill/subagent 条目分类
7. ❌ 不引入 local/global 双作用域（复用现有 session/user scope）

---

## 四、架构设计

### 4.1 核心洞察：refine = 受控的提取

prime-agent 的 refine 之所以需要独立的提取 prompt，是因为它管理四类条目（prompt/memory/skill/subagent），现有提取器只管 memory。deepai 只管 memory，现有 `ExtractUpdate` 已经在做 refine 要做的事。

因此 deepai 的 refine 不重新提取，而是**在现有提取链路外层加控制**：

```
现有链路（无 refine）:
  repl.go:780-785 每 5 轮无条件 ScheduleUpdateWith（session + user scope 各一次）
  → ExtractUpdate → Merge → Save

refine 增强后的链路（gate 取代无条件节流）:
  repl.go:780 每 5 轮 → ScheduleRefine 一次入队两个 job（各用自己的 scope key）:
    jobRefine(sessionID, token)       ← 跑 gate，verdict 写入 token
    jobRefineApproved(userScope, token) ← 读 token 的 verdict，不跑 gate
  手动:   /refine → CancelPendingUpdates(两 scope) → fan out 两 scope RefineAndRecord（同步，跳过 gate）
  回滚:   rollback → 穷举判定表合并 → Save
```

gate 是唯一新增的 LLM 调用，且它**取代**了原本无条件执行的提取——不是净增成本。gate 只在 session job 里跑一次，verdict 通过 token 传递给 user scope job，避免 gate 成本翻倍（§7.2）。

### 4.2 新增/修改文件

```
pkg/memory/
  refine.go              # RefineReview gate + RefinementRecord + Reviewer interface + rollback（新增）
  refine_test.go         #（新增）
  sqlite.go              # 新增 memory_refinements 表 + 迁移 + CRUD + SaveWithRefinement + SaveWithRollback（修改）
  queue.go               # 新增 jobRefine / jobRefineApproved 到既有 jobType block（修改）
  memory.go              # Service 新增 RefineAndRecord + ScheduleRefine（修改）

pkg/chat/
  slashcommands.go       # 新增 /refine 到命令列表（修改）
  repl.go                # handleSlashCommand 新增 /refine 分支（修改）
                         # gate 取代 repl.go:780-785 的两次 ScheduleUpdateWith（修改）

pkg/commands/
  setup.go               # Config 新增扁平字段（修改）
```

**不改动**：`pkg/agent/react.go`（turn 循环）、`pkg/agent/types.go`（AgentConfig）、`pkg/agent/promptbuild.go`（注入）。

**关于 `Storage` 的三个实现**（修正 v6 的文件表误标）：`Storage` interface 在 `memory.go:67`，本方案不动它（§7.8 走可选 interface）。实现有三个：

| 实现 | 位置 | 本方案 | 说明 |
|---|---|---|---|
| `SQLiteStore` | `sqlite.go` | **实现 `RefinementStore`** | 生产唯一路径（`chat.go:263` `NewSQLiteStoreFromDB`） |
| `PostgresStore` | `storage.go:127` | 暂不实现 | `OpenStore`（`store.go:36`）会把非 SQLite 的 `database_url` 路由到它，但 `OpenStore` 目前**只被测试调用**，生产不可达 |
| `FileStore` | `file_store.go:19` | 暂不实现 | 同样只在测试里构造 |

v6 把 `storage.go` 标成"Storage interface 不动"是误标——那个文件是 PostgresStore 实现，不是 interface。两个未实现 `RefinementStore` 的 backend 会走 §7.1 的 fallback（退回 `UpdateWith`，提取照常、只是没有 refine 历史），这正是那条 fallback 的价值：**Postgres 支持是待办，不是遗漏**。若将来把 `Config.DatabaseURL`（`setup.go:28`）接到 `OpenStore`，需要给 `PostgresStore` 补 `schemaSQL`（`storage.go:16`）里的 `memory_refinements` 建表和六个方法。

### 4.3 数据模型

#### RefinementRecord — 一次 refine 操作的完整记录

```go
// pkg/memory/refine.go

// RefinementRecord 记录一次 refine 操作，支持 rollback。
// PreSnapshot 是操作前的整份 Fact 快照（内容字段 + ID）。
// PostFactFingerprints 记录操作后各 Fact 的内容指纹，用于 rollback 冲突检测（§7.3）。
type RefinementRecord struct {
    ID                   string            `json:"id"`                     // refine_<unix_ns>（纳秒，避免同毫秒撞主键）
    PairID               string            `json:"pair_id"`                // 同一次 refine 的两条 scope 记录共享（§4.8 undo 成对回滚）
    Rationale            string            `json:"rationale"`              // 见下方「Rationale 的取值」
    SessionID            string            `json:"session_id"`             // 存储键（sessionID 或 UserScope.Key()）
    PreSnapshot          []Fact            `json:"pre_snapshot"`           // 操作前整份快照（内容字段）
    PostFactFingerprints map[string]string `json:"post_fact_fingerprints"` // 操作后 factID → 指纹
    FactIDsChanged       []string          `json:"fact_ids_changed"`       // 本次变更的 Fact ID（信息性）
    CreatedAt            time.Time         `json:"created_at"`
}
```

**ID 用纳秒**（v4 问题 C2）：同一 scope 同毫秒两次插入（rollback 记录 + 紧随的手动 refine）会撞主键 `(session_id, id)`。`refine_<unix_ns>` 用纳秒时间戳。

**PairID**（v6 问题 B1）：一次 refine 会在**两个分区**各写一条记录（`session_id = sessionID` 和 `session_id = UserScope(uid).Key()`）。`PairID` 由 `ScheduleRefine`/手动 `/refine` 一次生成、两条记录共用，`/refine undo` 据此成对回滚（§4.8）。auto 路径下 `PairID` 与传递 gate verdict 的 token 同值，省一个标识符。

**Rationale 的取值**（v6 问题 C4）：auto 路径 = gate 返回的 `rationale`；gate 被跳过（`reviewer == nil` 或 verdict 缺失 fail-open）= `"auto (no gate)"`；手动 `/refine` = `"manual"`；rollback 记录 = `"Rollback of <id>"`。调用方通过 `RefineAndRecord` 的 `rationale` 参数传入，`RefineAndRecord` 自己不构造。

**内容指纹**（v5 阻断 A1 → v6 修正 A2）：`formatDBTime`（`sqlite.go:318`）截断到整秒，时间戳跨存储往返有损。改用 `PostFactFingerprints map[string]string`。

**指纹必须覆盖 rollback 会覆盖的全部字段，且经过归一化**（v5 阻断 A2）：`prepareFact`（`storage.go:424`）在 Save 时对 Content/Category 做 `strings.TrimSpace`、对 Confidence 做 clamp。指纹若只取 Content，agent 用 `replace_fact` 只改 Category/Confidence 时指纹不变 → 判为"未被第三方改" → rollback 抹掉这些改动。指纹若取未归一化的内存值，与 Load 回来的已归一化内容可能不同（LLM 产出带首尾空白时）——这是 v4-A1 的同类错误（偶发失配而非必然）。

```go
// factFingerprint 对归一化后的覆盖字段取 hash，与 prepareFact 对齐。
// 覆盖字段 = rollback 会覆盖的字段：Content + Category + Confidence。
//
// Confidence 用 %f（6 位小数）是**有意的容差**，不是笔误：Confidence 经
// SQLite REAL 往返虽然精确，但 Merge（memory.go:662-667）和 prepareFact
// （storage.go:438-443）各有一次 clamp，留 6 位小数可以吸收任何表示层差异。
// 不要"修正"成 %v 或 %g —— 那会把浮点表示差异重新引入指纹（v6 问题 C2）。
func factFingerprint(f Fact) string {
    h := sha256.New()
    fmt.Fprintf(h, "%s\x00%s\x00%f",
        strings.TrimSpace(f.Content),
        strings.TrimSpace(f.Category),
        clampConfidence(f.Confidence))
    return hex.EncodeToString(h.Sum(nil))[:16]
}
```

#### RefineReview — auto-refine gate 的输出

```go
// RefineReview 是 auto-refine gate 的输出。
type RefineReview struct {
    ShouldRefine bool   `json:"shouldRefine"`
    Rationale    string `json:"rationale"`
}
```

#### Reviewer interface — gate 的调用接口

`Service` 只持有 `Extractor`（`memory.go:82`），接口里只有 `ExtractUpdate`。gate 需要独立接口：

```go
// pkg/memory/refine.go

// Reviewer 执行 auto-refine 的 review gate。
// *LLMClient 实现它；探测不到时退回无条件提取（不停止提取）。
type Reviewer interface {
    ReviewRefine(ctx context.Context, current Document, messages []models.Message) (RefineReview, error)
}
```

`Service` 新增 `reviewer Reviewer` 字段（可选，nil = 无 gate）。

#### 存储：新增 memory_refinements 表

`Document` 是逻辑聚合，物理存储是关系式的（`sqlite.go`）。**不给 `Document` 加 `Refinements` 字段**——两个 backend 一律只经 `RefinementStore` interface 访问。

```sql
-- sqlite.go AutoMigrate 新增
CREATE TABLE IF NOT EXISTS memory_refinements (
    session_id TEXT NOT NULL REFERENCES memories(session_id) ON DELETE CASCADE,
    id         TEXT NOT NULL,
    record     TEXT NOT NULL,   -- JSON blob of RefinementRecord
    created_at REAL NOT NULL,
    PRIMARY KEY (session_id, id)
);
CREATE INDEX IF NOT EXISTS idx_memory_refinements_session
    ON memory_refinements(session_id, created_at DESC);
```

**保留策略**：`ListRefinements` 默认返回最近 N 条（如 50），`InsertRefinement` 时若超过上限删除最旧的。`/refine list` 输出也有上限。

**外键约束**：DSN 开了 `foreign_keys(1)`（`sqlite.go:26`）。`RefineAndRecord` 的 `SaveWithRefinement` 已建 `memories` 行且先于 `InsertRefinement`，外键天然满足——不需要额外的空 Document Save。

#### RefinementStore — 可选 interface

不把方法塞进 `Storage`（会波及所有实现方和测试桩）。拆成可选 interface，类型断言探测：

```go
// pkg/memory/refine.go

// RefinementStore 是可选的 refinement 历史存储能力。
// 只有 SQLiteStore 实现（生产唯一路径）；PostgresStore/FileStore 及测试 fake
// 不实现，走 §7.1 的 fallback 退回 UpdateWith（提取照常，只是没有 refine 历史）。
type RefinementStore interface {
    ListRefinements(ctx context.Context, sessionID string, limit int) ([]RefinementRecord, error)
    // GetRefinement 按 id 取单条（rollback <id> 需要，且必须在锁内查，§4.6 RollbackRefinement）。
    GetRefinement(ctx context.Context, sessionID, id string) (RefinementRecord, error)
    InsertRefinement(ctx context.Context, sessionID string, record RefinementRecord) error
    DeleteRefinement(ctx context.Context, sessionID, id string) error
    // SaveWithRefinement 在同一事务内 Save doc + Insert record（refine 路径）。
    SaveWithRefinement(ctx context.Context, doc Document, record RefinementRecord) error
    // SaveWithRollback 在同一事务内 Save doc + Insert rollbackRecord + Delete originalID（rollback 路径）。
    SaveWithRollback(ctx context.Context, doc Document, newRecord RefinementRecord, deleteID string) error
}
```

`HasRefinementStore` 辅助函数用于 `/refine list`/`/refine status` 的能力探测（`RefineAndRecord` 内部就地断言，不用它）。

`SaveWithRollback`（v4 问题 B2）：rollback 路径的 `Save(doc)` + `InsertRefinement(rollbackRecord)` + `DeleteRefinement(originalID)` 必须在同一事务，否则中途失败留下不一致状态。

### 4.4 核心流程

#### 流程 A：auto-refine gate 取代无条件节流（REPL 轮结束）

**关键变更**（v5 阻断 A2/A3 + v5 问题 B1）：gate **取代** `repl.go:780-785` 的**两次** `ScheduleUpdateWith`。**两个 job 都提前入队**（各用自己的 scope key），gate verdict 通过 token 传递——不在 execute 内部反向 submit（避免单 worker 重入 submit 的 channel-full 丢弃）。

```
repl.go handleTurn 尾部（原 repl.go:780-785 位置）
  ↓ 原逻辑: if turn%interval==0 { ScheduleUpdateWith(session); ScheduleUpdateWith(userScope) }
  ↓         ← 全部删除
  ↓ 新逻辑:
    interval := resolveRefineInterval(cfg.MemoryRefineInterval)  // 0→5, -1→disabled(=0)
    switch {
    case interval > 0 && autoRefineEnabled && turn%interval == 0:
        ScheduleRefine(session, userScope, messages, ext)
    case interval <= 0 || !autoRefineEnabled:
        // auto-refine 关闭 → 退回原无条件提取，节流沿用 5 轮（§5）
        if turn%fallbackExtractInterval == 0 {
            ScheduleUpdateWith(session, ...)
            ScheduleUpdateWith(userScope, ...)
        }
    }

ScheduleRefine 生成一个 pairID（同时用作 verdict token），一次入队两个 job
（各用自己的 scope key）:
  - jobRefine(sessionID, pairID)            ← 跑 gate，verdict 写入 verdictStore[pairID]
  - jobRefineApproved(userScopeKey, pairID) ← 读 verdictStore[pairID]，不跑 gate

verdictStore 是 Service 持有的 sync.Map[string]gateVerdict。
单 worker FIFO（queue.go:246）保证 jobRefine 必先于 jobRefineApproved 出队，
不需要任何等待原语。

jobRefine executor:
  ctx = withCapturedFlushVersion(ctx, job)   ← 必须显式调用（§7.6 不变量）
  1. verdict 决策:
       reviewer == nil          → approved（rationale="auto (no gate)"）
       reviewer 返回 err        → approved（fail-open，warn 日志；§7.11）
       reviewer 返回 review     → approved = review.ShouldRefine
     verdictStore[pairID] = verdict（含时间戳，供 B2 兜底清理）
  2. 若 !approved → 结束（session 不提取；user scope 由 approved job 读到
     verdict=false 后同样跳过）
  3. 若 approved → RefineAndRecord(sessionID, ..., verdict.rationale)

jobRefineApproved executor:
  ctx = withCapturedFlushVersion(ctx, job)   ← 必须显式调用（§7.6 不变量）
  v, ok := verdictStore.LoadAndDelete(pairID)   ← 读后即删（§7.12 泄漏兜底）
  switch {
  case !ok:            // jobRefine 被 cancel/dedup 掉，gate 从未运行
      → fail-open：无条件 RefineAndRecord(userScopeKey, ..., "auto (no gate)")
  case !v.shouldRefine:
      → 结束（gate 明确拒绝）
  default:
      → RefineAndRecord(userScopeKey, ..., v.rationale)
  }
```

**verdict 缺失必须 fail-open**（v6 阻断 A2，§7.11）：`CancelPendingUpdates(sessionID)`（compaction，`compact.go:127`）只删 `"update:"+sessionID` 的 pending 条目，worker 出队时 `!exists → 跳过`（`queue.go:249-255`），于是 `jobRefine` 从不运行、verdict 从不写入。而 `jobRefineApproved` 用的是**独立分片**的 key，不受这次 cancel 影响，照常执行。若此时把"verdict 缺失"当作拒绝，user scope 的提取就被跳过——而 compaction 的同步 flush（`compact.go:129-133`）**只补 session scope**，user scope 的事实直接丢失。这正是 v4-A3 换了条路径重现。

区分两种缺失语义：**条目不存在** = gate 没跑过 → 无条件提取（恢复改造前的行为）；**条目存在且 `shouldRefine=false`** = gate 明确拒绝 → 跳过。

**为什么两个都提前入队**（v5 问题 B1）：v5 的"session job 通过后 submit user scope job"要求 worker 在 execute 内部反向调 submit。队列是单 worker（`queue.go:83`），submit 在 channel 满时阻塞至多 `submitTimeout = 2s`（`queue.go:105`）后丢弃并只打 warn。唯一的消费者此刻卡在 execute 里，没人排空——user scope 的 refine 被静默丢掉，worker 还白等 2 秒。两个都提前入队则各自在 submit 时拿到自己 scope key 的 flushVersion（正是 §7.4 要的），也没有重入。

**compaction flush 不经 gate**：`compact.go:127-132` 的同步 flush 仍走 `UpdateWith`，无条件保住 facts。这是对的——compaction 前必须无条件保住 facts，不要"顺手统一"掉。

#### 流程 B：手动 `/refine`（同步，fan out 两 scope）

```
用户输入 /refine
  ↓ repl.go handleSlashCommand 新增 "refine" 分支
先 CancelPendingUpdates(两 scope)（清掉队列里 pending 的 job，
  避免背靠背跑两次提取插两条记录。compact.go:127 就是这个做法）
  ↓
fan out 两 scope（与 auto 对齐，否则 /refine off 后 user scope 不更新）:
  - RefineAndRecord(sessionID, ...)（同步，跳过 gate）
  - RefineAndRecord(userScopeKey, ...)（同步，跳过 gate）
  → 显示两 scope 的 diff 摘要（+N / ~M / -K）
```

**进度提示**（v5 问题 C3）：手动 `/refine` 串行跑两个 scope 的提取，两次 LLM 调用阻塞 REPL，最坏到分钟级。执行前显示"正在提炼记忆..."，每个 scope 完成后显示进度。

`/refine` 不接受自由文本参数：所有参数都是保留子命令。

#### 流程 C：rollback（穷举判定表 + 内容指纹 + 只覆盖内容字段）

**关键变更**（v5 阻断 A1）：v5 的判定表缺"当前是否存在"这一维，3 个子情形错误或未定义。v6 改为按 `(in Pre, in Post, 当前存在, 指纹匹配)` 的**穷举表**，遍历集合 = `Pre ∪ Post ∪ current`。

**v7 关键变更**（v6 阻断 A3）：整个流程必须在 `getSessionLock` 内执行，且必须是 `pkg/memory` 的一个 Service 方法——`Service.RollbackRefinement`（§4.6）。v6 把编排放在 `pkg/chat/repl.go`，那里**结构上拿不到锁**（`getSessionLock` 未导出，`memory.go:133`），而 rollback 是一次整体重写 `doc.Facts` 的 load-modify-save，锁外执行会被 memory builtin tool 的 `svc.Save`（`builtin/memory.go:153/206/242`）插进中间并静默抹掉——§4.6 对 `RefineAndRecord` 的论证逐字适用。

```
Service.RollbackRefinement(ctx, sessionID, recordID)  ← 以下全部在 getSessionLock 内
  ↓
GetRefinement(sessionID, recordID) → record（含 PreSnapshot、PostFactFingerprints）
  ↓
Load 当前 Document（currentFacts）
  ↓
遍历集合 = PreSnapshot.IDs ∪ PostFactFingerprints.Keys ∪ currentFacts.IDs
对每个 fact ID，按穷举表判定（指纹 = factFingerprint，覆盖 Content+Category+Confidence）:

┌───────┬────────┬──────────┬──────────────┬──────────────────────────────────────────────┐
│ in Pre│ in Post│ 当前存在 │ 指纹==Post?  │ 处理                                         │
├───────┼────────┼──────────┼──────────────┼──────────────────────────────────────────────┤
│  是   │  是    │  是      │  是          │ **就地恢复**（只覆盖内容字段，计数保留当前） │
│  是   │  是    │  是      │  否（第三方改）│ 保留：当前值不动                            │
│  是   │  是    │  否（第三方删）│  —      │ 尊重删除：不恢复                             │
│  是   │  否    │  是      │  —（驱逐后重建）│ 保留：当前值不动（agent 新建的）           │
│  是   │  否    │  否      │  —（refine 驱逐）│ **整条重插**（含快照计数，UpdatedAt=now）   │
│  否   │  是    │  是      │  是          │ 删除：本次 refine 产出，回滚掉               │
│  否   │  是    │  是      │  否（第三方改）│ 保留：当前值不动                            │
│  否   │  是    │  否      │  —           │ 无操作（refine 新增后又被删，自然消失）      │
│  否   │  否    │  是      │  —（快照后新增）│ 保留：当前值不动                            │
│  否   │  否    │  否      │  —           │ 不可达（不在遍历集合中）                     │
└───────┴────────┴──────────┴──────────────┴──────────────────────────────────────────────┘

两行"恢复"语义不同，必须分开实现（v6 问题 B3）:
  - **就地恢复**（第 1 行）：存在同 ID 的当前 fact → 只覆盖 Content/Category/Confidence，
    保留 RetrievalCount/HelpfulCount/SuspectCount 的**当前值**（计数走直接 SQL 累加，
    绕过 Document，快照里的是陈旧值）。
  - **整条重插**（第 5 行）：当前无同 ID fact（被本次 refine 驱逐），没有"当前值"可保留，
    只能整条插回 Pre 的 Fact，**包括快照时的计数**。但 `UpdatedAt` 必须设为 now
    （`CreatedAt` 保留快照值，v6 问题 C1）——`Merge` 的排序（`memory.go:704-709`）和
    `factScore` 的 recency 衰减（`memory.go:727-728`）都吃 `UpdatedAt`，带回老时间戳
    会让这条 fact 在下次 Merge 里优先被驱逐，等于回滚完立刻又被赶走。
  ↓
doc.Facts = 判定表产出
  ↓
计算 rollback 记录的 PostFactFingerprints（v4 问题 B3：rollback 记录必须有 Post，
  否则再回滚时判定表 Post 列全空，什么都不删，不是逆操作）:
  postFP = factFingerprints(doc.Facts)
  rollbackRecord = RefinementRecord{
    PreSnapshot:          <rollback 前的 facts>,
    PostFactFingerprints: postFP,
    Rationale:            "Rollback of <id>",
    PairID:               <新 pairID；成对 undo 时两 scope 的 rollback 记录共用>,
    ...
  }
  ↓
SaveWithRollback(ctx, doc, rollbackRecord, originalID)（同一事务）
  → Save doc + Insert rollbackRecord + Delete originalID
  ↓
（仍在锁内，函数返回后释放）
```

**核心原则一致性**（v5 阻断 A1）：第 7 行（`否/是/是/否`）与第 2 行（`是/是/是/否`）是同一个原则——"被第三方改过就保留"。v5 的五行表在第 4 行（原"refine 新增"）放弃了这个原则，v6 穷举表统一执行。

**计数保护**：`RetrievalCount`/`HelpfulCount`/`SuspectCount` 由直接 SQL 累加（`sqlite.go:161/182/204`），绕过 Document。`Save` 是先 delete 再全量 insert（`sqlite.go:134`）。就地恢复只覆盖内容字段，计数保留当前值。

**暂时超限**：`prepareDocument`（`storage.go:393`）只做 trim/dedup 不驱逐。恢复被驱逐的 fact 后可能暂时超 30，直到下次 Merge 才驱逐——可接受。

**`RollbackFacts` 保持纯函数**：签名 `RollbackFacts(currentFacts []Fact, record RefinementRecord, now time.Time) (result []Fact, skipped []string)`。`now` 由调用方注入（第 5 行整条重插要用），便于单测固定时间。锁、IO、事务全在 `Service.RollbackRefinement` 里（§4.6）。

### 4.5 LLM Prompt — 仅 gate 一个

**只有 review gate 是新增的 LLM 调用**。提取复用现有 `MemoryUpdateSystemPrompt`（`llm.go`）。

**Review gate prompt**（对应 prime-agent 的 `AUTO_REFINE_REVIEW_SYSTEM_PROMPT`）：

```
你是 deepai 的自动记忆提炼审查门。
决定当前检查点是否值得跑一次完整的记忆提取。只有当对话轨迹包含对未来轮次有用的稳定事实、决策、偏好、或失败教训时才批准。
拒绝一次性噪声、无支撑的假设、瞬态工具输出、纯问答交互。

返回 JSON：
{"shouldRefine": true|false, "rationale": "简短理由"}
```

gate 不产出 Fact，只做布尔决策。

### 4.6 锁内原子方法：RefineAndRecord

**关键**：不能从外部组合"快照 + UpdateWith + diff"——`UpdateWith`（`memory.go:265`）自己拿 `getSessionLock`，在锁内完成 load→extract→merge→save，不向外暴露 pre-state。外部快照的 Load 发生在锁外，memory builtin tool 的 `svc.Save`（另一个独立临界区）可以插在中间。

`RefineAndRecord` 在**同一次加锁内**完成 pre-snapshot / extract / Save+Insert（同一事务）/ 返回：

```go
// pkg/memory/memory.go

// RefineAndRecord 在单次 session 锁内完成：快照 → 提取 → Save+Insert(同事务) → 返回。
// 返回 (record, saved, err)。saved=false 表示提取被跳过（无消息、flush-stale、或无变化）。
// 若 storage 不支持 RefinementStore，退回 UpdateWith（不记历史，不报错）。
func (s *Service) RefineAndRecord(
    ctx context.Context, sessionID string,
    messages []models.Message, ext Extractor,
    rationale string,
) (record RefinementRecord, saved bool, err error) {
    // 前置检查（锁外早退，避免空消息也付一次提取调用）
    filteredMessages := filterMessagesForMemory(messages)
    if len(filteredMessages) == 0 {
        return RefinementRecord{}, false, nil
    }

    // 探测 RefinementStore（一次断言拿值，避免两处判断漂移）
    rs, hasHistory := s.storage.(RefinementStore)

    // 不支持历史 → 退回 UpdateWith（不停止提取）
    if !hasHistory {
        err := s.UpdateWith(ctx, sessionID, messages, ext)
        // UpdateWith 在"无消息"和"flush-stale"上返回 nil 且不 Save（v5 问题 B3）。
        // fallback 路径无法区分，保守返回 false，避免 /refine 谎报"已提取"。
        return RefinementRecord{}, false, err
    }

    mu := s.getSessionLock(sessionID)
    mu.Lock()
    defer mu.Unlock()

    // 1. pre-snapshot（锁内）
    current, err := s.storage.Load(ctx, sessionID)
    if err != nil && !errors.Is(err, ErrNotFound) {
        return RefinementRecord{}, false, fmt.Errorf("load: %w", err)
    }
    preSnapshot := cloneFactsContent(current.Facts)

    // 2. flush-version 捕获（照抄 UpdateWith memory.go:269-275）
    captured := s.captureFlushVersion(sessionID)
    if v, ok := capturedFlushVersionFromContext(ctx); ok {
        captured = v
    }

    // 3. extract + merge（锁内）
    update, err := ext.ExtractUpdate(ctx, current, cloneMessages(filteredMessages))
    if err != nil {
        return RefinementRecord{}, false, err
    }
    update = sanitizeUpdateForStorage(update)
    merged := MergeWithFactSource(current, update, sessionID, factSourceFromMessages(filteredMessages), time.Now().UTC())

    // 4. flush-stale 检查（§7.5）
    if s.isFlushStale(sessionID, captured) {
        return RefinementRecord{}, false, nil  // 不 Save，不记录
    }

    // 5. diff（Save 前算，基于 pre vs merged）
    changedIDs := diffFactIDs(preSnapshot, merged.Facts)
    if len(changedIDs) == 0 {
        return RefinementRecord{}, false, nil  // 提取了但无变化，不记录
    }

    // 6. post-fingerprints（锁内，Save 前。对归一化后的覆盖字段取 hash，§7.3）
    postFP := factFingerprints(merged.Facts)
    record := RefinementRecord{
        ID:                   refineID(),  // refine_<unix_ns>
        Rationale:            rationale,
        SessionID:            sessionID,
        PreSnapshot:          preSnapshot,
        PostFactFingerprints: postFP,
        FactIDsChanged:       changedIDs,
        CreatedAt:            time.Now().UTC(),
    }

    // 7. Save + Insert 同一事务（§7.1 B1：避免 facts 变了记录失败）
    if err := rs.SaveWithRefinement(ctx, merged, record); err != nil {
        return RefinementRecord{}, false, err
    }
    return record, true, nil
}
```

**三条静默 return 的处理**（§7.5）：`len(filteredMessages)==0`（锁外早退）、`isFlushStale`、`len(changedIDs)==0` 都返回 `saved=false`，不插入记录。

**fallback 路径的 saved 返回值**（v5 问题 B3）：`UpdateWith` 在"无消息"（`memory.go:261`）和"flush-stale"（`memory.go:286`）上返回 nil 且不 Save。fallback 路径无法区分，保守返回 `false`，避免 `/refine` 谎报"已提取"。

**不支持 RefinementStore 时退回 UpdateWith**（v4 问题 B1）：探测不到不报错、不停提取。生产上只有 SQLite（`chat.go:263`），但 fake storage 的集成测试语义不会被静默改变。

#### Service.RollbackRefinement — rollback 的锁内入口（v6 阻断 A3）

rollback 与 refine 是同一类操作：load-modify-save 整份 `doc.Facts`。§4.6 开头对 `RefineAndRecord` 的论证逐字适用，所以它同样必须是 `pkg/memory` 的 Service 方法，而不能由 `pkg/chat` 编排——`getSessionLock` 未导出（`memory.go:133`），REPL 结构上拿不到锁。

```go
// pkg/memory/memory.go

// RollbackRefinement 在单次 session 锁内完成：取记录 → 判定表 → Save+Insert+Delete(同事务)。
// skipped 是被判定为"保留/尊重删除"的 fact ID，供 UI 说明哪些没有恢复。
// pairID 由调用方传入：成对 undo 时两个 scope 的 rollback 记录必须共用一个
// pairID，否则"回滚这次回滚"又变回单 scope 操作（自审发现）。
func (s *Service) RollbackRefinement(
    ctx context.Context, sessionID, recordID, pairID string,
) (skipped []string, err error) {
    rs, ok := s.storage.(RefinementStore)
    if !ok {
        return nil, errors.New("rollback requires a RefinementStore backend")
    }

    mu := s.getSessionLock(sessionID)
    mu.Lock()
    defer mu.Unlock()

    record, err := rs.GetRefinement(ctx, sessionID, recordID)   // 锁内查，避免 TOCTOU
    if err != nil {
        return nil, err
    }
    current, err := s.storage.Load(ctx, sessionID)
    if err != nil && !errors.Is(err, ErrNotFound) {
        return nil, fmt.Errorf("load: %w", err)
    }

    now := time.Now().UTC()
    facts, skipped := RollbackFacts(current.Facts, record, now)   // 纯函数

    doc := current
    doc.SessionID = sessionID   // Load 返回 ErrNotFound 时 current 是零值，
                                // prepareDocument（storage.go:397-400）要求非空（自审发现）
    doc.Facts = facts
    doc.UpdatedAt = now

    rollbackRecord := RefinementRecord{
        ID:                   refineID(),
        PairID:               pairID,
        Rationale:            "Rollback of " + recordID,
        SessionID:            sessionID,
        PreSnapshot:          cloneFactsContent(current.Facts),  // rollback 前状态
        PostFactFingerprints: factFingerprints(facts),           // §7.7：必须填，否则不可再回滚
        FactIDsChanged:       diffFactIDs(current.Facts, facts),
        CreatedAt:            now,
    }
    // 同一事务：Save doc + Insert rollbackRecord + Delete recordID
    if err := rs.SaveWithRollback(ctx, doc, rollbackRecord, recordID); err != nil {
        return nil, err
    }
    return skipped, nil
}
```

与 `RefineAndRecord` 不同，这里**不做 flush-version 检查**：rollback 是用户显式的同步操作，语义上就该覆盖任何在途的异步提取结果。但调用方（`/refine undo`/`rollback`）必须先 `CancelPendingUpdates(sessionID)`，让在途的 refine 在自己的 `isFlushStale` 上退出，否则它可能在 rollback 之后落盘、把回滚掉的 fact 又写回来。

这条顺序之所以够用，是因为 `CancelPendingUpdates`（`memory.go:126-131`）**自己也要拿同一把 session 锁**：若某个 refine 已经越过 `isFlushStale` 正在 `SaveWithRefinement` 里，它持锁，`CancelPendingUpdates` 会阻塞到它落盘完成；之后 rollback 再拿锁，看到的是最终状态。若那个 refine 还没到检查点，版本号已被 bump，它会在检查点退出。两种情况都不会出现"rollback 之后被覆盖"。

### 4.7 异步执行：复用 UpdateQueue，两个按 scope key 的 job

auto-refine 的 gate + 提取走现有 `UpdateQueue`（`queue.go`），新增两个 jobType 到**既有 block 末尾**：

```go
// queue.go，追加到既有 const block（queue.go:23-33）末尾
const (
    jobUpdateWith           jobType = iota
    jobUpdate
    jobUpdateWithFactSource
    jobRecordSkillUsage
    jobIncrementRetrieval
    jobIncrementHelpful
    jobIncrementSuspect
    jobUpdateScopeWithSkill
    jobPreferenceUpdate
    jobRefine           // ← 跑 gate，verdict 写入 token
    jobRefineApproved   // ← 读 token verdict，不跑 gate
)
```

**dedup key — 走 default 分支**：`queue.go:125` 的 flushVersion 捕获只在 `strings.HasPrefix(key, "update:")` 时触发。`jobRefine` 和 `jobRefineApproved` 都**不加 case**，落入 default（`queue.go:238`：`"update:"+sessionID`），自动获得 flushVersion 捕获。

**关键：两个 job 各用自己的 scope key**（§7.4）：`jobRefine` 的 `sessionID` 是会话 ID，`jobRefineApproved` 的 `sessionID` 是 `UserScope(uid).Key()`。各自走 default 分支得到各自的 `"update:"+<自己的scope key>`，flushVersion、dedup、cancel 三套机制各自按正确的 scope key 分片。

**executor 必须显式调 `withCapturedFlushVersion`**（v5 阻断 A3）：这个函数是逐 case 调用的（`queue.go:280/288/296`），9 个 job type 里只有 3 个用。`jobRefine`/`jobRefineApproved` 的 case 若漏掉这行，`capturedFlushVersionFromContext` 取不到值 → §4.6 第 2 步退回锁内自行 `captureFlushVersion` → §7.6 整套 flushVersion 保护静默失效。executor 规格里必须写死这行（§7.6 不变量）。

```go
// queue.go execute 新增 case
case jobRefine:
    ctx, cancel := context.WithTimeout(ctx, timeout)  // gate + 提取共用一个预算（v5 问题 C2）
    defer cancel()
    ctx = withCapturedFlushVersion(ctx, job)  // ← 必须显式调用
    // ... gate → verdictStore[token] → RefineAndRecord ...

case jobRefineApproved:
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    ctx = withCapturedFlushVersion(ctx, job)  // ← 必须显式调用
    // ... 读 verdictStore[token] → RefineAndRecord ...
```

**timeout 是一个预算**（v5 问题 C2）：`jobRefine` 的 gate + 提取两次 LLM 调用共用 execute 的单个 timeout（`queue.go:272`，默认 `defaultUpdateTimeout = 5m`，`memory.go:16`）。够用，但写明是一个预算而非两个，免得后来有人调小。

**不用裸 `go` + Run 的 ctx**。worker 有自己的 ctx（`queue.go:75`）、per-job timeout、dedup、并发上限，且受 `CancelPendingUpdates`（`memory.go:123`）管辖。

### 4.8 Slash command 集成

命令注册：`slashcommands.go` 新增到列表。
命令分发：`repl.go:904` `handleSlashCommand` 的 switch 新增 `"refine"` 分支。

| 命令 | 作用 | 同步/异步 |
|---|---|---|
| `/refine` | 立即触发两 scope 提取（同步，显示 diff 摘要 + 进度） | 同步 |
| `/refine undo` | 回滚最近一次 refine（**按 PairID 成对回滚两 scope**） | 同步 |
| `/refine rollback <id>` | 回滚指定记录（**只动这一条**，明确提示另一 scope 不受影响） | 同步 |
| `/refine list` | 合并列出两个 scope 分区的历史（最近 N 条，标注 scope） | 同步 |
| `/refine status` | 显示 auto-refine 状态（配置默认值 + 会话级覆盖） | 同步 |
| `/refine on` | 开启会话级 auto-refine | 同步 |
| `/refine off` | 关闭会话级 auto-refine | 同步 |

**保留字规则**：`undo`/`rollback`/`list`/`status`/`on`/`off` 是保留子命令，`/refine` 不接受自由文本。`/refine on` / `/refine off` 切换**会话级** auto-refine 开关（不写回 config.yaml，重启恢复默认）。

**`/refine status` 显示两层**：配置默认值（`memory_auto_refine`）+ 会话级覆盖（若 `/refine off` 过），让用户知道重启会恢复默认。

**与 `/undo` 的区分**：`/undo`（`slashcommands.go:19`）是"撤销最后一个对话轮"，`/refine undo` 是"撤销最后一次记忆提炼"。

**两 scope 的读侧语义**（v6 问题 B1）：写侧 fan out 之后，一次 refine 会在**两个分区**各留一条记录（`session_id = sessionID` / `UserScope(uid).Key()`）。读侧必须跟上：

- `/refine list`：分别 `ListRefinements` 两个分区，按 `CreatedAt` 归并排序，每行标注 scope 和 `ID`。
- `/refine undo`：取 session 分区最新一条，用它的 `PairID` 到 user scope 分区找配对记录，**两条都回滚**（两次 `RollbackRefinement`，传入同一个新 `pairID`，各自锁内，各自 `CancelPendingUpdates`）。配对记录不存在（例如那一侧当轮无变化、`saved=false` 没插记录）就只回滚存在的那条，并在输出中说明。
  - `PairID` 存在 `record` JSON blob 里，没有索引。配对查找 = `ListRefinements(userScopeKey, 50)` 后线性过滤。keep-last-N 是 50，扫 50 条无所谓，**不要**为此加列或加索引。
- `/refine rollback <id>`：`id` 只在一个分区里存在，先查 session 分区、再查 user scope 分区，命中即回滚该条；输出显式提示"另一 scope 的配对记录未回滚，如需成对回滚请用 `/refine undo`"。

**部分失败**：成对回滚是两次独立事务。不是 SQLite 的限制（同库同连接，一个 tx 完全能覆盖两个 `session_id`），而是**锁粒度**：两个 scope 是两把独立的 `getSessionLock`，合并成一个事务就得同时持有两把锁并定义锁序，为一个用户显式操作引入死锁面不划算。第一条成功、第二条失败时如实报告"session scope 已回滚，user scope 失败：<err>"，不做自动补偿——两条记录各自独立可回滚，用户重试第二条即可。

---

## 五、配置项

`~/.deepai/config.yaml`（`commands/paths.go:31`），`Config` 结构（`setup.go:25`）是**扁平**的，无嵌套 map：

```yaml
# ~/.deepai/config.yaml（扁平结构，与现有 token_metrics 等字段一致）
memory_auto_refine_disabled: false  # false（零值/缺省）= 启用 auto-refine；true = 禁用
memory_refine_interval: 5           # 每 N 个 REPL 轮检查一次（缺省/0 = 取默认 5；-1 = 禁用 auto-refine，退回无条件提取）
```

对应 `Config` 新增字段（`setup.go`）：

```go
type Config struct {
    // ... 现有扁平字段 ...
    // 反转命名：零值即启用，与"默认开启"语义一致（v5 问题 B2）。
    // 现有 TokenAging bool 默认关闭所以没暴露零值坑，不能照抄。
    MemoryAutoRefineDisabled bool `yaml:"memory_auto_refine_disabled,omitempty"`
    // 0（缺省）= 取默认 5；-1 = 禁用。见 resolveRefineInterval。
    MemoryRefineInterval     int  `yaml:"memory_refine_interval,omitempty"`
}
```

**零值语义**（v5 问题 B2）：Go 的 bool 零值是 false，`omitempty` 不区分"没写"和"写了 false"。`MemoryAutoRefine` bool 表达不了"默认 true"，反转成 `MemoryAutoRefineDisabled`（零值即启用）。

**interval 必须过 resolver，不能直接用**（v6 阻断 A1）：`MemoryRefineInterval` 的 Go 零值是 0，而绝大多数 config.yaml 里根本没有这一行。若 §4.4 直接用 `interval > 0` 守卫，缺省配置下 `interval == 0` → 既不 refine 也不退回无条件提取 → **提取彻底停止**。现成先例是 `RequestTimeout int`（`setup.go:31`，同样用 0 表示未设），由 `resolveRequestTimeout`（`chat.go:371-373`）在装配时补默认：

```go
// pkg/commands/chat.go，与 resolveRequestTimeout 并列
// resolveRefineInterval: 0/缺省 → 默认 5；负数 → 0（禁用 auto-refine）。
func resolveRefineInterval(v int) int {
    switch {
    case v == 0:
        return defaultRefineInterval  // 5
    case v < 0:
        return 0                      // 禁用
    default:
        return v
    }
}
```

**"每轮 refine" 无法通过 0 表达**：v6 曾声称"0 是合法的每轮值"——在 `int` + `omitempty` 下表达不出来（缺省与显式 0 不可区分），该说法已删除。真要支持就得把字段换成 `*int`，当前不值得。想要最密的节流写 `memory_refine_interval: 1`。

**`memoryExtractInterval`（`repl.go:52-56`）改名保留，不删**（修正 v6）：v6 说"删就完了"，但 §4.4 的关闭分支还要用它做 fallback 的节流。改名为 `fallbackExtractInterval`（值仍是 5）并保留常量，用于 auto-refine 关闭时的无条件提取节流。

**`memory_refine_interval: -1` 或 `memory_auto_refine_disabled: true` 的行为**：禁用 auto-refine，退回**原样的**无条件提取——`ScheduleUpdateWith(session)` + `ScheduleUpdateWith(userScope)`，每 `fallbackExtractInterval`（5）轮一次，与改造前逐字一致。这样配置关闭时不会停止提取，也不会改变节流密度。

**不新增 `auto_refine_model`**：gate 复用现有 memory LLM client（`LLMClient`，`llm.go`），其 model 已由 `NewLLMClient(provider, model)` 配置。

**不进 AgentConfig**（`types.go:47`）：refine 是 REPL 层功能，不涉及 agent 内部。

---

## 六、实现步骤（按依赖顺序）

| 步骤 | 文件 | 内容 | 验证 |
|---|---|---|---|
| 1 | `pkg/memory/refine.go` | `RefinementRecord`（含 `PairID`）、`RefineReview`、`Reviewer`、`RefinementStore`、`HasRefinementStore`、`factFingerprint`（归一化+覆盖字段）、`factFingerprints`、`refineID`（纳秒）、`newPairID`、`gateVerdict`（含时间戳） | 编译通过 |
| 2 | `pkg/memory/sqlite.go` | `memory_refinements` 表 DDL + `ListRefinements`/`GetRefinement`/`InsertRefinement`/`DeleteRefinement`/`SaveWithRefinement`/`SaveWithRollback` + keep-last-N | 单测：建表 + CRUD + 两事务方法失败时整体回滚 |
| 3 | `pkg/memory/review.go` | `(*LLMClient).ReviewRefine` — 实现 `Reviewer`；`RefineReviewSystemPrompt`、`BuildRefineReviewPrompt`、`refineReviewMaxTokens=2048` | 单测：批准/拒绝/fenced JSON/token 预算上限/空消息不调用/provider 错误与不可解析均返回 error（供调用方 fail-open） |
| 4 | `pkg/memory/refine.go` | JSON 容错解析（复用 `llm.go` 的 `extractJSON`） | 已有覆盖 |
| 5 | `pkg/memory/refine.go` | `Service.RefineAndRecord(ctx, sessionID, messages, ext, RefineMeta{PairID, Rationale})` — 锁内原子；不支持 `RefinementStore` 时退回 UpdateWith（saved=false）；`cloneFacts`/`diffFactIDs` | 单测：saved=false 的三条路径都不留记录；flush-stale 不覆盖同步 flush；提取期间**持锁**（`TryLock` 断言）；产出的 record 能被 `RollbackFacts` 正确回滚；PairID 落盘 |
| 6 | `pkg/memory/rollback.go` | `RollbackFacts(currentFacts, record, now) (result []Fact, skipped []string)` — 纯函数，穷举判定表（10 行）+ 就地恢复/整条重插两种语义 + 计数保留 | 单测：10 行判定各一例 + 指纹归一化 + 计数不被覆盖 + 重插的 `UpdatedAt==now` + 回滚的回滚是逆操作 |
| 7 | `pkg/memory/rollback.go` | `Service.RollbackRefinement(ctx, sessionID, recordID, pairID)` — 锁内：`GetRefinement` → `RollbackFacts` → `SaveWithRollback`；返回 `skipped`；无 `RefinementStore` 时**报错**（与提取不同，回滚没有降级模式） | 单测：并发两次只生效一次（锁覆盖 Get→Delete）；rollback 记录含 `PostFactFingerprints`+`PairID`；再回滚一次是逆操作；第三方编辑被保留并计入 `skipped`；`Merge` 的 `#prev` 归档随 refine 一起被回滚 |
| 8 | `pkg/memory/queue.go` | `jobRefine`/`jobRefineApproved` 追加到既有 block；`updateJob` 加 `pairID`/`pairQueued`；**dedupKey 不加 case**（走 default）；两 case **显式调 `withCapturedFlushVersion`** | 单测：gate 拒绝时两 scope 都不提取；**verdict 缺失时 user scope 仍提取（fail-open）**；两 scope 各自正确 flushVersion；reviewer=nil / gate 报错均 fail-open |
| 9 | `pkg/memory/refine.go` | `Service.ScheduleRefine(sessionID, userScopeKey, messages, ext)` — 生成 pairID，一次入队两 job；`Service.reviewer`/`verdicts` 字段 + `WithReviewer`；`purgeStaleVerdicts`（TTL 10 分钟兜底） | 单测：两 scope 各自入队且共享 PairID；无 user scope 时不发布 verdict；verdictStore 无泄漏 |
| 10 | `pkg/commands/setup.go`、`chat.go` | `Config` 加 `MemoryAutoRefineDisabled`/`MemoryRefineInterval`；`resolveRefineInterval`（与 `resolveRequestTimeout` 并列）；`memService.WithReviewer(memClient)` 接上 gate | 单测：缺省(0)→5、-1→0、正数原样 |
| 11 | `pkg/chat/repl.go` | `memoryScheduleFor` 三态决策 + `ReplConfig` 两个字段；替换 repl.go:780-785；`memoryExtractInterval` 改名 `fallbackExtractInterval` 保留 | 单测：判定表逐例；**性质测试：任何 interval/开关组合都不会完全停掉提取** |
| 12 | `pkg/chat/slashcommands.go` | `/refine` 加入命令列表 | `/help` 显示 |
| 13 | `pkg/chat/refine_command.go` | `parseRefineCommand`（保留字，拒自由文本）、`mergeScopedRecords`/`formatRefinementList`、`findUndoTargets`（按 PairID 配对）、`autoRefineEnabled`/`setAutoRefine`（会话级覆盖）、`handleRefineCommand` + run/undo/rollback 编排；`Service.ListRefinements`/`GetRefinement`/`NewPairID`/`RefinementRecord.Summary` | 单测：解析逐例；两分区归并按时间序并标注 scope；配对 undo 与缺失一半；会话覆盖优先于配置 |


---

## 七、关键设计决策与权衡

### 7.1 rollback 用穷举判定表 + 内容指纹 + 只覆盖内容字段

`MaxFactsPerSession = 30`（`memory.go:20`），`evictLowScoreFacts` 在 `Merge`（`memory.go:711`）里执行。存在第三个写入方（memory builtin tool + IncrementCounts），整体赋值会丢新增、丢计数、删冲突 fact。

穷举判定表（§4.4 流程 C）按 `(in Pre, in Post, 当前存在, 指纹匹配)` 10 行穷举，遍历集合 = `Pre ∪ Post ∪ current`。冲突检测用**内容指纹**而非时间戳。恢复时只覆盖 Content/Category/Confidence，计数保留当前值。

**Save 与 Insert/Delete 同事务**：refine 路径用 `SaveWithRefinement`，rollback 路径用 `SaveWithRollback`，避免中途失败留下不一致状态。

**不支持 RefinementStore 时退回 UpdateWith**：探测不到不报错、不停提取。生产只有 SQLite，但 fake storage 的集成测试语义不被静默改变。

### 7.2 auto-refine gate 取代无条件节流，gate 只跑一次

**为什么不挂 turn 循环**：`buildTurnInjection` 每个 Run 只算一次（`react.go:421`），`promptbuild.go:125-150` 把"per-Run 一次"定为显式不变量（M4-2 prefix 缓存）。

**为什么 gate 取代而非并存**：`repl.go:780-785` 已每 5 轮无条件两次 `ScheduleUpdateWith`。gate 并存则双跑。

**为什么 gate 只跑一次**：session 和 user scope 各跑一次 gate 会让成本翻倍。gate 只在 session job 跑，verdict 通过 token 传递给 user scope job。

**成本条件**："净成本为负"取决于拒绝率。省下的 = C(提取)·(1−P(通过)) − C(gate)。gate 输入与提取器几乎一致，C(gate) ≈ 0.6–0.8·C(提取)，拒绝率需 > 60–80% 才回本。这是待验证假设——gate 决策加 debug 日志（approve/reject + rationale），上线后实测。若拒绝率偏低，gate 价值退回"过滤噪声 Fact、保护 30 槽预算"，也是正当理由。

### 7.3 rollback 冲突检测：内容指纹 + 归一化不变量

> **不变量**：任何用于跨时刻比较的值，两侧必须经过同一条归一化/持久化路径。

`Merge` 给每条被更新 fact 设 `UpdatedAt=now`（`memory.go:690`）。v2 用 PreSnapshot 时间戳作基线，refine 自己改的 fact 永远比 PreSnapshot 新。v3/v4 改用 PostTimestamps 方向正确，但 `formatDBTime`（`sqlite.go:318`）截断到整秒，时间戳跨存储往返有损。v5 改用 Content 指纹方向正确，但只覆盖 Content 且未归一化——`prepareFact`（`storage.go:424`）trim Content/Category、clamp Confidence，内存值与 Load 回来的归一化值可能不同（偶发失配）。

v6 指纹覆盖 rollback 会覆盖的**全部字段**（Content+Category+Confidence），且内部做与 `prepareFact` 对齐的归一化（`strings.TrimSpace` + clamp）。只有第三方在 refine 之后改了这些字段，当前指纹才会 != Post[id]。

### 7.4 scope key 分片不变量

> **不变量**：一个 job 的 `sessionID` 字段必须等于它操作的 scope 的存储键。

dedup、cancel、flushVersion 三套机制**全部按 scope key 分片**：

- `captureFlushVersion(sessionID)`（`queue.go:200`）：key = `"update:"+sessionID`
- `isFlushStale(sessionID, captured)`（`queue.go:210`）：key = `"update:"+sessionID`
- `cancelPending(key)`（`queue.go:175`）：只 bump `strings.HasPrefix(key, "update:")` 的版本
- `CancelPendingUpdates(sessionID)`（`memory.go:130`）：`cancelPending("update:"+sessionID)`，只针对单个 sessionID

v4 把 session 和 user scope bundle 进一个会话键的 job 违反了这条不变量：user scope 拿会话的版本号比 user-scope 计数器 → 恒判陈旧 → 永不保存；compaction 的 `CancelPendingUpdates(sessionID)` 连带取消 user scope 提取。v5+ 拆成两个 job，各用自己的 scope key，三套机制各自正确。

### 7.5 三条静默 return 不产生幽灵记录

`len(filteredMessages)==0`（锁外早退）、`isFlushStale`、`len(changedIDs)==0` 都返回 `saved=false`，不插入记录。flush-stale 正是 compaction 触发的——compaction 取消未完成 refine 的实际效果是：提取被跳过，不记录。

### 7.6 flushVersion ctx 注入不变量

> **不变量**：任何依赖 flushVersion 的 job type，其 executor case 必须调 `withCapturedFlushVersion`。

`withCapturedFlushVersion`（`queue.go:161`）是逐 case 调用的（`queue.go:280/288/296`），9 个 job type 里只有 3 个用。`jobRefine`/`jobRefineApproved` 的 case 若漏掉这行，`capturedFlushVersionFromContext` 取不到值 → `RefineAndRecord` 退回锁内自行 `captureFlushVersion`（执行时刻而非入队时刻）→ flush-stale 保护静默失效。这与 v4-A2 是同一个失效模式，只是换了一层：队列层把 key 分片修对了，ctx 注入这一环还得手动接上。executor 规格里必须写死这行。

### 7.7 rollback 自身可逆

rollback 记录必须填 `PostFactFingerprints`：回滚这条记录时判定表的 Post 列需要非空，否则当前 fact 全落"保留"、Pre 全落"恢复"——结果是并集，永远不删，不是逆操作。rollback 完成后计算 `postFP = factFingerprints(doc.Facts)` 写入 rollback 记录，与 Save 同事务（`SaveWithRollback`）。误操作后可再回滚。

### 7.8 Reviewer / RefinementStore 可选 interface，不支持时退回

`Service` 只持有 `Extractor`（`memory.go:82`）。gate 需要独立 `Reviewer` interface，`*LLMClient` 实现；`reviewer==nil` 时退回无条件提取（不停止提取）。`RefinementStore` 同理，探测不到时 `RefineAndRecord` 退回 `UpdateWith`（不停止提取）。`Storage` 的 6 个现有方法不动，测试桩不断。

### 7.9 user scope 的记录归属

`UserScope(uid).Key()`（`scope.go:53`）与 session 文档共用 `memories` 表。`memory_refinements` 按 `session_id` 存储——user scope 的记录存 `session_id = UserScope(uid).Key()`，所有会话可见。`RefinementRecord.SessionID` 直接存存储键原值。

### 7.10 为什么不引入第二个提取 prompt

现有 `LLMClient.ExtractUpdate`（`llm.go`）用 `MemoryUpdateSystemPrompt` 提取 Fact，写入同一个 30 槽预算。引入第二个提取 prompt 会产生两个提取器抢槽位、互相驱逐、内容重叠。gate 是唯一新增的 LLM 调用，提取完全复用 `ExtractUpdate` → `Merge`。


### 7.11 跨分片依赖必须 fail-open（v6 阻断 A2 的根因）

> **不变量**：跨 job 的依赖必须与 cancel/dedup 的分片粒度一致；当依赖跨越分片时，被依赖方**缺失**必须 fail-open，不能 fail-closed。

§7.4 的不变量管的是"一个 job 的 key 要对应它操作的 scope"。v6 满足了它，却仍然踩坑：`jobRefineApproved` 通过 verdict token 依赖 `jobRefine`，而这两个 job 分属**不同分片**（`"update:"+sessionID` vs `"update:"+UserScope.Key()`）。`CancelPendingUpdates(sessionID)` 只作用于前者 → `jobRefine` 被跳过、verdict 从不写入 → `jobRefineApproved` 若把"缺失"当拒绝，user scope 的提取就白白丢了，而 compaction 的同步 flush 只补 session scope。

区分两种缺失是关键：**条目存在且为 false** 是一个真实的决策（gate 拒绝了），**条目不存在**只说明依赖方没跑过，不含任何决策信息——后者必须退回"没有这个特性时的行为"。这与 §7.8（`reviewer == nil` / 非 `RefinementStore` backend 都退回无条件提取）是同一条原则，只是从"能力探测"推广到了"运行时依赖"。

### 7.12 verdictStore 的生命周期

`verdictStore sync.Map[string]gateVerdict` 只有一个正常消费者（`jobRefineApproved` 的 `LoadAndDelete`），但有两条必然泄漏的路径：`jobRefineApproved` 被 dedup 顶掉（新一轮的 approved 替换 pending 的旧 approved，`queue.go:249-255`），或被 `CancelPendingUpdates(userScopeKey)` 取消。这两种情况下条目永远没人读、没人删，每个 refine 周期泄漏一条。

两层处理：(1) 消费用 `LoadAndDelete` 而非 `Load`；(2) `gateVerdict` 带 `CreatedAt`，`ScheduleRefine` 每次插入前顺手清掉超过 10 分钟的条目——上界远大于 `defaultUpdateTimeout`（5 分钟，`memory.go:16`），不会误删在途的。不引入独立的清理 goroutine。

---

## 八、风险与缓解

| 风险 | 缓解 |
|---|---|
| LLM gate 返回非 JSON | 复用 `llm.go` 的 `extractJSON`（已处理 fenced/brace-slicing） |
| auto-refine 产生噪声 Fact | gate 过滤 + Fact 已有 `SuspectCount` 降权机制 |
| 并发写 Document | `RefineAndRecord` 锁内原子（§7.4）+ `UpdateQueue` 串行化 |
| gate 成本未回本 | 拒绝率实测（§7.2）；gate 价值退回"过滤噪声"亦正当 |
| rollback 与 agent tool 写入冲突 | 内容指纹冲突检测，穷举判定表"保留"行（§7.3） |
| rollback 抹掉反馈计数 | 只覆盖内容字段，计数保留当前值（§7.1） |
| rollback 误操作 | rollback 记录填 Post，自身可逆（§7.7） |
| ghost 记录（提取未 Save） | `saved bool` 返回值，false 不记录（§7.5） |
| Save 与 Insert/Delete 不一致 | `SaveWithRefinement`/`SaveWithRollback` 同事务（§7.1） |
| user scope 提取停止 | 两 job 各用 scope key（§7.4） |
| flush-stale 防护失效 | 两 job 走 default dedup key + executor 显式调 `withCapturedFlushVersion`（§7.6） |
| 指纹跨归一化往返失配 | 指纹覆盖全部覆盖字段 + 内部归一化（§7.3） |
| 不支持 RefinementStore 停提取 | 退回 UpdateWith（§7.1/§7.8） |
| 手动 /refine 阻塞 REPL | 进度提示（§4.4 流程 B） |
| `/refine list` 子命令歧义 | 保留字规则（§4.8） |
| memory_refinements 无限增长 | keep-last-N（默认 50） |
| 同毫秒撞主键 | ID 用纳秒（§4.3） |
| 配置零值语义歧义 | 反转命名 + `resolveRefineInterval`（§5） |
| **缺省配置下提取彻底停止** | interval 过 resolver（0→5），关闭分支退回无条件提取（§5、§4.4 流程 A） |
| **compaction 取消 session job 连带跳过 user scope** | verdict 缺失 fail-open（§7.11） |
| **verdictStore 泄漏** | `LoadAndDelete` + 插入时清理超期条目（§7.12） |
| **rollback 锁外 load-modify-save** | `Service.RollbackRefinement` 锁内（§4.6） |
| 成对回滚部分失败 | 如实报告，不自动补偿；两条各自独立可重试（§4.8） |
| 两 scope 记录的读侧语义 | `PairID` 成对 undo + `list` 归并标注 scope（§4.8） |

---

## 九、与 prime-agent 的对应关系

| prime-agent | deepai 移植 | 说明 |
|---|---|---|
| `REFINEMENT_SYSTEM_PROMPT`（提取） | **复用** `MemoryUpdateSystemPrompt`（`llm.go`） | 不引入第二个提取 prompt |
| `AUTO_REFINE_REVIEW_SYSTEM_PROMPT`（gate） | review gate prompt（§4.5） | 唯一新增 prompt |
| `planRefinement` + `applyRefinementProposal` | **复用** `ExtractUpdate` → `Merge`，封装为 `RefineAndRecord`（锁内原子） | refine 不重新提取 |
| `reviewAutoRefine` | `Reviewer.ReviewRefine` | 原样移植 |
| `rollbackProposal`（逆向 edit） | `RollbackFacts`（穷举判定表 + 内容指纹 + 内容字段覆盖） | 用快照 + 指纹冲突检测替代逆向 edit |
| `harness_state.json` | `memory_facts` 表 + `memory_refinements` 表 | 关系式存储，显式建表 |
| `formatHarnessStateForPrompt` | 无需新增 | 复用 `InjectWithContext` |
| `refine.run`（agent 调用） | 已有等价：`memory` builtin tool | agent 早有写路径 |
| local/global scope | session/user scope | 复用现有；记录归属见 §7.9 |
| daemon 序列化 refine | 无（deepai 无 daemon） | plan/apply 无需分离 |

---

## 十、修订记录

- **v7**（本版）：修正 v6 的 3 个阻断级问题：
  - A1 `MemoryRefineInterval` 缺省为 0 撞上 `interval > 0` 守卫 → 缺省配置下提取彻底停止。加 `resolveRefineInterval`（0→5，-1→禁用，照 `resolveRequestTimeout` 先例），补上"auto-refine 关闭 → 退回原两次 `ScheduleUpdateWith`"的分支（v6 只在 §5 声称、流程图里没有），`memoryExtractInterval` 改名保留而非删除
  - A2 compaction 的 `CancelPendingUpdates(sessionID)` 取消 `jobRefine` 后，`jobRefineApproved` 读不到 verdict 而跳过 → user scope 提取丢失（v4-A3 换路径重现）。verdict **缺失** fail-open、**存在且 false** 才跳过；新增 §7.11 不变量
  - A3 rollback 的 load-modify-save 在锁外，且 v6 把它编排在 `pkg/chat`（`getSessionLock` 未导出，结构上拿不到锁）→ 新增 `Service.RollbackRefinement` 锁内入口，`RollbackFacts` 保持纯函数

  3 个设计问题：
  - B1 读侧 scope 语义没跟上写侧 fan-out → `PairID` 成对 undo、`list` 归并标注、`rollback <id>` 明确只动单条，并写明成对回滚的部分失败行为
  - B2 `verdictStore` 无清理路径 → `LoadAndDelete` + 插入时清理超期条目（§7.12）
  - B3 判定表两行都叫"恢复"语义却不同 → 拆为"就地恢复"（只覆盖内容字段）与"整条重插"（含快照计数）

  4 个小问题：C1 重插的 `UpdatedAt` 设为 now（否则回滚完立刻被 recency 衰减赶走）、C2 `%f` 容差写明意图、C3 gate 报错也 fail-open、C4 `Rationale` 各路径取值表。

  新增两条不变量（§7.11 跨分片依赖 fail-open、§7.12 verdictStore 生命周期），并新增 `GetRefinement` 到 `RefinementStore`（rollback 需要锁内按 id 查）。

- **v6**：修正 v5 的 3 个阻断级问题：
  - A1 判定表缺"当前是否存在"维，3 子情形错误 → 改为 `(in Pre, in Post, 当前存在, 指纹匹配)` 10 行穷举表，遍历集合 = `Pre ∪ Post ∪ current`
  - A2 指纹未归一化、只覆盖 Content → 覆盖 Content+Category+Confidence，内部做与 `prepareFact` 对齐的归一化
  - A3 executor 未显式调 `withCapturedFlushVersion` → executor 规格写死，新增 §7.6 不变量
  
  3 个设计问题：
  - B1 ScheduleRefine 三处说法不一致 + 重入 submit 隐患 → 统一为"两个都提前入队"+ token 传递 verdict
  - B2 配置零值语义歧义 → 反转命名（`MemoryAutoRefineDisabled`）+ 哨兵值（interval: -1）
  - B3 fallback 路径 saved 撒谎 → 保守返回 false
  
  4 个小问题：C1 HasRefinementStore 用途说明、C2 timeout 一个预算写明、C3 手动 /refine 进度提示、C4 interval>0 守卫。
  
  新增两条不变量表述（§7.3 归一化不变量、§7.6 flushVersion ctx 注入不变量），把"修对了方向但没修到边界"这类问题写成规则。

- **v5**：修正 v4 的 3 个阻断级问题（PostFactTimestamps 跨整秒截断改内容指纹、fan-out 与 default dedup key 冲突拆两 job、compaction 连带取消 user scope）。新增 §7.4 scope key 分片不变量。

- **v4**：修正 v3 的 4 个阻断级问题（user scope fan-out、dedup key flushVersion、三集合判定表、Reviewer interface）和 4 个设计问题。

- **v3**：修正 v2 的 4 个阻断级问题（PostFactTimestamps 基线、三集合合并、锁内原子 RefineAndRecord、saved bool 防幽灵记录）和 5 个设计缺口。

- **v2**：修正 v1 的 3 个阻断级问题（SQLite 关系式存储、buildTurnInjection per-Run 不变量、裸 goroutine）和 6 个设计缺口。根本方向调整：围绕现有 `ExtractUpdate` 构建控制层。

- **v1**：初版，基于 prime-agent 直接移植，未核实 deepai 存储架构和注入不变量。
