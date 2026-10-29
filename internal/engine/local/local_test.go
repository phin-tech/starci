package local

import (
	"testing"

	"git.phin.tech/sam/starci/internal/engine/enginetest"
	"git.phin.tech/sam/starci/internal/step"
)

func TestEngine(t *testing.T) {
	enginetest.Run(t, &Engine{Config: step.Config{Machine: "test", StateDir: t.TempDir(), Bare: true}, Slots: 4})
}
