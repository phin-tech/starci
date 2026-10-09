package hatchet

import (
	"context"
	"os"
	"testing"
	"time"

	"git.phin.tech/sam/starci/internal/engine/enginetest"
	"git.phin.tech/sam/starci/internal/step"
)

// TestEngine puts Hatchet through what every engine has to get right. It
// needs a Hatchet to talk to, so it runs only when asked:
//
//	set -a; . dev/.env; set +a
//	STARCI_TEST_HATCHET=1 go test ./internal/engine/hatchet -run TestEngine
//
// This process is the worker, so the steps run on this machine.
func TestEngine(t *testing.T) {
	if os.Getenv("STARCI_TEST_HATCHET") == "" {
		t.Skip("set STARCI_TEST_HATCHET=1, and the HATCHET_CLIENT_* variables, to run against Hatchet")
	}
	eng, err := New(Options{
		Machine: step.Config{Worker: "enginetest", StateDir: t.TempDir()},
		Slots:   4,
		Labels:  map[string]string{"enginetest": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- eng.Work(ctx) }()

	// Hatchet has to know the worker before it will hand it anything.
	deadline := time.After(time.Minute)
	for known := false; !known; {
		select {
		case err := <-stopped:
			t.Fatalf("the worker stopped: %v", err)
		case <-deadline:
			t.Fatal("Hatchet never listed the worker")
		case <-time.After(time.Second):
			workers, _ := eng.Workers(ctx)
			for _, w := range workers {
				known = known || w.Name == "starci-enginetest"
			}
		}
	}
	enginetest.Run(t, eng)
}
