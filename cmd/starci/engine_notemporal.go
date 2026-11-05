//go:build !temporal

package main

import (
	"errors"

	"git.phin.tech/sam/starci/internal/engine"
)

// temporalEngine is not in this starci: the Temporal engine is compiled in
// only when asked for (docs/adr/0004-an-engine-sdk-behind-a-build-tag.md).
func temporalEngine() (engine.Engine, error) {
	return nil, errors.New("this starci was built without the Temporal engine; build it with -tags temporal")
}
