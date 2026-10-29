package step

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// Bare steps need no Docker, so these run wherever git and a shell do.

func bareConfig(t *testing.T) Config {
	return Config{Machine: "here", StateDir: t.TempDir(), Bare: true}
}

func runBareStep(t *testing.T, cfg Config, r *repo, sha string, s plan.Step) plan.Result {
	t.Helper()
	if s.Inputs == nil {
		s.Inputs = []string{"."}
	}
	return Run(context.Background(), cfg, r.dir, sha, s, func(string) {})
}

func TestABareStepRunsInItsWorkspace(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"src/a.txt": "a", "other.txt": "o"})
	res := runBareStep(t, bareConfig(t), r, sha, plan.Step{Name: "s", Inputs: []string{"src"},
		Run: `test -f src/a.txt && test ! -e other.txt && test "$PWD" = "$STARCI_WORKSPACE" && ! ls ../git 2>/dev/null`})
	if res.Status != "passed" {
		t.Fatalf("status = %s: %s", res.Status, res.Detail)
	}
}

func TestABareStepIsGivenNothingOfTheMachinesEnvironment(t *testing.T) {
	t.Setenv("STARCI_TEST_SECRET", "hunter2")
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	res := runBareStep(t, bareConfig(t), r, sha, plan.Step{Name: "s", Cache: false, Env: map[string]string{"MINE": "yes"},
		Run: `test -z "$STARCI_TEST_SECRET" && test "$MINE" = yes && test "$HOME" != "` + os.Getenv("HOME") + `" && test -d "$HOME" && test -d "$TMPDIR"`})
	if res.Status != "passed" {
		t.Fatalf("status = %s: %s", res.Status, res.Detail)
	}
}

func TestABareStepHasTheMachinesPrograms(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	cfg := bareConfig(t)
	res := runBareStep(t, cfg, r, sha, plan.Step{Name: "shell", Run: "git --version"})
	if res.Status != "passed" {
		t.Fatalf("a program on the machine's PATH was not found: %s", res.Detail)
	}
	res = runBareStep(t, cfg, r, sha, plan.Step{Name: "list", Argv: []string{"git", "--version"}})
	if res.Status != "passed" || !strings.HasPrefix(res.Output, "git version") {
		t.Fatalf("argv: status = %s, output %q: %s", res.Status, res.Output, res.Detail)
	}
	res = runBareStep(t, cfg, r, sha, plan.Step{Name: "missing", Argv: []string{"no-such-program-anywhere"}})
	if res.Status != "failed" || !strings.Contains(res.Detail, "not on the step's PATH") {
		t.Fatalf("missing program: status = %s: %s", res.Status, res.Detail)
	}
}

func TestABarePassIsReusedUntilAnInputChanges(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "1"})
	cfg := bareConfig(t)
	s := plan.Step{Name: "s", Cache: true, Run: "cat a"}
	if res := runBareStep(t, cfg, r, sha, s); res.Status != "passed" || res.Cached {
		t.Fatalf("first: %+v", res)
	}
	if res := runBareStep(t, cfg, r, sha, s); !res.Cached {
		t.Fatalf("second run was not reused: %+v", res)
	}
	sha = r.commit(map[string]string{"a": "2"})
	if res := runBareStep(t, cfg, r, sha, s); res.Cached {
		t.Fatalf("a changed input was reused: %+v", res)
	}
}

func TestAMachineRunsBareStepsOnlyIfItSaysSo(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	cfg := bareConfig(t)
	cfg.Bare = false
	res := runBareStep(t, cfg, r, sha, plan.Step{Name: "s", Run: "true"})
	if res.Status != "failed" || !strings.Contains(res.Detail, "does not run bare steps") {
		t.Fatalf("status = %s: %s", res.Status, res.Detail)
	}
}

func TestAStepThatNeedsAnotherKindOfMachineSaysSo(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "darwin"
	}
	res := runBareStep(t, bareConfig(t), r, sha, plan.Step{Name: "s", Run: "true", On: map[string]string{"os": other}})
	if res.Status != "failed" || !strings.Contains(res.Detail, "needs os="+other) {
		t.Fatalf("status = %s: %s", res.Status, res.Detail)
	}
	res = runBareStep(t, bareConfig(t), r, sha, plan.Step{Name: "s", Run: "true", On: map[string]string{"os": runtime.GOOS, "gpu": "yes"}})
	if res.Status != "passed" {
		t.Fatalf("a matching os, and a label the machine does not judge: status = %s: %s", res.Status, res.Detail)
	}
}

func TestABareCacheIsKeptBetweenRuns(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	cfg := bareConfig(t)
	s := plan.Step{Name: "s", Cache: false, Caches: map[string]string{"c": "MY_CACHE"}, Run: `test -d "$MY_CACHE" && echo $$ >> "$MY_CACHE/runs"`}
	for i := 0; i < 2; i++ {
		if res := runBareStep(t, cfg, r, sha, s); res.Status != "passed" {
			t.Fatalf("run %d: %s", i, res.Detail)
		}
	}
	s.Run = `test "$(wc -l < "$MY_CACHE/runs")" -eq 2`
	if res := runBareStep(t, cfg, r, sha, s); res.Status != "passed" {
		t.Fatalf("the cache did not keep both runs: %s", res.Detail)
	}
}

func TestWhatABareStepLeavesRunningEndsWithIt(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	started := time.Now()
	res := runBareStep(t, bareConfig(t), r, sha, plan.Step{Name: "s", Cache: false, Run: "sleep 60 & echo left it"})
	if res.Status != "passed" {
		t.Fatalf("status = %s: %s", res.Status, res.Detail)
	}
	if time.Since(started) > 20*time.Second {
		t.Fatalf("the step waited for what it left running")
	}
}

func TestCancelStopsABareStep(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan plan.Result, 1)
	go func() {
		done <- Run(ctx, bareConfig(t), r.dir, sha, plan.Step{Name: "s", Inputs: []string{"."}, Run: "sleep 60 & sleep 60"}, func(string) {})
	}()
	time.Sleep(time.Second)
	cancel()
	select {
	case res := <-done:
		if res.Status != "failed" || res.Detail != "cancelled" {
			t.Fatalf("status = %s: %s", res.Status, res.Detail)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the step did not stop")
	}
}

func TestABareStepWithoutTheNetworkIsKeptOffIt(t *testing.T) {
	if testing.Short() {
		t.Skip("reaches the internet; skipped with -short")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("needs curl")
	}
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	cfg := bareConfig(t)
	reach := `curl -sS -m 10 -o /dev/null https://example.com`
	if res := runBareStep(t, cfg, r, sha, plan.Step{Name: "on", Cache: false, Run: reach}); res.Status != "passed" {
		t.Skipf("this machine cannot reach the internet anyway: %s", res.Detail)
	}
	res := runBareStep(t, cfg, r, sha, plan.Step{Name: "off", Cache: false, Offline: true, Run: reach})
	if res.Status != "failed" || strings.Contains(res.Detail, "cannot start") {
		t.Fatalf("a step without the network reached it, or could not start: %s: %s", res.Status, res.Detail)
	}
}

func TestABareImageRunsBareStepsInIt(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src/a.txt": "a", "other.txt": "o"})
	// The machine itself does not run bare steps: in a container it need not.
	cfg := Config{Machine: "here", StateDir: t.TempDir(), BareImage: testImage}
	s := plan.Step{Name: "s", Cache: false, Inputs: []string{"src"}, Caches: map[string]string{"c": "MY_CACHE"},
		Run: `test -f /etc/alpine-release && test -f src/a.txt && test ! -e other.txt && test "$PWD" = "$STARCI_WORKSPACE" && test -d "$MY_CACHE"`}
	if res := runBareStep(t, cfg, r, sha, s); res.Status != "passed" {
		t.Fatalf("status = %s: %s", res.Status, res.Detail)
	}
	s.Offline, s.Run = true, "ls /sys/class/net"
	if res := runBareStep(t, cfg, r, sha, s); res.Status != "passed" || res.Output != "lo\n" {
		t.Fatalf("without the network: status = %s, interfaces %q: %s", res.Status, res.Output, res.Detail)
	}
}
