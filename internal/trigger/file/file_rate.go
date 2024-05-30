package file

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/form3tech-oss/f1/v2/internal/trace"
	"github.com/form3tech-oss/f1/v2/internal/trigger/api"
)

type runnableStages struct {
	scenario            string
	stages              []runnableStage
	stagesTotalDuration time.Duration
	maxDuration         time.Duration
	concurrency         int
	maxIterations       uint64
	maxFailures         uint64
	maxFailuresRate     int
	ignoreDropped       bool
}

type runnableStage struct {
	rate              api.RateFunction
	params            map[string]string
	stageDuration     time.Duration
	iterationDuration time.Duration
	usersConcurrency  int
}

func Rate() api.Builder {
	flags := pflag.NewFlagSet("file", pflag.ContinueOnError)

	return api.Builder{
		Name:        "file <filename>",
		Description: "triggers test iterations from a yaml config file",
		Flags:       flags,
		New: func(flags *pflag.FlagSet, tracer trace.Tracer) (*api.Trigger, error) {
			filename := flags.Arg(0)
			fileContent, err := readFile(filename)
			if err != nil {
				return nil, err
			}
			runnableStages, err := parseConfigFile(*fileContent, time.Now())
			if err != nil {
				return nil, err
			}

			return &api.Trigger{
				Trigger:     newStagesWorker(runnableStages.stages, tracer),
				DryRun:      newDryRun(runnableStages.stages),
				Description: fmt.Sprintf("%d different stages", len(runnableStages.stages)),
				Duration:    runnableStages.stagesTotalDuration,
				Options: api.Options{
					Scenario:        runnableStages.scenario,
					MaxDuration:     runnableStages.maxDuration,
					Concurrency:     runnableStages.concurrency,
					MaxIterations:   runnableStages.maxIterations,
					MaxFailures:     runnableStages.maxFailures,
					MaxFailuresRate: runnableStages.maxFailuresRate,
					IgnoreDropped:   runnableStages.ignoreDropped,
				},
			}, nil
		},
		IgnoreCommonFlags: true,
	}
}

func readFile(filename string) (*[]byte, error) {
	file, err := os.Open(filepath.Clean(filename))
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer func() {
		if err = file.Close(); err != nil {
			logrus.WithError(err).Error("unable to close the config file")
		}
	}()

	fileContent, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	return &fileContent, nil
}

func newDryRun(stagesToRun []runnableStage) api.RateFunction {
	var startTime time.Time
	started := false
	stageIdx := 0

	return func(time time.Time) int {
		if stageIdx >= (len(stagesToRun)) {
			return 0
		}

		if !started {
			startTime = time
			started = true
		}

		currentStage := stagesToRun[stageIdx]

		if startTime.Add(currentStage.stageDuration).Before(time) {
			startTime = startTime.Add(currentStage.stageDuration)
			stageIdx++
		}

		if currentStage.usersConcurrency > 0 {
			return 1
		}

		rate := currentStage.rate(time)
		return rate
	}
}
