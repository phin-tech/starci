package step

import (
	"os"
	"os/exec"
	"syscall"
)

// confine makes cmd a bare step: in a process group of its own, so that it
// and everything it starts are stopped together, and kept off the network if
// it asked to be.
func confine(cmd *exec.Cmd, offline bool) (*guard, error) {
	if offline {
		// A network namespace of its own, with nothing in it, entered
		// through a user namespace so that no privilege is needed. Where the
		// kernel does not allow that, the command fails to start and says so.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
		}
	}
	return group(cmd), nil
}
