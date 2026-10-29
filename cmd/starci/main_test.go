package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// When starci is killed outright partway through a bare step, nothing of the
// step is left running: its watchdog sees starci go, and stops the step's
// processes.
func TestWhatABareStepStartedEndsWhenStarciIsKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("builds starci and runs it; skipped with -short")
	}
	dir := t.TempDir()
	starci := filepath.Join(dir, "starci")
	if out, err := exec.Command("go", "build", "-o", starci, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	repo := filepath.Join(dir, "repo")
	pidFile := filepath.Join(dir, "pid")
	script := `def ci(ctx):
    return [step("linger", run = "sleep 300 & echo $! > ` + pidFile + `; wait")]
`
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".ci.star"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run := exec.Command(starci, "run", repo, "--bare")
	run.Env = append(os.Environ(), "STARCI_STATE="+filepath.Join(dir, "state"))
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	defer run.Process.Kill()

	var pid int
	for deadline := time.Now().Add(30 * time.Second); pid == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the step never started its sleep")
		}
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the step's sleep is not running: %v", err)
	}

	if err := run.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = run.Wait()
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the step's sleep was still running 5s after starci was killed")
		}
	}
}
