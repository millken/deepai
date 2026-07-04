# claudeplugin 包

发现 Claude Code 插件包（含 `.claude-plugin/plugin.json` 清单的目录），暴露其中的 **skills** 与 **mcpServers** 供 chat 运行时加载。

本包**只负责发现 + 解析 + `${CLAUDE_PLUGIN_ROOT}` 展开**，不调用 skill/MCP loader、无副作用——chat 层是唯一聚合点。

## 发现路径

按优先级扫描（项目级覆盖全局同名插件）：

1. `~/.deepai/plugins/` —— 全局
2. `<workdir>/.deepai/plugins/` —— 项目级

每个根下的直接子目录是候选插件；含有效 `.claude-plugin/plugin.json` 才视为插件。无清单的目录静默跳过；清单损坏/缺 `name` → 计入 `problems`（供启动 report 展示，不静默）。

## 加载的组件（2a）

- **skills**：`Plugin.SkillRoot()` 返回**插件根目录**（注意：`skill.Registry.LoadAllReported` 自己会拼 `/skills`，所以这里不拼）。同名 skill last-write-wins（不加前缀，命名空间留待后续统一设计）。
- **mcpServers**：`Plugin.MCPServers()` 合并三种官方来源并展开 `${CLAUDE_PLUGIN_ROOT}`：
  - 默认 `<plugin>/.mcp.json`
  - 清单 inline `mcpServers`（object）
  - 清单 `mcpServers` 为字符串路径 → 读该文件
  - array 等非规范形状 → 计入 `problem`（不静默丢弃），`${VAR}` 环境展开交给 MCP loader。

## 不在 2a 范围

插件的 `agents/`、`commands/`、`hooks/` 不加载（见 [docs/spec/PLUGIN_INTEGRATION_SPEC.md](../../docs/spec/PLUGIN_INTEGRATION_SPEC.md) 的 2b/2c/2d）。`pkg/plugin`（旧 plugin.yaml 系统）格式不兼容，本包独立实现、不复用。

## 用法

```go
plugins, problems := claudeplugin.Discover(workDir)
var pluginRoots []string
pluginServers := map[string]mcp.ServerConfig{}
for _, p := range plugins {
    pluginRoots = append(pluginRoots, p.SkillRoot())
    if servers, problem := p.MCPServers(); problem == "" {
        for n, s := range servers { pluginServers[n] = s }
    }
}
// pluginRoots → skill.Registry.LoadAllReported(workDir, pluginRoots)
// pluginServers → mcp.LoadWithServers(ctx, registry, workDir, pluginServers)
```
