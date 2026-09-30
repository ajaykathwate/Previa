package environments

import (
	"context"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/riverqueue/river"
)

type BuildWorker struct {
	river.WorkerDefaults[BuildArgs]
	Service *Service
	Broker  *Broker
}

func (w *BuildWorker) Timeout(*river.Job[BuildArgs]) time.Duration {
	return w.Service.buildTimeout + queue.CompletionGrace
}
func (w *BuildWorker) Work(ctx context.Context, job *river.Job[BuildArgs]) error {
	request := Request(job.Args)
	if err := request.validate(); err != nil {
		return river.JobCancel(err)
	}
	err := w.Service.Build(ctx, request)
	return finishJob(ctx, w.Broker, request, job, err)
}

type TeardownWorker struct {
	river.WorkerDefaults[TeardownArgs]
	Service *Service
	Broker  *Broker
}

func (w *TeardownWorker) Timeout(*river.Job[TeardownArgs]) time.Duration {
	return queue.TeardownTimeout + queue.CompletionGrace
}
func (w *TeardownWorker) Work(ctx context.Context, job *river.Job[TeardownArgs]) error {
	request := Request(job.Args)
	if err := request.validate(); err != nil {
		return river.JobCancel(err)
	}
	workCtx, cancel := context.WithTimeout(ctx, queue.TeardownTimeout)
	err := w.Service.Teardown(workCtx, request)
	cancel()
	return finishJob(ctx, w.Broker, request, job, err)
}

// RegisterWorkers is used when real runtime/publisher adapters are available.
// The production app does not register no-op build workers for missing adapters.
func RegisterWorkers(workers *river.Workers, service *Service, broker *Broker) {
	river.AddWorker(workers, &BuildWorker{Service: service, Broker: broker})
	river.AddWorker(workers, &TeardownWorker{Service: service, Broker: broker})
}
