package local

import (
	"os"
	"testing"

	"git.phin.tech/sam/starci/internal/step"
)

// A watchdog over a bare step is this test binary run again.
func TestMain(m *testing.M) {
	step.Watchdog()
	os.Exit(m.Run())
}
