# DeepAI 上下文分层压缩实施计划

**状态**: 🟡 讨论中  
**日期**: 2026-08-01  
**基于**: docs/context-compression-design.md  
**当前进展**: Token 计量已完成 (3.3 bytes/token)

---

## 📋 总体目标

实施四层防护架构 (L0-L3)，解决长会话上下文增长问题，确保：
1. 长会话水位受控，不触顶 (context overflow)
2. 压缩代价可接受，不引发显著重读
3. 缓存友好，最小化 KV/prompt cache 失效
4. 可度量，基于 tokens 压力判断

---

## 🎯 实施原则

1. **ROI 优先**: 先实施低成本高收益的改进
2. **数据驱动**: 基于 Phase 0 metrics 和 replay 评估
3. **渐进式**: 先便宜的层，后贵的层
4. **可回滚**: 每个里程碑都有独立的验收标准

---

## 📊 里程碑规划

### M1: 基础设施完善 (Week 1-2)
**优先级**: 🔴 **最高** - 其他里程碑的依赖

#### M1.1 Token 计量增强 ✅ (已完成)
- [x] 实现 3.3 bytes/token 估算
- [x] 添加 provider usage 调试日志
- [x] 添加校准测试套件
- **状态**: 已完成并提交 (695bd80)

#### M1.2 Metrics 增强
**目标**: 支持重读率监控和压缩效果评估

**新增字段**:
```go
type ToolResultMetric struct {
    Turn        int    `json:"turn"`
    ToolName    string `json:"tool_name"`
    ResultBytes int    `json:"result_bytes"`
    // 新增字段
    ArgsHash    string `json:"args_hash"`    // 工具参数哈希
    Path        string `json:"path"`         // 文件路径 (如适用)
    Offloaded   bool   `json:"offloaded"`    // 是否触发落盘
    DurationMs  int64  `json:"duration_ms"`  // 工具执行耗时
}
```

**实施步骤**:
1. 更新 `pkg/agent/metrics.go` 中的 `ToolResultMetric` 结构
2. 在 `pkg/agent/react.go` 工具调用处收集新字段
3. 更新 `pkg/agent/metrics_file.go` 写入逻辑
4. 添加单元测试验证新字段收集

**验收标准**:
- [ ] 所有工具调用都记录 `args_hash`
- [ ] 文件类工具记录 `path`
- [ ] offload 功能实现后正确标记 `offloaded`
- [ ] 现有测试通过，新测试覆盖新增字段

**预计工作量**: 2-3 天

---

### M2: L0 源头防护 (Week 2-3)
**优先级**: 🟢 **高** - ROI 最高，47% 字节来自 2.4% 大结果

#### M2.1 git_diff --stat 默认化
**目标**: 将 git_diff 从默认完整 diff 改为 --stat 格式

**实施步骤**:
1. 修改 `pkg/tools/builtin/git.go` 中的 `GitDiffHandler`
2. 添加 `format` 参数支持: `{"format": "stat"|"full"}`
3. 默认行为改为 `--stat`，agent 需要细节时显式请求 `format: "full"`
4. 更新工具描述文档

**代码变更**:
```go
// GitDiffHandler shows the diff of changes
func GitDiffHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
    args := call.Arguments
    dir := workingDirFromArgs(args)
    staged, _ := args["staged"].(bool)
    
    // 新增: 支持格式选择
    format := "stat" // 默认 stat
    if f, ok := args["format"].(string); ok && f == "full" {
        format = "full"
    }
    
    if format == "stat" {
        // 默认只返回 stat 格式
        statArgs := []string{"diff", "--stat"}
        if staged {
            statArgs = []string{"diff", "--cached", "--stat"}
        }
        statOutput, err := gitCmd(ctx, dir, statArgs...).CombinedOutput()
        if err != nil {
            return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("git diff --stat failed: %w", err)
        }
        
        // 获取文件列表
        nameArgs := []string{"diff", "--name-only"}
        if staged {
            nameArgs = []string{"diff", "--cached", "--name-only"}
        }
        nameOutput, _ := gitCmd(ctx, dir, nameArgs...).CombinedOutput()
        
        result := map[string]any{
            "staged": staged,
            "format": "stat",
            "files":  splitLines(string(nameOutput)),
            "stats":  strings.TrimSpace(string(statOutput)),
            "note":   "Full diff available with format: \"full\"",
        }
        data, _ := json.Marshal(result)
        return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data), Data: result}, nil
    }
    
    // 原有的完整 diff 逻辑
    // ...
}
```

**验收标准**:
- [ ] 默认调用返回 stat 格式，字节减少 >80%
- [ ] 显式 `format: "full"` 返回完整 diff
- [ ] 工具描述更新，说明 drill-down 能力
- [ ] 现有测试通过

**预计工作量**: 1 天

#### M2.2 grep 结果上限
**目标**: 防止极端大结果 (471KB)，添加合理上限

**实施步骤**:
1. 修改 `pkg/tools/builtin/grep.go` 中的默认参数
2. 添加 `head_limit` 参数: 保留前 N 个匹配的完整上下文
3. 超过上限时返回截断标记和总匹配数

**代码变更**:
```go
// grep.go 默认参数调整
const (
    GrepMaxResults = 200      // 降低默认上限 (当前可能更高)
    GrepHeadLimit  = 100     // 新增: 前 100 个匹配保留完整上下文
)

// GrepHandler 中的逻辑
if len(matches) > GrepHeadLimit {
    // 保留前 GrepHeadLimit 个完整匹配
    fullMatches := matches[:GrepHeadLimit]
    // 后续只保留文件名和行号
    partialMatches := make([]grepMatch, len(matches)-GrepHeadLimit)
    for i, match := range matches[GrepHeadLimit:] {
        partialMatches[i] = grepMatch{
            File:  match.File,
            Line:  match.Line,
            Content: "... (truncated, " + strconv.Itoa(len(matches)) + " total matches)",
        }
    }
    matches = append(fullMatches, partialMatches...)
}
```

**验收标准**:
- [ ] 默认 `max_results=200`, `head_limit=100`
- [ ] 超过上限时正确标记截断
- [ ] 工具描述说明截断行为
- [ ] 现有测试通过

**预计工作量**: 1 天

#### M2.3 Offload 落盘机制
**目标**: 超大结果 (>24KB) 落盘，上下文只留引用

**实施步骤**:
1. 创建 `pkg/agent/offload.go` 实现落盘逻辑
2. 定义 offload 目录结构: `~/.deepai/offload/<session_id>/<tool_call_hash>.txt`
3. 在工具结果处理处添加大小检查
4. 生成引用+摘要+首尾各 50 行

**代码结构**:
```go
// pkg/agent/offload.go
package agent

import (
    "crypto/sha256"
    "encoding/hex"
    "os"
    "path/filepath"
)

const OffloadThreshold = 24 * 1024 // 24KB

type OffloadMeta struct {
    SessionID   string
    CallID      string
    ToolName    string
    Size        int64
    Hash        string
    Path        string
    Summary     string
    HeadLines   []string
    TailLines   []string
}

func MaybeOffload(content string, meta OffloadMeta) (string, bool) {
    if len(content) <= OffloadThreshold {
        return content, false
    }
    
    // 生成哈希
    hash := sha256.Sum256([]byte(content))
    hashStr := hex.EncodeToString(hash[:])
    
    // 创建 offload 路径
    offloadPath := filepath.Join(os.Getenv("HOME"), ".deepai", "offload", 
                                meta.SessionID, hashStr + ".txt")
    
    // 确保目录存在
    os.MkdirAll(filepath.Dir(offloadPath), 0755)
    
    // 写入完整内容
    if err := os.WriteFile(offloadPath, []byte(content), 0644); err != nil {
        return content, false // 失败时返回原内容
    }
    
    // 生成引用内容
    headLines := firstLines(content, 50)
    tailLines := lastLines(content, 50)
    summary := fmt.Sprintf("[Offloaded: %d bytes, see %s]", len(content), offloadPath)
    
    refContent := summary + "\n" +
                 "=== First 50 lines ===\n" + strings.Join(headLines, "\n") + "\n" +
                 "=== Last 50 lines ===\n" + strings.Join(tailLines, "\n")
    
    return refContent, true
}
```

**验收标准**:
- [ ] >24KB 结果自动落盘
- [ ] 引用内容包含摘要+首尾各 50 行
- [ ] offload 文件可正确读取
- [ ] metrics 正确标记 `offloaded=true`
- [ ] 清理策略实现 (会话结束 7 天后清理)

**预计工作量**: 2-3 天

---

### M3: L1 确定性压缩 + 双阈值迟滞 (Week 3-4)
**优先级**: 🟡 **中** - 依赖 M1 完成

#### M3.1 双阈值迟滞机制
**目标**: 替换单一 `MinContextPressure`，实现分层触发

**实施步骤**:
1. 扩展 `AgingConfig` 结构，添加双阈值
2. 修改 `pkg/agent/aging.go` 中的触发逻辑
3. 实现迟滞下界 (P_target = 0.25)

**代码变更**:
```go
// pkg/agent/aging.go
type AgingConfig struct {
    Enabled bool
    
    // 新增: 双阈值
    SoftPressure float64 // 0.35 - 触发 L1 确定性压缩
    HardPressure float64 // 0.55 - 触发 L2 语义摘要
    CritPressure float64 // 0.90 - 触发 L3 会话重建
    TargetPressure float64 // 0.25 - 压缩到此即停
    
    // 保留兼容性
    MinContextPressure float64 // 已废弃，使用 SoftPressure
    
    ToolResultBudgets map[int]int
    ConversationBudgets map[int]int
}

// 默认配置
const (
    defaultSoftPressure = 0.35
    defaultHardPressure = 0.55
    defaultCritPressure = 0.90
    defaultTargetPressure = 0.25
)
```

**验收标准**:
- [ ] 双阈值正确触发 L1/L2 压缩
- [ ] 压缩到 P_target 即停，不抖动
- [ ] 保留向后兼容性
- [ ] 单元测试覆盖阈值逻辑

**预计工作量**: 2 天

#### M3.2 差异化工具预算表
**目标**: 按工具类型设置不同的 age budget

**实施步骤**:
1. 修改 `defaultToolResultBudgets` 为按工具分组的 map
2. 更新 `buildPromptView` 中的预算查找逻辑
3. 添加工具特定的预算配置

**代码变更**:
```go
// pkg/agent/aging.go
type ToolBudgets map[string]map[int]int // tool_name -> age -> bytes

var defaultToolResultBudgets = ToolBudgets{
    "read_file":  {1: 8192, 2: 2048, 3: 300},
    "bash":       {1: 4096, 2: 1024, 3: 300},
    "edit_file":  {1: 300,  2: 300,  3: 300},
    "write_file": {1: 300,  2: 300,  3: 300},
    "grep":       {1: 4096, 2: 1024, 3: 300},
    "git_diff":   {1: 2048, 2: 1024, 3: 300},
    "web_fetch":  {1: 8192, 2: 2048, 3: 300},
    "default":    {1: 4096, 2: 1024, 3: 300},
}

// 在 buildPromptView 中查找预算
func getToolBudget(toolName string, age int) int {
    if budgets, ok := defaultToolResultBudgets[toolName]; ok {
        // 找到最大的 key <= age
        var maxAge int
        for a := range budgets {
            if a <= age && a > maxAge {
                maxAge = a
            }
        }
        return budgets[maxAge]
    }
    // 回退到默认
    return getToolBudget("default", age)
}
```

**验收标准**:
- [ ] 不同工具使用不同的 age budget
- [ ] read_file age-1 保护 8KB
- [ ] 工具不在表中时使用默认预算
- [ ] 单元测试验证预算查找

**预计工作量**: 1-2 天

#### M3.3 L1 压缩算法实现
**目标**: 实现去重、即时折叠、掐头留尾

**实施步骤**:
1. 实现去重逻辑 (基于 `args_hash`)
2. 实现即时折叠 (edit_file/write_file)
3. 实现掐头留尾 (head 70% + tail 30%)
4. 添加保护清单逻辑

**代码结构**:
```go
// pkg/agent/compression.go
package agent

// L1Compression 确定性压缩
type L1Compression struct {
    DedupMap    map[string]string // args_hash -> 最新内容
    ProtectList []string          // 保护的消息 ID
}

func (c *L1Compression) Deduplicate(messages []models.Message) []models.Message {
    // 去重: 相同 args_hash 的工具结果只保留最新
    // ...
}

func (c *L1Compression) FoldConfirmations(messages []models.Message) []models.Message {
    // 即时折叠: edit_file/write_file 确认消息
    // ...
}

func (c *L1Compression) TruncateHeadTail(content string, budget int) string {
    // 掐头留尾: head 70% + tail 30%
    // ...
}
```

**验收标准**:
- [ ] 去重正确识别重复工具调用
- [ ] 确认消息折叠为单行
- [ ] 掐头留尾符合预算限制
- [ ] 保护清单内容不被压缩
- [ ] 单元测试覆盖各种场景

**预计工作量**: 3-4 天

---

### M4: 离线 Replay 评估 (Week 4-5)
**优先级**: 🟡 **中** - 验证参数，指导后续优化

#### M4.1 Replay 框架搭建
**目标**: 基于 metrics.jsonl 回放历史会话

**实施步骤**:
1. 创建 `pkg/agent/replay.go` 回放框架
2. 实现会话重建逻辑
3. 模拟不同参数组合的压缩行为

**代码结构**:
```go
// pkg/agent/replay.go
package agent

type ReplayConfig struct {
    SoftPressure   float64
    HardPressure   float64
    ToolBudgets    ToolBudgets
    MetricsPath    string
}

type ReplayResult struct {
    PeakPressure     float64
    AvgPressure      float64
    L1TriggerCount   int
    L2TriggerCount   int
    RereadRate       float64
    CompressionCost  int64
    SavedTokens      int64
}

func ReplaySession(config ReplayConfig, sessionID string) ReplayResult {
    // 回放单个会话，模拟压缩行为
    // ...
}
```

**验收标准**:
- [ ] 能正确回放历史会话
- [ ] 模拟压缩行为准确
- [ ] 输出关键指标 (峰值压力、触发次数、重读率)
- [ ] 支持参数扫描

**预计工作量**: 3-4 天

#### M4.2 参数扫描与优化
**目标**: 找到最优参数组合

**实施步骤**:
1. 定义参数扫描空间
2. 批量运行 replay
3. 分析结果，选择最优配置

**扫描参数**:
```yaml
参数空间:
  soft_pressure: [0.30, 0.35, 0.40]
  hard_pressure: [0.50, 0.55, 0.60, 0.65]
  read_file_age1: [6144, 8192, 10240]
  target_pressure: [0.20, 0.25, 0.30]
```

**验收标准**:
- [ ] 扫描至少 27 种参数组合
- [ ] 找到最优配置 (峰值 < P_hard，L2 < 5%)
- [ ] 生成参数优化报告
- [ ] 更新推荐配置

**预计工作量**: 2-3 天

---

### M5: L2 语义摘要 (Week 6-7)
**优先级**: 🟢 **低** - 依赖 M4 验证 L1 不足

#### M5.1 结构化摘要 Schema
**目标**: 定义 L2 摘要的结构化格式

**实施步骤**:
1. 设计摘要 JSON schema
2. 定义摘要提示词
3. 选择摘要模型 (小模型)

**Schema 设计**:
```json
{
  "type": "object",
  "properties": {
    "files_modified": {"type": "array", "items": {"type": "string"}},
    "key_decisions": {"type": "array", "items": {"type": "string"}},
    "unresolved_errors": {"type": "array", "items": {"type": "string"}},
    "user_requirements": {"type": "array", "items": {"type": "string"}},
    "next_steps": {"type": "array", "items": {"type": "string"}},
    "offload_refs": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["files_modified", "key_decisions", "next_steps"]
}
```

**验收标准**:
- [ ] Schema 定义完整
- [ ] 摘要提示词有效
- [ ] 摘要模型选择合理
- [ ] 单元测试验证摘要质量

**预计工作量**: 2 天

#### M5.2 L2 摘要实现
**目标**: 对 age ≥ 3 的轮次生成结构化摘要

**实施步骤**:
1. 实现摘要生成逻辑
2. 集成到压缩流程
3. 添加 offload 引用保留

**验收标准**:
- [ ] age ≥ 3 轮次正确摘要
- [ ] 摘要质量满足要求
- [ ] offload 引用正确保留
- [ ] 压缩成本 < 节省量的 20%

**预计工作量**: 3-4 天

---

### M6: L3 会话重建 (Week 7-8)
**优先级**: 🔵 **最低** - 兜底机制，希望很少触发

#### M6.1 Handoff 摘要生成
**目标**: 生成会话重建所需的结构化摘要

**实施步骤**:
1. 设计 handoff 摘要 schema
2. 实现摘要生成逻辑
3. 添加关键文件清单

**验收标准**:
- [ ] handoff 摘要包含关键信息
- [ ] 新会话能正确继承摘要
- [ ] 会话连续性保持

**预计工作量**: 2-3 天

#### M6.2 自动 vs 确认机制
**目标**: 决定 L3 触发策略

**实施步骤**:
1. 实现配置选项 (自动/确认)
2. 添加用户通知逻辑
3. 实现新会话创建

**验收标准**:
- [ ] 支持自动和确认两种模式
- [ ] 默认模式合理
- [ ] 用户知情权保障

**预计工作量**: 1-2 天

---

## 📅 时间线总览

| 周次 | 里程碑 | 主要交付物 |
|------|--------|-----------|
| W1-2 | M1 | Metrics 增强，Token 计量完善 |
| W2-3 | M2 | L0 源头防护 (git_diff, grep, offload) |
| W3-4 | M3 | L1 确定性压缩 + 双阈值 |
| W4-5 | M4 | 离线 replay 评估 + 参数优化 |
| W6-7 | M5 | L2 语义摘要 |
| W7-8 | M6 | L3 会话重建 |

---

## 🎯 成功指标

### 技术指标
- [ ] 长会话 (10+ 轮) 峰值压力 < P_hard (0.55)
- [ ] L2 触发频率 < 5% turns
- [ ] 重读率不显著高于基线
- [ ] 压缩成本 < 节省量的 20%

### 业务指标
- [ ] context overflow 失败率 < 1%
- [ ] 长会话平均 token 消耗降低 30%+
- [ ] 短会话性能无退化

### 可观测性
- [ ] 每次压缩记录 before/after 水位
- [ ] 丢弃内容清单可查询
- [ ] 重读率可监控告警

---

## 🚧 风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| 参数调优耗时 | M4 replay 框架优先，数据驱动决策 |
| 摘要质量不足 | 结构化 schema + 小模型评测 |
| 过度压缩重读 | read_file age-1 保护 + 重读率监控 |
| Cache 失效 | turn 边界压缩 + 静态工具子集 |
| 实施复杂度 | 渐进式实施，每里程碑独立验收 |

---

## 🔄 下一步行动

**立即开始** (本周):
1. M1.2: Metrics 增强 (args_hash, path, offloaded)
2. M2.1: git_diff --stat 默认化

**短期规划** (本月):
3. M2.2: grep 结果上限
4. M2.3: offload 落盘机制
5. M3.1: 双阈值迟滞机制

**中期规划** (下月):
6. M3.2-3: L1 确定性压缩
7. M4: 离线 replay 评估

---

*本计划将根据实施进展和 replay 评估结果动态调整*