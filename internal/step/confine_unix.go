//go:build unix

package step

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
)

// group puts cmd in a process group of its own, stopped as one when the
// step is cancelled or its command exits, and by a watchdog if starci itself
// is killed first.
func group(cmd *exec.Cmd) *guard {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	kill := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Cancel = kill
	var dog *watchdog
	return &guard{
		started: func(log func(string)) {
			var err error
			if dog, err = watch(cmd.Process.Pid); err != nil {
				log("warning: nothing will stop this step's processes if starci is killed: " + err.Error())
			}
		},
		// What the command left running is the step's, and ends with it.
		exited: func() {
			_ = kill()
			dog.done()
		},
	}
}

// watchCommand is the hidden command starci is run as to be a watchdog.
const watchCommand = "__starci_watch"

// watchdog is a process of its own that kills a step's process group if the
// starci that started the step dies without saying it is done with it.
//
// It holds the reading end of a pipe and starci the writing end. However
// starci ends, even by SIGKILL, the kernel closes its end, and the watchdog
// reads the end of the pipe: unless starci first wrote that the step had
// ended, it kills the group. It is in a session of its own, so that whatever
// stops starci's process group or terminal does not stop it too.
type watchdog struct {
	cmd *exec.Cmd
	w   *os.File
}

func watch(pgid int) (*watchdog, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cmd := exec.Command(self, watchCommand, strconv.Itoa(pgid))
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, err
	}
	return &watchdog{cmd: cmd, w: w}, nil
}

// done tells the watchdog the step has ended, and waits for it to go.
func (d *watchdog) done() {
	if d == nil {
		return
	}
	_, _ = d.w.Write([]byte{1})
	d.w.Close()
	_ = d.cmd.Wait()
}

// Watchdog makes this process a watchdog and never returns, if that is what
// it was started as; otherwise it does nothing. A program that runs bare
// steps calls it before anything else, since a watchdog is that program run
// again.
func Watchdog() {
	if len(os.Args) != 3 || os.Args[1] != watchCommand {
		return
	}
	pgid, err := strconv.Atoi(os.Args[2])
	if err != nil || pgid <= 1 {
		os.Exit(2)
	}
	// Only starci's end of the pipe decides when this ends.
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM)
	said := make([]byte, 1)
	if n, _ := os.Stdin.Read(said); n == 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	os.Exit(0)
}
