package step

import "os/exec"

// noNetwork is the sandbox profile a bare step without the network runs
// under: everything is allowed but the network. Unix sockets are not the
// network, and a build often talks to a local daemon over one.
const noNetwork = `(version 1)
(allow default)
(deny network*)
(allow network* (remote unix-socket))`

// confine makes cmd a bare step: in a process group of its own, so that it
// and everything it starts are stopped together, and kept off the network if
// it asked to be.
func confine(cmd *exec.Cmd, offline bool) (*guard, error) {
	if offline {
		// sandbox-exec is deprecated, and still the one way macOS offers an
		// ordinary user to keep a process and its children off the network.
		cmd.Args = append([]string{"/usr/bin/sandbox-exec", "-p", noNetwork, cmd.Path}, cmd.Args[1:]...)
		cmd.Path = "/usr/bin/sandbox-exec"
	}
	return group(cmd), nil
}
