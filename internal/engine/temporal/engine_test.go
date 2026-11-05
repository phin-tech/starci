//go:build temporal

package temporal

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/engine/enginetest"
	"git.phin.tech/sam/starci/internal/step"
)

// TestEngine puts the engine through the shared tests against a real Temporal
// server, with a machine in this process taking the steps. It runs when
// STARCI_TEST_TEMPORAL_SERVER=1, connecting as the SDK's environment says
// (TEMPORAL_ADDRESS and the rest), on a task queue of its own so that no other
// machine takes its steps.
func TestEngine(t *testing.T) {
	if os.Getenv("STARCI_TEST_TEMPORAL_SERVER") == "" {
		t.Skip("set STARCI_TEST_TEMPORAL_SERVER=1 and TEMPORAL_ADDRESS to run against a Temporal server")
	}
	opts, err := envconfig.LoadDefaultClientOptions()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Dial(opts)
	if err != nil {
		t.Fatalf("connecting to Temporal: %v", err)
	}
	defer c.Close()

	eng := &Engine{Client: c, Queue: "starci-test-" + uuid.NewString()}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- eng.Serve(ctx, engine.Machine{
			Config: step.Config{Machine: "test-" + uuid.NewString()[:8], StateDir: t.TempDir(), Bare: true},
			Slots:  4,
		})
	}()
	defer func() {
		stop()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(time.Minute):
			t.Error("the machine did not stop")
		}
	}()

	enginetest.Run(t, eng)
}
