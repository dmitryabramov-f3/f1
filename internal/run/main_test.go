package run_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/phayes/freeport"
	log "github.com/sirupsen/logrus"
	"go.uber.org/goleak"

	"github.com/form3tech-oss/f1/v2/internal/envsettings"
)

var fakePrometheus FakePrometheus

const (
	fakePrometheusNamespace = "test-namespace"
	fakePrometheusID        = "test-run-name"
)

func TestMain(m *testing.M) {
	var err error
	fakePrometheus.Port, err = freeport.GetFreePort()
	if err != nil {
		log.Fatal(err)
	}
	err = os.Setenv(envsettings.EnvPrometheusPushGateway, fmt.Sprintf("http://localhost:%d/", fakePrometheus.Port))
	if err != nil {
		log.Fatal(err)
	}
	err = os.Setenv(envsettings.EnvPrometheusNamespace, fakePrometheusNamespace)
	if err != nil {
		log.Fatal(err)
	}
	err = os.Setenv(envsettings.EnvPrometheusLabelID, fakePrometheusID)
	if err != nil {
		log.Fatal(err)
	}

	fakePrometheus.StartServer()

	result := m.Run()

	fakePrometheus.StopServer()

	if result == 0 {
		if err := goleak.Find(); err != nil {
			log.Errorf("goleak: Errors on successful test run: %v\n", err)
			result = 1
		}
	}

	os.Exit(result)
}
