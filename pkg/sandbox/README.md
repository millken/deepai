# sandbox 包

`sandbox` 包用于在每个会话目录中隔离命令执行与文件访问，适合给 Agent、工具调用或代码执行场景提供一个受限的工作目录。

功能概览
- 为每个 `sessionID` 创建独立目录，所有文件操作都限制在该目录下。
- 支持在沙箱中执行 shell 命令，并返回标准输出、标准错误、退出码和耗时。
- 优先使用 Linux `Landlock`，其次尝试 `bubblewrap`，都不可用时退回直接执行。
- 追踪已启动进程，在超时或关闭时主动清理。

主要类型
- `Sandbox`：沙箱实例，负责命令执行、文件读写和生命周期管理。
- `Config`：运行配置，包含超时、实例数量和清理延迟。
- `Result`：命令执行结果，包含 `Stdout()`、`Stderr()`、`ExitCode()`、`Duration()` 和 `Error()`。
- `TimeoutError`：命令执行超时时返回的错误类型。

后端选择
- `landlock`：如果当前 Linux 内核支持 `Landlock`，且规则集探测成功，则优先使用。
- `bwrap`：若 `Landlock` 不可用，但系统存在 `/usr/bin/bwrap` 且探测成功，则使用 `bubblewrap`。
- `direct`：两者都不可用时，回退到普通 shell 执行。

说明
- `Landlock` 分支通过 `no_new_privs` 和文件系统规则限制访问范围。
- `bubblewrap` 分支会将会话目录绑定为 `/workspace`，并只读绑定 `/usr`、`/bin`、`/lib`、`/lib64`、`/etc` 等运行时依赖路径。
- 回退到 `direct` 后端时，文件路径仍会受 `Sandbox` 的路径解析保护，但进程级隔离能力会减弱。

快速示例

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/millken/deepai/pkg/sandbox"
)

func main() {
    sb, err := sandbox.New("session-1", "/tmp/deepai-sandbox")
    if err != nil {
        panic(err)
    }
    defer sb.Close()

    if err := sb.WriteFile("input.txt", []byte("hello sandbox")); err != nil {
        panic(err)
    }

    result, err := sb.Exec(context.Background(), "cat input.txt", 5*time.Second)
    if err != nil {
        panic(err)
    }

    fmt.Println("stdout:", result.Stdout())
    fmt.Println("exit:", result.ExitCode())
}
```

API 概览
- `New(sessionID, baseDir)`：创建一个使用默认配置的沙箱。
- `NewWithConfig(sessionID, baseDir, cfg)`：创建并应用自定义配置。
- `(*Sandbox).Exec(ctx, cmd, timeout)`：在当前后端中执行 shell 命令。
- `(*Sandbox).WriteFile(path, data)`：向会话目录写入文件。
- `(*Sandbox).ReadFile(path)`：从会话目录读取文件。
- `(*Sandbox).GetDir()`：返回当前会话目录绝对路径。
- `(*Sandbox).Close()`：结束已追踪进程并删除会话目录。

路径约束
- `WriteFile` 和 `ReadFile` 都会将路径解析到当前会话目录下。
- 绝对路径会被转换为相对于会话目录的路径。
- 若路径尝试逃逸到会话目录之外，会返回 `path escapes sandbox` 错误。

超时与清理
- 未显式指定超时时，默认超时为 5 分钟。
- 命令超时后会返回 `*TimeoutError`，并主动终止对应进程组。
- `Close()` 会终止仍在运行的进程，并在短暂延迟后删除会话目录。

测试覆盖
- [pkg/sandbox/sandbox_test.go](pkg/sandbox/sandbox_test.go#L1) 覆盖了目录创建、命令执行、文件读写和超时行为。

适用场景
- 执行受限 shell 命令。
- 为 Agent 任务创建独立工作目录。
- 将临时文件、生成代码或命令结果与主工作区隔离。

限制
- `Landlock` 依赖 Linux 内核能力；在不支持的平台或内核版本下会自动降级。
- `bubblewrap` 依赖系统安装 `/usr/bin/bwrap`。
- 当前 `Config.MaxInstances` 字段尚未在实现中生效，更多像是预留配置。
