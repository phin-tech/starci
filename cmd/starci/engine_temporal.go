//go:build temporal

package main

import (
	"fmt"
	"log/slog"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
	tlog "go.temporal.io/sdk/log"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/engine/temporal"
)

// temporalEngine connects to Temporal as the SDK's own environment says
// (TEMPORAL_ADDRESS, TEMPORAL_NAMESPACE, TEMPORAL_API_KEY, TEMPORAL_TLS and
// the rest). Runs and steps go to the task queue STARCI_QUEUE, "starci" if it
// says nothing.
func temporalEngine() (engine.Engine, error) {
	opts, err := envconfig.LoadDefaultClientOptions()
	if err != nil {
		return nil, fmt.Errorf("reading the Temporal connection from the environment: %w", err)
	}
	// The SDK says a good deal at INFO; a person running starci wants only
	// what is wrong.
	opts.Logger = tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	c, err := client.Dial(opts)
	if err != nil {
		return nil, fmt.Errorf("connecting to Temporal: %w", err)
	}
	return &temporal.Engine{Client: c, Queue: os.Getenv("STARCI_QUEUE")}, nil
}
