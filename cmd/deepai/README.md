# cmd/deepai

`cmd/deepai` 是一个交互式命令行工具，提供完整的 AI 代理功能：

- 交互式提示符界面，支持多轮对话
- 集成工具系统（bash、文件操作、澄清、任务等）
- 支持技能系统（skill system）扩展
- 支持从 DEEPAI.md 文件加载配置
- 支持调试日志记录
- 支持脚本化 provider（用于演示）和真实 LLM provider

## 功能特性

### 1. 交互式对话
- 提供 `>` 提示符进行多轮对话
- 支持 `exit` 或 `quit` 命令退出
- 支持 Ctrl+D 结束会话

### 2. 工具系统
- **bash 工具**：执行 shell 命令
- **文件工具**：读取、写入、列出文件等操作
- **澄清工具**：向用户请求澄清信息
- **任务工具**：启动子代理执行复杂任务

### 3. 技能系统
- 自动加载项目中的技能定义
- 支持动态注册技能工具
- 技能描述会添加到系统提示中

### 4. 配置系统
- 支持从 `~/.deepai/DEEPAI.md` 和 `./deepai/DEEPAI.md` 加载配置
- 支持环境变量配置：
  - `DEEPAI_PROVIDER`：LLM 提供者名称
  - `DEEPAI_MODEL`：模型名称
  - `DEEPAI_DEBUG_FILE`：调试日志文件路径
  - `DEEPAI_DEBUG`：启用调试日志（默认输出到临时文件）

### 5. 调试功能
- 支持异步写入调试日志
- 记录工具调用、结果、文本块和错误
- 可配置日志文件路径

## 运行方式

### 使用脚本化 provider（演示模式）
```bash
# 不设置 DEEPAI_PROVIDER 时自动使用脚本化 provider
go run ./cmd/deepai
```

### 使用真实 LLM provider
```bash
# 设置环境变量
export DEEPAI_PROVIDER=openai
export OPENAI_API_KEY=your-api-key
export DEEPAI_MODEL=gpt-4

export DEEPAI_PROVIDER=openai-compat
export OPENAI_COMPAT_BASE_URL=https://open.bigmodel.cn/api/coding/paas/v4
export OPENAI_COMPAT_API_KEY=2844a818197d435a814cd0c51dcec1bc.xxxx
# 运行
go run ./cmd/deepai
```

### 启用调试日志
```bash
# 输出到指定文件
export DEEPAI_DEBUG_FILE=/tmp/deepai-debug.log
go run ./cmd/deepai

# 或使用默认临时文件
export DEEPAI_DEBUG=1
go run ./cmd/deepai
```

## 输出说明

运行后你会看到以下输出：

- `[tool call]`：工具调用信息，显示工具名称和 ID
- `[tool result]`：工具执行结果（超过 200 字符会截断显示）
- `[subagent]`：子代理任务事件
- `[error]`：错误信息
- `[usage]`：token 使用统计（输入、输出、总计）

## 交互示例

```
deepai interactive mode — type your prompt, Ctrl+D or 'exit' to quit

> 请帮我查看当前目录的文件
[tool call] bash(call-123)
[tool result] 文件列表...
> 退出
bye.
```

## 配置文件示例

### DEEPAI.md 文件示例
```markdown
# 项目配置
- 项目名称：deepai-demo
- 开发语言：Go
- 主要功能：AI 代理系统

# 使用指南
1. 使用 bash 工具执行命令
2. 使用文件工具操作文件
3. 使用任务工具委托复杂任务
```

### 环境变量配置
```bash
# .env 文件
DEEPAI_PROVIDER=openai
DEEPAI_MODEL=gpt-4
OPENAI_API_KEY=sk-your-key
OPENAI_BASE_URL=https://api.openai.com/v1
DEEPAI_DEBUG_FILE=/tmp/deepai.log
```

## 代码结构

### 主要组件
1. **沙箱环境**：提供安全的工具执行环境
2. **工具注册表**：管理所有可用工具
3. **技能注册表**：管理动态加载的技能
4. **代理执行器**：协调工具调用和对话流程
5. **用户交互接口**：处理用户输入和输出

### 关键函数
- `main()`：初始化所有组件并启动交互循环
- `cliUserInteraction`：实现用户交互接口
- `asyncWriter`：异步写入调试日志
- `scriptedProvider`：脚本化 LLM 提供者（用于演示）

## 扩展开发

### 添加新工具
1. 实现 `models.Tool` 接口
2. 在 `main()` 中注册到工具注册表

### 添加新技能
1. 在项目目录创建技能定义文件
2. 技能会自动加载并注册

### 替换 LLM 提供者
1. 实现 `llm.LLMProvider` 接口
2. 在 `main()` 中替换 `scriptedProvider`

## 相关文件

- [cmd/deepai/main.go](cmd/deepai/main.go) - 主程序
- [pkg/agent/react.go](pkg/agent/react.go) - 代理执行逻辑
- [pkg/tools/registry.go](pkg/tools/registry.go) - 工具注册表
- [pkg/skill/registry.go](pkg/skill/registry.go) - 技能注册表
- [pkg/sandbox/sandbox.go](pkg/sandbox/sandbox.go) - 沙箱环境
- [pkg/llm/provider.go](pkg/llm/provider.go) - LLM 提供者接口
