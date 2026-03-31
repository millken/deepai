# subagent 包

`subagent` 包用于把长任务拆成独立的子代理任务，并通过池化执行、事件回传和超时控制来管理它们的生命周期。它适合用于需要后台异步处理的场景，例如规划、代码分析、文件处理或受限的工具链执行。

功能概览
- 通过 `Pool` 统一创建、调度、等待和查询子代理任务。
- 通过 `Executor` 将实际执行逻辑与任务调度解耦。
- 通过 `EventSink` 将任务状态和中间过程事件回传给调用方。
- 提供 `general-purpose` 和 `bash` 两种预置子代理类型。
- 支持并发限制、任务超时和默认配置回退。

主要类型
- `Pool`：任务池，负责启动任务、等待任务完成和读取任务状态。
- `Task`：任务实体，包含 `ID`、`RequestID`、`Status`、`Prompt`、`Result`、`Messages` 等信息。
- `SubagentConfig`：单个任务的配置，例如 `Type`、`MaxTurns`、`Timeout`、`SystemPrompt` 和 `Tools`。
- `PoolConfig`：池级配置，例如最大并发数、默认超时、日志器和默认任务模板。
- `Executor`：执行接口，负责真正跑任务并产出结果。
- `ExecutionResult`：执行返回值，包含最终 `Result` 和 `Messages`。
- `TaskEvent`：任务事件，用于通知开始、运行中、完成、失败或超时。
- `EventSink`：事件回调函数类型。

任务生命周期
1. 调用 `Pool.StartTask(...)` 创建任务并进入 `pending`。
2. 任务获得并发许可后切换到 `running`。
3. `Executor.Execute(...)` 产出最终结果和过程消息。
4. 任务结束后切换到 `completed`、`failed` 或 `timed_out`。
5. 调用 `Pool.Wait(...)` 可以等待任务完成并拿到快照。

预置类型
- `SubagentGeneralPurpose`：通用型任务，默认 `MaxTurns=6`，工具默认为 `file_ops`。
- `SubagentBash`：面向 shell/bash 风格任务，默认 `MaxTurns=4`，工具默认为 `bash`。

事件说明
- `task_started`：任务已创建并进入队列。
- `task_running`：任务开始执行。
- `task_completed`：任务成功完成。
- `task_failed`：任务失败。
- `task_timed_out`：任务超时。

事件可通过 `WithEventSink(ctx, sink)` 绑定到上下文中，执行过程中的事件会自动回调。

快速示例

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/millken/deepai/pkg/models"
    "github.com/millken/deepai/pkg/subagent"
)

func main() {
    pool := subagent.NewPool(subagent.FuncExecutor(func(ctx context.Context, task *subagent.Task, emit func(subagent.TaskEvent)) (subagent.ExecutionResult, error) {
        emit(subagent.TaskEvent{Type: "task_running", Message: "working"})
        return subagent.ExecutionResult{
            Result: "done",
            Messages: []models.Message{{Role: models.RoleAI, Content: "done"}},
        }, nil
    }), subagent.PoolConfig{Timeout: 2 * time.Minute})

    ctx := subagent.WithEventSink(context.Background(), func(evt subagent.TaskEvent) {
        fmt.Printf("event: %s %s\n", evt.Type, evt.Message)
    })

    task, err := pool.StartTask(ctx, "demo task", "do work", subagent.SubagentConfig{Type: subagent.SubagentGeneralPurpose})
    if err != nil {
        panic(err)
    }

    completed, err := pool.Wait(context.Background(), task.ID)
    if err != nil {
        panic(err)
    }

    fmt.Println("status:", completed.Status)
    fmt.Println("result:", completed.Result)
}
```

核心 API
- `NewPool(executor, cfg)`：创建任务池。
- `(*Pool).StartTask(ctx, description, prompt, cfg)`：启动一个任务并返回任务快照。
- `(*Pool).Wait(ctx, taskID)`：等待任务完成。
- `(*Pool).GetTask(taskID)`：获取当前任务快照。
- `WithEventSink(ctx, sink)`：将事件回调绑定到上下文。
- `EmitEvent(ctx, evt)`：向上下文中的 sink 发送事件。
- `FuncExecutor`：把普通函数适配成 `Executor`。

任务字段说明
- `Task.CreatedAt()`：返回创建时间，格式为 RFC3339Nano。
- `Task.CompletedAt()`：返回完成时间，格式为 RFC3339Nano。

调度策略
- `PoolConfig.MaxConcurrent` 控制同时运行的任务数，默认值为 1。
- `PoolConfig.Timeout` 作为任务默认超时，默认值为 2 分钟。
- `PoolConfig.Logger` 用于输出任务状态日志，默认使用标准日志器。
- `PoolConfig.Defaults` 用于按子代理类型覆盖默认配置。

测试覆盖
- [pkg/subagent/pool_test.go](pkg/subagent/pool_test.go#L1) 覆盖了任务完成、超时和未知任务等待等场景。

适用场景
- 后台异步子任务管理。
- 将复杂工作流拆成多个受控执行单元。
- 将执行过程中的中间事件逐步推送给上层调用方。

注意事项
- `StartTask` 传入的 `prompt` 不能为空。
- 若 `cfg.Type` 没有对应默认配置，会回退到 `general-purpose`。
- `Wait` 只等待任务结束，不负责重新执行或恢复失败任务。
