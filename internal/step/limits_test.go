package step

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// containers lists the containers, running or not, whose command mentions
// marker.
func containers(t *testing.T, marker string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-a", "--no-trunc", "--format", "{{.Names}} {{.Command}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, marker) {
			found = append(found, line)
		}
	}
	return strings.Join(found, "\n")
}

func TestAStepThatRunsOutOfTimeIsStopped(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	// A length of sleep nothing else asks for, to find the container by.
	// Long enough for a busy machine to have started the container, since
	// what it printed before being stopped is part of what is checked.
	s := plan.Step{Name: "slow", Timeout: 8, Cache: true, Run: "echo started; sleep 31337"}

	began := time.Now()
	got := run(t, cfg, r, sha, s)
	if took := time.Since(began); took > time.Minute {
		t.Errorf("it took %s to stop a step allowed 8s", took)
	}
	if got.Status != "failed" || !strings.HasPrefix(got.Detail, "it ran out of time: it had 8s") {
		t.Fatalf("%+v, want it failed for running out of time", got)
	}
	// What it printed before it was stopped is not lost.
	if got.Output != "started\n" || !strings.Contains(got.Detail, "started") {
		t.Errorf("output = %q, detail = %q; want what the step printed", got.Output, got.Detail)
	}
	if left := containers(t, "sleep 31337"); left != "" {
		t.Errorf("the container was left behind:\n%s", left)
	}
	// And a step that was stopped is not remembered as having passed.
	if again := run(t, cfg, r, sha, s); again.Cached {
		t.Error("a step that ran out of time was reused as a pass")
	}
}

func TestAStepThatRunsOutOfMemoryIsStoppedAndSaysSo(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	// tail holds everything it has read until it sees a line end, and
	// /dev/zero never has one. The comment is there to be found by.
	s := plan.Step{Name: "greedy", Memory: 32 << 20, Timeout: 60, Cache: false, Run: "echo started; tail /dev/zero # 31338"}
	got := run(t, cfg, r, sha, s)
	if got.Status != "failed" || !strings.HasPrefix(got.Detail, "it ran out of memory: it had 32m") {
		t.Fatalf("%+v, want it failed for running out of memory", got)
	}
	if got.Output != "started\n" {
		t.Errorf("output = %q, want what the step printed", got.Output)
	}
	if left := containers(t, "31338"); left != "" {
		t.Errorf("the container was left behind:\n%s", left)
	}

	// A step with a memory limit that passes, or fails on its own account,
	// is cleaned up as well and is not said to have run out.
	ok := run(t, cfg, r, sha, plan.Step{Name: "fine", Memory: 64 << 20, Cache: false, Run: "echo 31339"})
	bad := run(t, cfg, r, sha, plan.Step{Name: "broken", Memory: 64 << 20, Cache: false, Run: "echo 31339; exit 4"})
	if ok.Status != "passed" || bad.Status != "failed" || !strings.HasPrefix(bad.Detail, "exit code 4") {
		t.Errorf("fine = %+v\nbroken = %+v", ok, bad)
	}
	if left := containers(t, "31339"); left != "" {
		t.Errorf("containers were left behind:\n%s", left)
	}
}

func TestAStepIsHeldToItsLimits(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	read := "cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max 2>/dev/null"

	// What it asked for.
	got := run(t, Config{Worker: "one", StateDir: t.TempDir()}, r, sha, plan.Step{Name: "asks", CPUs: 1.5, Memory: 64 << 20, Cache: false, Run: read})
	if got.Status != "passed" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got.Output, "cpu.max") && got.Output == "" {
		t.Skip("this docker does not show a container its limits")
	}
	if want := "150000 100000\n67108864\n0\n"; got.Output != want {
		t.Errorf("a step that asked for 1.5 cpus and 64m saw %q, want %q", got.Output, want)
	}

	// What the machine gives a step that did not ask.
	machine := Config{Worker: "one", StateDir: t.TempDir(), Limits: Limits{CPUs: 2, Memory: 128 << 20}}
	got = run(t, machine, r, sha, plan.Step{Name: "silent", Cache: false, Run: read})
	if want := "200000 100000\n134217728\n0\n"; got.Output != want {
		t.Errorf("a step that asked for nothing saw %q, want the machine's %q", got.Output, want)
	}
	// Less than the machine allows is what it gets.
	got = run(t, machine, r, sha, plan.Step{Name: "modest", CPUs: 1, Cache: false, Run: read})
	if want := "100000 100000\n134217728\n0\n"; got.Output != want {
		t.Errorf("a step that asked for 1 cpu saw %q, want %q", got.Output, want)
	}
}

func TestAStepThatAsksForTooMuchIsNotStarted(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir(), Limits: Limits{Timeout: 10 * time.Minute, CPUs: 2, Memory: 1 << 30}}
	for want, s := range map[string]plan.Step{
		"it asks for a timeout of 1h, and a step on this machine may have at most 10m": {Timeout: 3600},
		"it asks for 4 cpus, and a step on this machine may have at most 2":            {CPUs: 4},
		"it asks for 8g of memory, and a step on this machine may have at most 1g":     {Memory: 8 << 30},
	} {
		s.Name, s.Run, s.Cache = "big", "echo ran anyway", false
		got := run(t, cfg, r, sha, s)
		if got.Status != "failed" || got.Detail != want || got.Output != "" {
			t.Errorf("%+v\nwant it failed, not run, saying %q", got, want)
		}
	}
	// On a machine that sets none, a step may ask for what it likes, and
	// the timeout is the one limit there always is.
	if _, err := (Limits{}).For(plan.Step{Timeout: 7200}); err == nil || !strings.Contains(err.Error(), "at most 30m") {
		t.Errorf("two hours on a machine that sets no timeout: %v, want it refused at the default", err)
	}
	if got, err := (Limits{}).For(plan.Step{CPUs: 64, Memory: 256 << 30}); err != nil || got.CPUs != 64 || got.Timeout != DefaultTimeout {
		t.Errorf("a large step on a machine with no limits: %+v, %v", got, err)
	}
}

func TestLimitsAreNotInAKey(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	s := plan.Step{Name: "same", Cache: true, Run: "echo ran"}
	run(t, cfg, r, sha, s)
	s.Timeout, s.CPUs, s.Memory = 60, 1, 64<<20
	if got := run(t, cfg, r, sha, s); !got.Cached {
		t.Errorf("under different limits = %+v, want the earlier pass", got)
	}
}
