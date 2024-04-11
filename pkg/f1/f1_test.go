//nolint:paralleltest // incompatible with system signal testing
package f1_test

import (
	"syscall"
	"testing"
	"time"
)

func TestSignalHandling(t *testing.T) {
	tests := []struct {
		signal syscall.Signal
	}{
		{signal: syscall.SIGTERM},
		{signal: syscall.SIGINT},
	}
	for _, test := range tests {
		t.Run(test.signal.String(), func(t *testing.T) {
			given, when, then := newF1Stage(t)

			given.
				after_duration_signal_will_be_sent(500*time.Millisecond, test.signal).
				a_scenario_where_each_iteration_takes(50 * time.Millisecond)

			when.
				the_f1_scenario_is_executed_with_constant_rate_and_args(
					"--rate", "10/1s",
					"--max-duration", "60s",
				)

			then.
				expect_the_scenario_iterations_to_have_run_no_more_than(10).and().
				expect_no_error_sending_signals().and().
				expect_no_goroutines_to_run()
		})
	}
}