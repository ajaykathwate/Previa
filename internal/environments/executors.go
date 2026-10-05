package environments

import "context"

// Executors routes each operation kind to the adapter that performs it. A
// kind without an adapter is never treated as done: its operation fails
// retryably with executor_unavailable and stays visible until one exists.
type Executors map[OperationKind]OperationExecutor

func (e Executors) Execute(ctx context.Context, op Operation) error {
	if executor := e[op.Kind]; executor != nil {
		return executor.Execute(ctx, op)
	}
	return &OperationFailure{Code: "executor_unavailable"}
}
