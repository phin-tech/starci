package step

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// counting is a command that fails until it has been run n times, counting
// in a directory kept between runs, and says which try it is on.
func counting(n int) string {
	return fmt.Sprintf(`echo x >> /seen/tries; tries=$(wc -l < /seen/tries); echo "try $tries"; test "$tries" -ge %d`, n)
}

// seen is a cache of its own for one test, which is where counting counts.
func seen(t *testing.T) map[string]string {
	name := fmt.Sprintf("retry-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", "starci-cache-"+name).Run() })
	return map[string]string{name: "/seen"}
}

func TestAStepIsTriedAgainWhenItAsksToBe(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}

	// Passes on its third try, and is allowed two more than the first.
	var logged []string
	s := plan.Step{Name: "flaky", Image: testImage, Inputs: []string{"."}, Caches: seen(t), Retries: 2, Cache: true, Run: counting(3)}
	got := Run(context.Background(), cfg, r.dir, sha, s, func(line string) { logged = append(logged, line) })
	if got.Status != "passed" || got.Attempts != 3 || !got.Flaky || got.Output != "try 3\n" {
		t.Fatalf("%+v, want a pass on the third try, marked flaky", got)
	}
	if all := strings.Join(logged, "\n"); !strings.Contains(all, "the step asks to be tried 2 more time(s), this is number 1") {
		t.Errorf("the log does not say it was tried again:\n%s", all)
	}
	// What is remembered is that it passed. Reused, it is not flaky again:
	// that was said of the run it happened in.
	if again := run(t, cfg, r, sha, s); !again.Cached || again.Flaky || again.Attempts != 0 {
		t.Errorf("the reused result = %+v, want a pass that is not called flaky a second time", again)
	}

	// Allowed one more try and needing two: it has failed.
	got = run(t, cfg, r, sha, plan.Step{Name: "worse", Caches: seen(t), Retries: 1, Cache: false, Run: counting(3)})
	if got.Status != "failed" || got.Attempts != 2 || got.Flaky || !strings.Contains(got.Detail, "try 2") {
		t.Errorf("%+v, want it failed after its second try", got)
	}

	// And one that does not ask is run once.
	got = run(t, cfg, r, sha, plan.Step{Name: "once", Caches: seen(t), Cache: false, Run: counting(2)})
	if got.Status != "failed" || got.Attempts != 0 || got.Output != "try 1\n" {
		t.Errorf("%+v, want it run once and failed", got)
	}
}

func TestTheMachinesFailuresAreTriedAgainUnasked(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir(), MachineRetries: 2}
	// The first time, it waits to be killed. After that it passes.
	s := plan.Step{Name: "killed", Image: testImage, Inputs: []string{"."}, Caches: seen(t), Cache: true,
		Run: "if test -f /seen/once; then echo recovered; else touch /seen/once; echo waiting 31340; sleep 31340; fi"}

	killed := make(chan struct{})
	var logged []string
	got := Run(context.Background(), cfg, r.dir, sha, s, func(line string) {
		logged = append(logged, line)
		if strings.Contains(line, "waiting 31340") {
			// From outside, as a machine being cleared would.
			go func() {
				defer close(killed)
				out, _ := exec.Command("docker", "ps", "--no-trunc", "--format", "{{.Names}} {{.Command}}").Output()
				for _, c := range strings.Split(string(out), "\n") {
					if strings.Contains(c, "sleep 31340") {
						exec.Command("docker", "kill", strings.Fields(c)[0]).Run()
					}
				}
			}()
		}
	})
	<-killed
	// Tried twice, and not flaky: it was not the step that failed.
	if got.Status != "passed" || got.Attempts != 2 || got.Flaky || got.Output != "recovered\n" {
		t.Fatalf("%+v, want it run again after being killed, and passing", got)
	}
	if all := strings.Join(logged, "\n"); !strings.Contains(all, "that was the machine's failure and not the step's") {
		t.Errorf("the log does not say whose failure it was:\n%s", all)
	}
}

func TestWhoseFailureItWas(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	// A machine that would retry, so that anything classed as its own
	// failure shows up as a second try.
	cfg := Config{Worker: "one", StateDir: t.TempDir(), MachineRetries: 1}
	for name, c := range map[string]struct {
		step    plan.Step
		sha     string
		machine bool
	}{
		"the command failing":         {plan.Step{Run: "exit 1"}, sha, false},
		"running out of time":         {plan.Step{Run: "sleep 60", Timeout: 2}, sha, false},
		"running out of memory":       {plan.Step{Run: "tail /dev/zero", Memory: 32 << 20}, sha, false},
		"a commit that is not there":  {plan.Step{Run: "true"}, strings.Repeat("0", 40), true},
		"an image that cannot be had": {plan.Step{Run: "true", Image: "localhost:1/starci/no-such-image:1"}, sha, true},
	} {
		t.Run(name, func(t *testing.T) {
			c.step.Name, c.step.Cache = "x", false
			got := run(t, cfg, r, c.sha, c.step)
			if got.Status != "failed" || got.Machine != c.machine {
				t.Errorf("%+v, want it failed, the machine's = %v", got, c.machine)
			}
			// The machine's are tried once more here; the step's own are not.
			if tries := max(got.Attempts, 1); (tries == 2) != c.machine {
				t.Errorf("it was tried %d time(s)", tries)
			}
		})
	}
}

func TestNothingIsTriedAgainAfterBeingCancelled(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir(), MachineRetries: 2}
	ctx, cancel := context.WithCancel(context.Background())
	began := time.Now()
	got := Run(ctx, cfg, r.dir, sha, plan.Step{Name: "replaced", Image: testImage, Inputs: []string{"."}, Retries: 3, Cache: false, Run: "echo started; sleep 600"},
		func(line string) {
			if line == "started" {
				cancel()
			}
		})
	if got.Status != "failed" || got.Detail != "cancelled" || got.Attempts > 1 {
		t.Errorf("%+v, want it cancelled and not tried again", got)
	}
	if took := time.Since(began); took > time.Minute {
		t.Errorf("it took %s to stop", took)
	}
}

// A command that had started may have done what it does. One whose pass
// would be reused is run again; one marked as having an effect is not.
func TestAKilledStepWithAnEffectIsNotRunAgainUnasked(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir(), MachineRetries: 2}
	s := plan.Step{Name: "deploy", Image: testImage, Inputs: []string{"."}, Cache: false, Run: "echo deploying 31342; sleep 31342"}
	got := Run(context.Background(), cfg, r.dir, sha, s, func(line string) {
		if strings.Contains(line, "deploying 31342") {
			go func() {
				out, _ := exec.Command("docker", "ps", "--no-trunc", "--format", "{{.Names}} {{.Command}}").Output()
				for _, c := range strings.Split(string(out), "\n") {
					if strings.Contains(c, "sleep 31342") {
						exec.Command("docker", "kill", strings.Fields(c)[0]).Run()
					}
				}
			}()
		}
	})
	if got.Status != "failed" || got.Machine || got.Attempts > 1 || !strings.Contains(got.Detail, "it may already have done its work") {
		t.Errorf("%+v, want it failed once, saying why it was not run again", got)
	}
}
