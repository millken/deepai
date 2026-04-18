# DeepAI Session 管理设计

> 日期：2026-04-18
> 参考：hermes-agent v2026.4.16 session 子命令
> 范围：CLI 会话管理的存储层、子命令、REPL 增强

## 一、现状分析

### 1.1 当前实现

deepai 的 session 管理目前是**最小可用状态**：

| 维度 | 现状 |
|------|------|
| 存储方式 | JSON 文件，`~/.deepai/sessions/<ID>.json` |
| Session 模型 | `chat.Session`（ID, CreatedAt, Title, Messages, Metadata） |
| ID 格式 | 时间戳 `20060102_150405`，无随机后缀，同秒冲突会覆盖 |
| 子命令 | **无**，仅有 root/chat 上的 `-r`/`-c` flag |
| REPL 斜杠命令 | `/clear`、`/history`、`/exit` |
| 列表/删除/搜索 | 均不支持 |
| Title | 字段存在但从未被赋值 |

### 1.2 存在两套 Session 模型

| 模型 | 位置 | 用途 | 存储 |
|------|------|------|------|
| `chat.Session` | `pkg/chat/session.go` | CLI REPL | JSON 文件 |
| `models.Session` | `pkg/models/session.go` | Gateway/Server | PostgreSQL（checkpoint） |

两套模型字段不互通，CLI 路径完全不使用 `models.Session`。

### 1.3 数据库碎片化问题

当前各模块各用各的存储，互不打通：

| 模块 | 存储 | 数据库 |
|------|------|--------|
| Memory（CLI 默认） | `~/.deepai/memory.db` | SQLite |
| Memory（Server） | 远程 PG | PostgreSQL |
| Session（CLI） | `~/.deepai/sessions/*.json` | 文件 |
| Session（Gateway） | 远程 PG | PostgreSQL（checkpoint） |

问题：同一用户的会话和记忆被分散在不同数据库中，无法跨模块查询。开发阶段直接统一，不保留旧路径兼容。

### 1.4 可用基础设施

- **SQLite 驱动**：`modernc.org/sqlite`（已在 go.mod，memory 模块使用）
- **PostgreSQL 驱动**：`jackc/pgx/v5`（已在 go.mod，checkpoint 使用）
- **SessionStore**：`ListRecent()` 已实现但 CLI 未调用（死代码）
- **REPL 斜杠命令框架**：`handleSlashCommand()` 已有 switch 分发
- **`memory.OpenStore()`**：已有 SQLite/PG 自动选择逻辑，可复用

## 二、目标

借鉴 hermes 的 session 管理，为 deepai CLI 新增 `session` 子命令组，同时将存储层统一到单一数据库，增强 REPL 内会话操作。

**设计原则：**
1. **单一数据库**：session、message、memory 全部存入同一个数据库实例。CLI 默认 SQLite（`~/.deepai/deepai.db`），Server/Gateway 用 PostgreSQL。不再出现 SQLite + PG 共存、session 和 memory 各存各的情况。
2. **永久保存**：所有会话和消息永久持久化，**不自动删除、不自动过期、不自动压缩磁盘数据**。只有用户显式执行 `session delete` / `session prune` 才会移除数据。与 Claude Code 的关键区别：Claude Code 会在会话变旧后丢弃历史，deepai 永不丢弃。
3. **上下文压缩 ≠ 数据丢失**：当对话超出模型上下文窗口时，仅对发送给 LLM 的 in-memory 消息做压缩摘要，数据库中的完整原始消息**始终保留不删不改**。压缩摘要仅存在于运行时，不回写数据库。
4. **保持简洁**：不过度设计，优先覆盖高频操作
5. **模型统一**：CLI 复用 `models.Session`，消除两套模型的维护负担

## 三、子命令设计

### 3.1 命令树

```
deepai session list [--limit N] [--source S]
deepai session show <ID|TITLE>
deepai session rename <ID|TITLE> <NEW_TITLE>
deepai session export <OUTPUT> [--session-id ID]
deepai session delete <ID|TITLE> [-y]
deepai session prune [--older-than N] [-y]
deepai session stats
```

**ID/TITLE 参数统一匹配规则（show/rename/delete 共用）：**
1. 输入形如 `\d{8}_\d{6}` → 按 ID 精确匹配
2. 否则按标题匹配：精确 → 前缀 → 模糊（LIKE %query%）
3. 精确匹配命中 1 个 → 直接使用
4. 多个匹配时：
   - `show`/`rename`：取 `updated_at` 最新的
   - `delete`：列出所有候选，交互确认选择（避免误删）
5. 无匹配时报错并列出最近会话供参考

### 3.2 各命令详细设计

#### `session list`

列出最近会话，表格输出。

```
$ deepai session list
ID                     TITLE              MSGS  CREATED
20260418_143022_b7c2   调试并发 bug        12    2026-04-18 14:30
20260417_091503_a1f3   重构 auth 模块      45    2026-04-17 09:15
20260416_201000_e9c4   查询 go.mod 依赖     3    2026-04-16 20:10
```

**Flags：**
- `--limit N`（默认 20）— 返回数量
- `--source S`（预留）— 按来源过滤（当前全部为 "cli"）

**实现要点：**
- SQL: `SELECT s.id, s.title, COUNT(*) AS msg_count, s.created_at FROM sessions s LEFT JOIN messages m ON m.session_id = s.id GROUP BY s.id ORDER BY s.updated_at DESC LIMIT ?`
- 标题是必填项，所有会话均有标题（首轮对话后自动生成）
- 可选 `--verbose` 显示 model 列

#### `session show <ID>`

显示会话详情。

```
$ deepai session show 重构auth
ID:         20260417_091503_a1f3
Title:      重构 auth 模块
Created:    2026-04-17 09:15:03
Messages:   45
Model:      claude-sonnet-4-20250514

--- Last 5 messages ---
[human] 帮我重构 auth 模块，把 session 中间件拆出来
[ai]    好的，我先看一下当前的 auth 模块结构...
[human] 可以，开始吧
[ai]    已完成重构，主要变更...
[human] 看起来不错，提交一下
```

**实现要点：**
- 默认显示最近 5 条消息，`--full` 显示全部
- SQL: `SELECT * FROM messages WHERE session_id = ? ORDER BY seq DESC LIMIT 5`（查询倒序取最新，显示时反转为正序）

#### `session rename <ID> <TITLE>`

设置会话标题。

```
$ deepai session rename 调试并发 "修复并发竞态条件"
Session "调试并发 bug" renamed to "修复并发竞态条件"
```

**实现要点：**
- SQL: `UPDATE sessions SET title = ? WHERE id = ?`
- 支持按标题恢复时使用：`deepai -r "调试并发 bug"`

#### `session export <OUTPUT>`

导出会话为 JSONL 格式。

```
$ deepai session export ./backup.jsonl --session-id 20260417_091503
Exported 45 messages from session 20260417_091503

$ deepai session export -                    # stdout，导出全部会话
```

**Flags：**
- `--session-id ID` — 指定会话（默认全部）
- `-` 表示 stdout

**JSONL 格式：** 每行一个完整 `models.Message` JSON 对象，不做字段裁剪，确保 export/import 可还原完整会话。

#### `session delete <ID>`

删除指定会话。

```
$ deepai session delete 查询go
Delete session "查询 go.mod 依赖" (3 messages)? [y/N] y
Deleted.

$ deepai session delete 查询go -y    # 跳过确认
Deleted.
```

**实现要点：**
- SQL: `DELETE FROM sessions WHERE id = ?`（messages 通过 `ON DELETE CASCADE` 自动级联删除）
- `-y` 跳过确认提示

#### `session prune`

清理旧会话。

```
$ deepai session prune --older-than 90
Found 23 sessions older than 90 days (total 1.2 MB)
Delete all? [y/N] y
Pruned 23 sessions.
```

**Flags：**
- `--older-than N`（默认 90）— 天数
- `-y` 跳过确认

**实现要点：**
- SQL: `DELETE FROM sessions WHERE updated_at < ?`（messages 通过 `ON DELETE CASCADE` 自动级联删除）
- `--dry-run` 只统计不删除

#### `session stats`

显示统计信息。

```
$ deepai session stats
Sessions:     47
Messages:     1,283
Total size:   4.8 MB
Oldest:       2026-02-10
Latest:       2026-04-18 14:30
```

## 四、REPL 增强

### 4.1 新增斜杠命令

| 命令 | 说明 |
|------|------|
| `/new` | 新建会话（当前会话标记 completed，创建新会话） |
| `/title <name>` | 设置当前会话标题 |
| `/save` | 刷新会话元数据到数据库（updated_at、metadata 等）。消息已通过 AppendMessage 实时写入，此命令主要用于确保元数据同步 |
| `/undo` | 撤销最后一轮对话（从最后一条 role=human 向前回溯，移除该 human 及其之后所有消息，包括中间的 tool_call/tool_result/ai） |

### 4.2 自动标题生成（必选）

与 Claude Code 行为一致：**每个会话必有标题，不存在 untitled 状态。**

首轮 AI 回复完成后，立即异步调用 LLM 生成标题，用户无感知。

**生成时机：** 第 1 次 AI 回复完成后（仅触发一次，后续不再覆盖，除非用户 `/title` 手动修改）

**实现要点：**
- 异步执行，不阻塞用户交互（后台 goroutine）
- 使用当前会话模型生成，避免引入额外模型依赖
- 标题上限 30 字符，截断过长内容
- 生成失败时降级为取用户首条消息前 20 字符作为标题，确保总有标题
- 标题写入后立即 `UPDATE sessions SET title = ? WHERE id = ?`

**Prompt 模板（自适应语言）：**
```
Generate a concise title (≤30 chars) in the same language as the user's message. Return only the title text, no quotes or formatting.
User: {first_user_message}
```

**降级逻辑：**
```
LLM 生成成功 → 使用生成标题
LLM 生成失败 → 取 first_user_message[:20] + "..." 作为标题
```

### 4.3 Session 元数据增强

在创建/恢复会话时自动记录到 `Metadata`：

```go
Metadata: {
    "model":    "claude-sonnet-4-20250514",
    "cwd":      "/workspace/Codes/github.com/millken/deepai",
    "provider": "anthropic",
}
```

## 五、ID 格式改进

**当前问题：** `20060102_150405` 格式无随机后缀，同秒创建会覆盖。

**改进方案：** 追加 4 位随机 hex，与 hermes 对齐。

```
格式: 20060102_150405_a3f1
示例: 20260418_143022_b7c2
```

## 六、会话恢复

### 6.1 三种恢复方式

```
$ deepai -r                        # 无参数：弹出交互式选择器
$ deepai -r "重构 auth 模块"        # 有参数：按标题匹配
$ deepai -r 20260417_091503         # 有参数：按 ID 精确匹配
```

### 6.2 交互式选择器（-r 无参数）

终端内渲染会话列表，支持键盘导航和实时搜索过滤：

```
$ deepai -r
? 选择要恢复的会话:
  ▸ 调试并发 bug              12 msgs   2026-04-18 14:30
    重构 auth 模块             45 msgs   2026-04-17 09:15
    查询 go.mod 依赖            3 msgs   2026-04-16 20:10
    实现 session 搜索          28 msgs   2026-04-15 11:22

  筛选: _
  ↑↓ 导航  /  输入筛选  /  Enter 选择  /  Esc 取消
```

**实现要点：**
- 使用 [huh](https://github.com/charmbracelet/huh)（已在项目依赖中，setup 模块使用）
- 按 `updated_at` 倒序排列，默认选中最近会话
- 实时过滤：输入即时筛选标题（支持模糊匹配）
- 选中的会话直接恢复进入 REPL
- 非 TTY 环境（管道、CI）降级为按最近会话自动恢复

### 6.3 按标题/ID 匹配（-r 有参数）

复用 3.1 的 ID/TITLE 统一匹配规则。

## 七、存储层：直接切换 SQLite

### 7.1 为什么不保留文件存储

在 JSON 文件上补全 CRUD + Search/Stats，本质上是在手写一个简陋数据库：
- Delete：遍历找到文件再删
- Rename：读 JSON → 改字段 → 写回
- Search：遍历所有文件，解析 JSON，匹配标题/内容
- Stats：遍历所有文件，解析 JSON，汇总计数

而 SQLite 给出的是：
- 上述所有操作各一条 SQL
- FTS5 全文搜索开箱即用
- WAL 模式支持并发读
- `models.Session` 已有完整验证，直接复用

**选择 SQLite 而非 PostgreSQL 的原因：** CLI 场景不需要远程连接、连接池、事务隔离级别，SQLite 零配置、单文件、纯 Go 驱动（`modernc.org/sqlite` 无 CGO），更适合桌面工具。Server/Gateway 场景仍可配置 PostgreSQL。

### 7.2 单一数据库

**消除数据库碎片化，所有数据统一到一个数据库实例：**

| 场景 | 数据库路径 | 说明 |
|------|-----------|------|
| CLI（默认） | `~/.deepai/deepai.db` | SQLite，session + message + memory 全部在此 |
| Server/Gateway | 用户配置的 `database_url` | PostgreSQL，同一库包含所有表 |

**配置方式：**
- `config.yaml` 中的 `database_url` 字段复用，CLI 默认值改为 `~/.deepai/deepai.db`
- 项目处于开发阶段，不考虑旧数据迁移

### 7.3 统一 Schema

session、message、memory 全部在同一数据库中：

```sql
-- ============ Schema Version ============

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);
-- 初始化：INSERT INTO schema_version VALUES (1);

-- ============ Session & Message ============

-- sessions 表
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    user_id     TEXT DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    model       TEXT DEFAULT '',
    cwd         TEXT DEFAULT '',
    source      TEXT DEFAULT 'cli',
    state       TEXT DEFAULT 'active',   -- active / completed
    created_at  REAL NOT NULL,            -- Unix timestamp (秒)
    updated_at  REAL NOT NULL,
    metadata    TEXT DEFAULT '{}'          -- JSON
);

-- messages 表（禁止 WITHOUT ROWID，FTS5 依赖隐式 rowid）
CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,         -- 格式: {session_id}_{seq}
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,         -- 会话内序号，用于排序
    role        TEXT NOT NULL,             -- human / ai / system / tool
    content     TEXT DEFAULT '',
    tool_calls  TEXT DEFAULT '[]',         -- JSON
    tool_result TEXT DEFAULT '',           -- JSON
    created_at  REAL NOT NULL,            -- Unix timestamp (秒)
    UNIQUE(session_id, seq)
);

-- FTS5 全文索引（仅索引 human/ai 消息，跳过 system/tool）
-- messages.id 为 TEXT PRIMARY KEY，SQLite 会自动分配隐式 rowid，FTS5 通过 rowid 关联
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content=messages,
    content_rowid=rowid
);

-- FTS 同步触发器（条件过滤：只索引 human/ai 消息）
-- 注意：SQLite CREATE TRIGGER 不支持 IF 语句，UPDATE 拆为两个 WHEN 触发器
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages WHEN new.role IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages WHEN old.role IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au_del AFTER UPDATE ON messages WHEN old.role IN ('human', 'ai') AND new.role NOT IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au_ins AFTER UPDATE ON messages WHEN new.role IN ('human', 'ai') BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

-- ============ Memory ============

CREATE TABLE IF NOT EXISTS memories (
    session_id text primary key,
    user_memory text not null default '{}',
    history_memory text not null default '{}',
    source text not null default '',
    updated_at real not null             -- Unix timestamp (秒)
);

CREATE TABLE IF NOT EXISTS memory_facts (
    session_id text not null references memories(session_id) on delete cascade,
    id text not null,
    content text not null,
    category text not null default '',
    confidence real not null default 0,
    source text not null default '',
    retrieval_count integer not null default 0,
    helpful_count integer not null default 0,
    created_at real not null,            -- Unix timestamp (秒)
    updated_at real not null,
    primary key (session_id, id)
);

-- ============ Indexes ============

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC);
```

### 7.4 SessionRepository 接口

抽取接口，上层代码依赖接口而非具体实现，与 `memory.Store` 模式一致：

```go
// pkg/chat/session.go

type SessionRepository interface {
    Create(opts CreateOpts) (*models.Session, error)
    Load(id string) (*models.Session, error)
    Save(sess *models.Session) error
    AppendMessage(sessionID string, msg models.Message) error
    Delete(id string) error
    Rename(id, title string) error
    ListRecent(limit int) ([]SessionMeta, error)
    Search(query string, limit int) ([]SessionMeta, error)
    Stats() (SessionStats, error)
    Prune(olderThanDays int, dryRun bool) (int, error)
    ExportSession(id string) ([]models.Message, error)
    ExportAll() ([]SessionExport, error)
    Resolve(input string) (*models.Session, error)  // ID/TITLE 统一匹配
    Latest() (*models.Session, error)
    Close() error
}

// SQLite 实现
type SQLiteSessionStore struct {
    db *sql.DB
}

func NewSQLiteSessionStore(dbPath string) (*SQLiteSessionStore, error)
func (s *SQLiteSessionStore) Migrate() error  // 按 schema_version 递增执行 DDL
```

`ChatRepl` 和 commands 层均持有 `SessionRepository` 接口，未来切换 PG 只需新增 `PgSessionStore` 实现。

**注意：** `AppendMessage` 直接写入数据库，不调用 `models.Message.Validate()` 全量校验。REPL 运行时某些消息（如空 content 的 system 消息、只有 error 的 tool_result）可能不满足 Validate 的严格要求，但写入数据库是安全的。Validate 留给 import/export 等数据完整性敏感场景使用。

**seq 生成策略：** `AppendMessage` 在事务内执行 `SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ? FOR UPDATE`，配合 `UNIQUE(session_id, seq)` 约束防止并发重复。CLI 单进程场景下并发风险极低，但事务保护是防御性编程。

### 7.5 FTS5 搜索

```go
func (s *SQLiteSessionStore) Search(query string, limit int) ([]SessionMeta, error) {
    // FTS5 查询：支持关键词、短语、布尔运算
    // 只搜索 human/ai 消息（FTS 触发器已过滤）
    // sql := `SELECT s.id, s.title, COUNT(m.id), s.updated_at
    //         FROM sessions s
    //         JOIN messages m ON m.session_id = s.id
    //         JOIN messages_fts fts ON fts.rowid = m.rowid
    //         WHERE messages_fts MATCH ?
    //         GROUP BY s.id ORDER BY rank LIMIT ?`
}
```

## 八、模型统一

消除 `chat.Session`，CLI 直接使用 `models.Session` + `models.Message`：

**需要的改动：**
- `models.Session` 新增 `Title` 字段
- `models.Session.Validate()` 中 `UserID` 改为可选（CLI 场景无 UserID 概念，Gateway 场景由上层填入）
- `chat.SessionStore` 的返回类型改为 `models.Session`

**UserID 处理：**
- CLI 场景：`UserID` 留空（`""`），`Validate()` 允许空值
- Gateway 场景：由上层代码填入实际用户 ID
- 这样 CLI 和 Gateway 共用同一模型，无需"默认值" hack

**好处：**
- CLI 和 Gateway 共用同一套模型和验证逻辑
- 未来切换 PostgreSQL 只需换 Store 实现，不动上层代码

## 九、实现清单

| # | 文件 | 改动 |
|---|------|------|
| 1 | `pkg/models/session.go` | 新增 `Title` 字段；`Validate()` 中 `UserID` 改为可选 |
| 2 | `pkg/chat/session.go` | **重写**：抽取 `SessionRepository` 接口 + `SQLiteSessionStore` 实现（Migrate/CRUD/Search/Export/Prune） |
| 3 | `pkg/chat/repl.go` | 适配 `SessionRepository` 接口；`resolveSession()` 改造适配新接口签名；新增 `/new`、`/title`、`/save`、`/undo` |
| 4 | `pkg/commands/session.go` | **新增**：`session` 子命令组（list/show/rename/export/delete/prune/stats），子命令依赖 `SessionRepository` 接口 |
| 5 | `pkg/commands/commands.go` | 注册 `addSession(topLevel)` |
| 6 | `pkg/commands/chat.go` | `-r` 无参数弹出 UI 选择器，有参数按标题/ID 匹配；会话创建时写入 Metadata |
| 7 | `pkg/chat/title.go` | **新增**：自动标题生成逻辑（首轮对话后调用） |
| 8 | `pkg/chat/picker.go` | **新增**：交互式会话选择器（huh），支持键盘导航 + 实时过滤 |
| 9 | `pkg/commands/paths.go` | 新增 `DBFile()` 返回 `~/.deepai/deepai.db` |
| 10 | `pkg/memory/store.go` | `OpenStore()` 默认路径改为 `deepai.db`，不再创建独立的 `memory.db` |
| 11 | `pkg/memory/sqlite.go` | schema SQL 移除（由 session.go 统一建表）；`formatDBTime`/`parseDBTime` 从 RFC3339Nano 改为 Unix timestamp（`t.Unix()` / `time.Unix(sec, 0)`） |

### 不在本次范围

- Gateway session 管理（已有 checkpoint 模块，后续统一到同一 schema）
- 自动压缩/分支（compaction 仅影响运行时上下文，不涉及持久化层，属于 agent 核心逻辑）
