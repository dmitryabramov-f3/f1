//nolint:paralleltest // incompatible with individual leak checks
package raterun_test

import (
	"testing"
	"time"

	"github.com/form3tech-oss/f1/v2/internal/raterun"
)

func Test_FunctionIsExecutedAtSpecifiedRates(t *testing.T) {
	given, when, then := NewRatedRunnerStage(t)

	given.some_rates([]raterun.Schedule{
		// Start immediately, firing function at intervals of 80ms
		{StartDelay: time.Nanosecond, Frequency: time.Millisecond * 80},
		// after 1 second, fire function at intervals of 250ms
		{StartDelay: time.Second, Frequency: time.Millisecond * 250},
		// after another 1 second, fire function at intervals of 10ms
		{StartDelay: time.Second, Frequency: time.Millisecond * 10},
	}).
		and().
		a_rate_runner()

	when.runner_is_run().and().
		// wait for 1600ms, which would allow our function to have run 12 times with 80ms interval and 2 times with 250ms interval
		time_passes(time.Millisecond * 1600).and().
		runner_is_terminated()

	then.function_ran_times(14).and().
		a_go_leak_is_not_found()
}

func Test_FunctionIsExecutedAtSpecifiedRatesWhenRatesAreReset(t *testing.T) {
	given, when, then := NewRatedRunnerStage(t)

	given.some_rates([]raterun.Schedule{
		// Start immediately, firing function at intervals of 80ms
		{StartDelay: time.Nanosecond, Frequency: time.Millisecond * 80},
		// after 1 second, fire function at intervals of 250ms
		{StartDelay: time.Second, Frequency: time.Millisecond * 250},
	}).
		and().
		a_rate_runner()

	when.runner_is_run().and().
		// wait for 1600ms, which would allow our function to have run 12 times with 80ms interval and 2 times with 250ms interval
		time_passes(time.Millisecond * 1600).and().
		runner_is_reset().and().
		// allow 2 more runs of the function
		time_passes(time.Millisecond * 200).and().
		runner_is_terminated()

	then.function_ran_times(16).and().
		a_go_leak_is_not_found()
}

func Test_RunnerLeaksWhenNotTerminated(t *testing.T) {
	given, when, then := NewRatedRunnerStage(t)

	given.some_rates([]raterun.Schedule{
		{StartDelay: time.Nanosecond, Frequency: time.Millisecond * 80},
	}).
		and().
		a_rate_runner()

	when.runner_is_run().and().
		time_passes(time.Millisecond * 1600)

	then.a_go_leak_is_found().and().
		runner_is_terminated().and().
		a_go_leak_is_not_found()
}
