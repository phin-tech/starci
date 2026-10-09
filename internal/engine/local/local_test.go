package local

import (
	"testing"

	"git.phin.tech/sam/starci/internal/engine/enginetest"
	"git.phin.tech/sam/starci/internal/step"
)

func TestEngine(t *testing.T) {
	enginetest.Run(t, &Engine{Config: step.Config{Worker: "test", StateDir: t.TempDir()}, Slots: 4})
}
