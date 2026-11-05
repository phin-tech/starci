//go:build temporal

package temporal

import (
	"context"
	"fmt"
	"os"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/step"
)

// A watchdog over a bare step is this test binary run again.
//
// Run with STARCI_TEST_MACHINE set, it is instead a machine of the pool, for
// a test to kill (TestKilledMachine): it serves STARCI_TEST_MACHINE_QUEUE
// until it is killed.
func TestMain(m *testing.M) {
	step.Watchdog()
	if name := os.Getenv("STARCI_TEST_MACHINE"); name != "" {
		if err := serveForATest(name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func serveForATest(name string) error {
	opts, err := envconfig.LoadDefaultClientOptions()
	if err != nil {
		return err
	}
	c, err := client.Dial(opts)
	if err != nil {
		return err
	}
	eng := &Engine{Client: c, Queue: os.Getenv("STARCI_TEST_MACHINE_QUEUE")}
	return eng.Serve(context.Background(), engine.Machine{
		Config: step.Config{Machine: name, StateDir: os.Getenv("STARCI_TEST_MACHINE_STATE"), Bare: true},
		Slots:  4,
	})
}
