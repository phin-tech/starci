//go:build !linux && !darwin

package step

import (
	"fmt"
	"os/exec"
	"runtime"
)

// confine refuses: on this system, starci does not know how to stop a bare
// step and everything it starts together.
func confine(cmd *exec.Cmd, offline bool) (*guard, error) {
	return nil, fmt.Errorf("bare steps are not supported on %s", runtime.GOOS)
}

// Watchdog does nothing here: there are no bare steps to watch over.
func Watchdog() {}
