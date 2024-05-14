package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/sirupsen/logrus"

	"github.com/form3tech-oss/f1/v2/internal/console"
	"github.com/form3tech-oss/f1/v2/internal/envsettings"
	"github.com/form3tech-oss/f1/v2/internal/logging"
	"github.com/form3tech-oss/f1/v2/internal/metrics"
	"github.com/form3tech-oss/f1/v2/internal/options"
	"github.com/form3tech-oss/f1/v2/internal/raterun"
	"github.com/form3tech-oss/f1/v2/internal/run/templates"
	"github.com/form3tech-oss/f1/v2/internal/trace"
	"github.com/form3tech-oss/f1/v2/internal/trigger/api"
	"github.com/form3tech-oss/f1/v2/internal/xcontext"
	"github.com/form3tech-oss/f1/v2/pkg/f1/scenarios"
)

const (
	NextIterationWindow = 10 * time.Millisecond

	metricsRefreshInterval = 5 * time.Second
)

func NewRun(
	options options.RunOptions,
	t *api.Trigger,
	settings envsettings.Settings,
	metricsInstane *metrics.Metrics,
	tracer trace.Tracer,
	printer *console.Printer,
) (*Run, error) {
	run := Run{
		Options:         options,
		Settings:        settings,
		RateDescription: t.Description,
		trigger:         t,
		metrics:         metricsInstane,
		tracer:          tracer,
		printer:         printer,
	}

	run.templates = templates.Parse(templates.RenderTermColors)
	run.result = NewResult(options, run.templates)

	if run.Settings.Prometheus.PushGateway != "" {
		run.pusher = push.New(settings.Prometheus.PushGateway, "f1-"+options.Scenario).
			Gatherer(run.metrics.Registry)

		if run.Settings.Prometheus.Namespace != "" {
			run.pusher = run.pusher.Grouping("namespace", run.Settings.Prometheus.Namespace)
		}

		if run.Settings.Prometheus.LabelID != "" {
			run.pusher = run.pusher.Grouping("id", run.Settings.Prometheus.LabelID)
		}
	}
	if run.Options.RegisterLogHookFunc == nil {
		run.Options.RegisterLogHookFunc = logging.NoneRegisterLogHookFunc
	}

	progressRunner, _ := raterun.New(func(rate time.Duration, _ time.Time) {
		run.gatherProgressMetrics(rate)
		run.printer.Println(run.result.Progress())
	}, []raterun.Rate{
		{Start: time.Nanosecond, Rate: time.Second},
		{Start: time.Minute, Rate: time.Second * 10},
		{Start: time.Minute * 5, Rate: time.Minute / 2},
		{Start: time.Minute * 10, Rate: time.Minute},
	})
	run.progressRunner = progressRunner

	return &run, nil
}

type Run struct {
	tracer          trace.Tracer
	progressRunner  raterun.Runner
	metrics         *metrics.Metrics
	templates       *templates.Templates
	activeScenario  *ActiveScenario
	trigger         *api.Trigger
	pusher          *push.Pusher
	printer         *console.Printer
	Settings        envsettings.Settings
	RateDescription string
	result          Result
	Options         options.RunOptions
	iteration       atomic.Uint64
	failures        atomic.Uint64
	notifyDropped   sync.Once
	busyWorkers     atomic.Int32
}

func (r *Run) Do(ctx context.Context, s *scenarios.Scenarios) (*Result, error) {
	r.printer.Print(r.templates.Start(templates.StartData{
		Scenario:        r.Options.Scenario,
		MaxDuration:     r.Options.MaxDuration,
		MaxIterations:   r.Options.MaxIterations,
		RateDescription: r.RateDescription,
	}))

	defer r.printSummary()
	defer r.printLogOnFailure()

	if err := r.configureLogging(); err != nil {
		return nil, fmt.Errorf("configure logging: %w", err)
	}

	r.metrics.Reset()

	scenario := s.GetScenario(r.Options.Scenario)
	if scenario == nil {
		return nil, fmt.Errorf("scenario not defined: %s", r.Options.Scenario)
	}
	r.activeScenario = NewActiveScenario(scenario, r.metrics)
	r.pushMetrics(ctx)

	// run teardown even if the context is cancelled
	teardownContext := xcontext.Detach(ctx)
	defer r.teardownActiveScenario(teardownContext)

	if r.activeScenario.t.Failed() {
		return r.reportSetupFailure(ctx), nil
	}

	// set initial started timestamp so that the progress trackers work
	r.result.RecordStarted()
	r.progressRunner.Run()

	metricsCloseCh := make(chan struct{})
	go func() {
		t := time.NewTicker(metricsRefreshInterval)
		defer t.Stop()

		for {
			select {
			case <-t.C:
				r.pushMetrics(ctx)
			case <-ctx.Done():
				return
			case <-metricsCloseCh:
				return
			}
		}
	}()

	r.run(ctx)

	r.progressRunner.Terminate()
	close(metricsCloseCh)
	r.gatherMetrics()

	return &r.result, nil
}

func (r *Run) reportSetupFailure(ctx context.Context) *Result {
	r.fail("setup failed")
	r.pushMetrics(ctx)
	r.printer.Println(r.result.Setup())
	return &r.result
}

func (r *Run) teardownActiveScenario(ctx context.Context) {
	r.activeScenario.Teardown()
	if r.activeScenario.t.TeardownFailed() {
		r.fail("teardown failed")
	}
	r.pushMetrics(ctx)
	r.printer.Println(r.result.Teardown())
}

func (r *Run) configureLogging() error {
	err := r.Options.RegisterLogHookFunc(r.Options.Scenario)
	if err != nil {
		return fmt.Errorf("calling register log hook func: %w", err)
	}

	if !r.Options.Verbose {
		r.result.LogFile = redirectLoggingToFile(r.Options.Scenario, r.Settings.LogFilePath, r.printer.Writer)
		welcomeMessage := r.templates.Start(templates.StartData{
			Scenario:        r.Options.Scenario,
			MaxDuration:     r.Options.MaxDuration,
			MaxIterations:   r.Options.MaxIterations,
			RateDescription: r.RateDescription,
		})

		logrus.Info(welcomeMessage)
		r.printer.Printf("Saving logs to %s\n\n", r.result.LogFile)
	}

	return nil
}

func (r *Run) printSummary() {
	summary := r.result.String()
	r.printer.Println(summary)
	if !r.Options.Verbose {
		logrus.Info(summary)
		logrus.StandardLogger().SetOutput(r.printer.Writer)
		stdlog.SetOutput(r.printer.Writer)
	}
}

func (r *Run) run(ctx context.Context) {
	// Build a worker group to process the iterations.
	workers := r.Options.Concurrency
	doWorkChannel := make(chan uint64, workers)
	stopWorkers := make(chan struct{})

	r.busyWorkers.Store(0)
	workDone := make(chan bool, workers)

	iterationStatePool := make([]*iterationState, workers)
	for i := range workers {
		iterationStatePool[i] = newIterationState(r.Options.Scenario)
	}

	wg := &sync.WaitGroup{}
	defer wg.Wait()
	wg.Add(workers)
	for i := range workers {
		go r.runWorker(doWorkChannel, stopWorkers, wg, i, workDone, iterationStatePool[i])
	}

	// if the trigger has a limited duration, restrict the run to that duration.
	duration := r.Options.MaxDuration
	if r.trigger.Duration > 0 && r.trigger.Duration < r.Options.MaxDuration {
		duration = r.trigger.Duration
	}

	// Cancel work slightly before end of duration to avoid starting a new iteration
	durationElapsed := NewCancellableTimer(duration-NextIterationWindow, r.tracer)
	r.result.RecordStarted()
	defer r.result.RecordTestFinished()

	workTriggered := make(chan bool, workers)
	stopTrigger := make(chan bool, 1)
	go r.trigger.Trigger(workTriggered, stopTrigger, workDone, r.Options)

	// This blocks waiting for cancellable timer
	go func() {
		elapsed := <-durationElapsed.C
		r.tracer.ReceivedFromChannel("C")
		if elapsed {
			r.printer.Println(r.result.MaxDurationElapsed())
		}
		logrus.Info("Stopping worker")
		stopTrigger <- true
		close(stopWorkers)
		wg.Wait()
	}()

	// run more iterations on every tick, until duration has elapsed.
	for {
		r.tracer.Event("Run loop ")
		select {
		case <-ctx.Done():
			r.printer.Println(r.result.Interrupted())
			r.progressRunner.RestartRate()
			// stop listening to interrupts - second interrupt will terminate immediately
			durationElapsed.Cancel()
		case <-workTriggered:
			r.tracer.ReceivedFromChannel("workTriggered")
			r.doWork(doWorkChannel, durationElapsed)
			r.tracer.Event("Called do work")
		case <-stopWorkers:
			wg.Wait()
			return
		}
	}
}

func (r *Run) doWork(doWorkChannel chan<- uint64, durationElapsed *CancellableTimer) {
	if r.busyWorkers.Load() >= int32(r.Options.Concurrency) {
		r.activeScenario.RecordDroppedIteration()
		r.notifyDropped.Do(func() {
			// only log once.
			logrus.Warn("Dropping requests as workers are too busy. Considering increasing `--concurrency` argument")
		})
		return
	}
	iteration := r.iteration.Add(1)
	if r.Options.MaxIterations > 0 && iteration > r.Options.MaxIterations {
		r.tracer.IterationEvent("Max iterations exceeded Calling Cancel", iteration)
		durationElapsed.Cancel()
		r.printer.Println(r.result.MaxIterationsReached())
		r.tracer.IterationEvent("Max iterations exceeded Called Cancel", iteration)
	} else if r.Options.MaxIterations <= 0 || iteration <= r.Options.MaxIterations {
		r.tracer.IterationEvent("Within Max iterations So calling dowork()", iteration)
		doWorkChannel <- iteration
	}
}

func (r *Run) gatherMetrics() {
	m, err := r.metrics.Registry.Gather()
	if err != nil {
		r.result.AddError(fmt.Errorf("gather metrics: %w", err))
	}
	for _, metric := range m {
		if metric.GetName() == metrics.IterationMetricName {
			for _, m := range metric.GetMetric() {
				result := metrics.UnknownResult
				stage := metrics.IterationStage
				for _, label := range m.GetLabel() {
					if label.GetName() == metrics.ResultLabel {
						result = metrics.ResultTypeFromString(label.GetValue())
					}
					if label.GetName() == metrics.StageLabel {
						stage = label.GetValue()
					}
				}

				if stage == metrics.IterationStage {
					r.result.SetMetrics(result, m.GetSummary().GetSampleCount(), m.GetSummary().GetQuantile())
				}
			}
		}
	}
}

func (r *Run) gatherProgressMetrics(duration time.Duration) {
	m, err := r.metrics.ProgressRegistry.Gather()
	if err != nil {
		r.result.AddError(fmt.Errorf("gather metrics: %w", err))
	}
	r.metrics.Progress.Reset()
	r.result.ClearProgressMetrics()
	for _, metric := range m {
		for _, m := range metric.GetMetric() {
			result := metrics.UnknownResult
			for _, label := range m.GetLabel() {
				if label.GetName() == metrics.ResultLabel {
					result = metrics.ResultTypeFromString(label.GetValue())
				}
			}

			r.result.IncrementMetrics(
				duration, result, m.GetSummary().GetSampleCount(), m.GetSummary().GetQuantile(),
			)
		}
	}
}

func (r *Run) runWorker(
	iterationInput <-chan uint64,
	stop <-chan struct{},
	wg *sync.WaitGroup,
	worker int,
	workDone chan<- bool,
	iterationState *iterationState,
) {
	defer wg.Done()
	r.tracer.WorkerEvent("Started worker", worker)
	for {
		select {
		case <-stop:
			r.tracer.WorkerEvent("Stopping worker", worker)
			return
		case iteration := <-iterationInput:
			r.tracer.IterationEvent("Received work from Channel 'doWork'", iteration)
			r.busyWorkers.Add(1)

			iterationState.t.Reset(strconv.FormatUint(iteration, 10))
			successful := r.activeScenario.Run(iterationState)
			if !successful {
				r.failures.Add(1)
			}
			r.busyWorkers.Add(-1)

			// if we need to stop - no one is listening for workDone,
			// so it will block forever. check the stop channel to stop the worker
			select {
			case workDone <- true:
			case <-stop:
				return
			}

			r.tracer.IterationEvent("Completed iteration", iteration)
		}
	}
}

func (r *Run) fail(message string) {
	r.result.AddError(errors.New(message))
}

func (r *Run) pushMetrics(ctx context.Context) {
	if r.pusher == nil {
		return
	}
	err := r.pusher.PushContext(ctx)
	if err != nil {
		logrus.Errorf("unable to push metrics to prometheus: %v", err)
	}
}

func (r *Run) printLogOnFailure() {
	if r.Options.Verbose || !r.Options.VerboseFail || !r.result.Failed() {
		return
	}

	if err := r.printResultLogs(); err != nil {
		logrus.WithError(err).Error("error printing logs")
	}
}

func (r *Run) printResultLogs() error {
	fd, err := os.Open(r.result.LogFile)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer func() {
		if fd == nil {
			return
		}
		if err := fd.Close(); err != nil {
			logrus.WithError(err).Error("error closing log file")
		}
	}()

	if fd != nil {
		if _, err := io.Copy(r.printer.Writer, fd); err != nil {
			return fmt.Errorf("printing logs: %w", err)
		}
	}

	return nil
}
