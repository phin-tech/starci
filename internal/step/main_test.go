package step

import (
	"os"
	"testing"
)

// A watchdog over a bare step is this test binary run again.
func TestMain(m *testing.M) {
	Watchdog()
	os.Exit(m.Run())
}
