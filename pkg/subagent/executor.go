package subagent

import "context"

type FuncExecutor func(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error)

func (f FuncExecutor) Execute(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error) {
	return f(ctx, task, emit)
}
