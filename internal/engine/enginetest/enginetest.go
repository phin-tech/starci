// Package enginetest is what an engine has to get right, as tests.
//
// An engine is anything that carries a run out. That it does so correctly is
// not a matter of its own tests agreeing with its own code: every engine is
// put through the same runs here and has to produce the same results.
//
//	func TestEngine(t *testing.T) { enginetest.Run(t, myEngine) }
//
// The runs use real containers and a real git repository, made for the
// purpose in a temporary directory. The engine's steps must be able to fetch
// from a path on this machine, which means its machines are on this machine.
package enginetest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
)

const image = "alpine:3"

// Run puts an engine through every test that applies to it.
func Run(t *testing.T, eng engine.Engine) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs containers; skipped with -short")
	}
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
	repo := newRepo(t)

	t.Run("a basic plan runs in order and skips what depends on a failure", func(t *testing.T) { basicPlan(t, eng, repo) })
	t.Run("steps are given the commit and only their inputs", func(t *testing.T) { inputs(t, eng, repo) })
	t.Run("a step that already passed is not run again", func(t *testing.T) { reuse(t, eng, repo) })
	t.Run("a step that gave up the network has none", func(t *testing.T) { offline(t, eng, repo) })
	t.Run("a step reaches the services it asked for, and they end with it", func(t *testing.T) { services(t, eng, repo) })
	t.Run("a bare step runs on a machine that runs them", func(t *testing.T) { bare(t, eng, repo) })
	t.Run("a step is handed what another kept, in either kind of script", func(t *testing.T) { artifacts(t, eng, repo) })
	t.Run("an advanced script acts on what its steps return", func(t *testing.T) { advanced(t, eng, repo) })
	t.Run("an advanced script that fails says why", func(t *testing.T) { advancedFailure(t, eng, repo) })
	t.Run("what the engine says it can do, it does", func(t *testing.T) { claims(t, eng, repo) })
}

type repo struct {
	dir, sha string
}

func newRepo(t *testing.T) repo {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	for name, content := range map[string]string{
		"src/a.txt":     "alpha\n",
		"src/b.txt":     "beta\n",
		"tests/one.txt": "1\n",
		"tests/two.txt": "2\n",
		"README.md":     "readme\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "start")
	return repo{dir: dir, sha: git("rev-parse", "HEAD")}
}

// execute evaluates a script and has the engine carry out the run.
func execute(t *testing.T, eng engine.Engine, r repo, src string) engine.Result {
	t.Helper()
	c := plan.Context{Repo: "test/repo", SHA: r.sha, Ref: "main", Event: plan.EventManual}
	run := engine.Run{Context: c, CloneURL: r.dir}
	mode, err := plan.Mode("ci.star", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	run.Mode = mode
	if mode == plan.Basic {
		if run.Plan, err = plan.Eval("ci.star", []byte(src), c); err != nil {
			t.Fatal(err)
		}
	} else {
		run.Filename, run.Source = "ci.star", []byte(src)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := eng.Execute(ctx, run)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

func statuses(result engine.Result) string {
	var parts []string
	for _, s := range result.Steps {
		parts = append(parts, s.Name+"="+s.Status)
	}
	return strings.Join(parts, " ")
}

func step(result engine.Result, name string) plan.Result {
	for _, s := range result.Steps {
		if s.Name == name {
			return s
		}
	}
	return plan.Result{}
}

// unique makes a command no earlier test run could have stored a result for.
func unique() string {
	return "echo " + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
}

const helpers = `
def sh(name, command, **kwargs):
    return step(name, image = "` + image + `", run = command, **kwargs)
`

func basicPlan(t *testing.T, eng engine.Engine, r repo) {
	result := execute(t, eng, r, helpers+`
def ci(ctx):
    ok = sh("ok", "true", cache = False)
    bad = sh("bad", "echo about to fail; exit 3", cache = False)
    return [
        ok,
        bad,
        sh("after ok", "true", needs = [ok], cache = False),
        sh("after bad", "true", needs = [bad], cache = False),
        sh("after both", "true", needs = ["after ok", "after bad"], cache = False),
    ]
`)
	if got, want := statuses(result), "ok=passed bad=failed after ok=passed after bad=skipped after both=skipped"; got != want {
		t.Errorf("results:\n  %s\nwant:\n  %s", got, want)
	}
	if result.OK {
		t.Error("the run is reported as passing with a failed step")
	}
	// Why a step failed has to reach whoever asked.
	if d := step(result, "bad").Detail; !strings.Contains(d, "exit code 3") || !strings.Contains(d, "about to fail") {
		t.Errorf("the failed step's detail is %q; want its exit code and what it printed", d)
	}
	if d := step(result, "after bad").Detail; !strings.Contains(d, "bad") {
		t.Errorf("the skipped step's detail is %q; want it to name what it was waiting on", d)
	}
}

func artifacts(t *testing.T, eng engine.Engine, r repo) {
	build := "mkdir -p out && printf '#!/bin/sh\\necho made\\n' > out/app && chmod +x out/app"
	for _, src := range []string{helpers + `
def ci(ctx):
    build = sh("build", "` + build + `", keep = ["out"], cache = False)
    return [build, sh("use", "test \"$(./bin/app)\" = made", artifacts = {"bin": artifact(build, "out")}, cache = False)]
`, helpers + `
def pipeline(ctx):
    build = run(sh("build", "` + build + `", keep = ["out"], cache = False))
    run(sh("use", "test \"$(./bin/app)\" = made", artifacts = {"bin": build.artifacts["out"]}, cache = False))
`} {
		result := execute(t, eng, r, src)
		if !result.OK {
			t.Fatalf("results: %s (%s%s%s)", statuses(result), result.Error, step(result, "build").Detail, step(result, "use").Detail)
		}
		if step(result, "build").Artifacts["out"].Hash == "" {
			t.Errorf("build's result carries no pointer to what it kept: %+v", step(result, "build"))
		}
	}
}

func inputs(t *testing.T, eng engine.Engine, r repo) {
	result := execute(t, eng, r, helpers+`
def ci(ctx):
    return [sh("look", "`+unique()+` >/dev/null; find . -type f | sort", inputs = ["src", "tests/one.txt"])]
`)
	if !result.OK {
		t.Fatalf("results: %s (%s)", statuses(result), step(result, "look").Detail)
	}
	if got, want := step(result, "look").Output, "./src/a.txt\n./src/b.txt\n./tests/one.txt\n"; got != want {
		t.Errorf("the step saw:\n%swant:\n%s", got, want)
	}
}

// offline is here because the choice is the step's and the container is
// started wherever the engine sends it: the choice has to make the trip.
func offline(t *testing.T, eng engine.Engine, r repo) {
	result := execute(t, eng, r, helpers+`
def ci(ctx):
    return [
        sh("with", "ls /sys/class/net", cache = False),
        sh("without", "ls /sys/class/net", cache = False, network = False),
    ]
`)
	if !result.OK {
		t.Fatalf("results: %s", statuses(result))
	}
	if got := step(result, "without").Output; got != "lo\n" {
		t.Errorf("a step with network = False has these interfaces, and should have only lo:\n%s", got)
	}
	if got := step(result, "with").Output; got == "lo\n" {
		t.Error("a step that did not give up the network has only lo")
	}
}

// bare is here because a bare step can only go to a machine that has agreed
// to run one, and the engine has to find it one. The engine's machines are
// expected to have agreed.
func bare(t *testing.T, eng engine.Engine, r repo) {
	result := execute(t, eng, r, `
def ci(ctx):
    return [step("look", run = "`+unique()+` >/dev/null; find . -type f | sort", inputs = ["src"], cache = False)]
`)
	if !result.OK {
		t.Fatalf("results: %s (%s)", statuses(result), step(result, "look").Detail)
	}
	if got, want := step(result, "look").Output, "./src/a.txt\n./src/b.txt\n"; got != want {
		t.Errorf("the bare step saw:\n%swant:\n%s", got, want)
	}
}

// services is here for the same reason: a step's services are started on
// whatever machine the engine sends the step to, so the engine has to carry
// them there, and an engine that does not run steps through internal/step
// has to provide them itself. What is asked of it is that each answers to its
// name, that the step does not start before a service is ready, and that
// nothing of them is left afterwards.
func services(t *testing.T, eng engine.Engine, r repo) {
	// Names no other run on this machine is using, so that what is left of
	// them afterwards can be told from anyone else's.
	id := strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	quick, slow := "quick"+id, "slow"+id
	result := execute(t, eng, r, helpers+`
def web(name, delay):
    return service(name, image = "`+image+`",
                   args = ["sh", "-c", "sleep " + delay + "; exec nc -lk -p 8080 -e hostname"],
                   ready = "nc -w 2 127.0.0.1 8080 </dev/null | grep -q .")

def ci(ctx):
    return [sh("ask", "nc -w 2 `+quick+` 8080 </dev/null && nc -w 2 `+slow+` 8080 </dev/null",
               cache = False, services = [web("`+quick+`", "0"), web("`+slow+`", "2")])]
`)
	if !result.OK {
		t.Fatalf("results: %s (%s)", statuses(result), step(result, "ask").Detail)
	}
	if got, want := step(result, "ask").Output, quick+"\n"+slow+"\n"; got != want {
		t.Errorf("the step heard %q from its services, want %q", got, want)
	}
	// Where an engine's machines are during these tests is this machine.
	if out, _ := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}", "--filter", "name="+id).Output(); len(out) > 0 {
		t.Errorf("services outlived their step:\n%s", out)
	}
}

func reuse(t *testing.T, eng engine.Engine, r repo) {
	src := helpers + `
def ci(ctx):
    return [sh("once", "` + unique() + `", inputs = ["src"])]
`
	first := execute(t, eng, r, src)
	if !first.OK || step(first, "once").Cached {
		t.Fatalf("first run: %s, cached = %v", statuses(first), step(first, "once").Cached)
	}
	again := execute(t, eng, r, src)
	got := step(again, "once")
	if !again.OK || !got.Cached {
		t.Errorf("second run: %s, cached = %v; want the first run's pass reused", statuses(again), got.Cached)
	}
	// A reused pass hands back what the step printed when it ran.
	if got.Output != step(first, "once").Output || got.Output == "" {
		t.Errorf("the reused result's output is %q, the original's %q", got.Output, step(first, "once").Output)
	}
}

func advanced(t *testing.T, eng engine.Engine, r repo) {
	result := execute(t, eng, r, helpers+`
def pipeline(ctx):
    # How wide to fan out is read from a step.
    listing = run(sh("list", "ls tests", inputs = ["tests"], cache = False))
    files = listing.output.splitlines()
    results = parallel([sh("check " + f, "test " + f + " != two.txt", cache = False) for f in files], allow_failure = True)

    # And what happens next from how those went.
    failed = [r.name for r in results if not r.ok]
    if failed:
        run(sh("report", "echo failed: " + ", ".join(failed), cache = False))
    else:
        run(sh("celebrate", "true", cache = False))
`)
	if got, want := statuses(result), "list=passed check one.txt=passed check two.txt=failed report=passed"; got != want {
		t.Errorf("results:\n  %s\nwant:\n  %s", got, want)
	}
	// The script allowed for the failure and carried on to the end.
	if !result.OK || result.Error != "" {
		t.Errorf("OK = %v, error = %q; want a run that passed", result.OK, result.Error)
	}
	if got := step(result, "report").Output; got != "failed: check two.txt\n" {
		t.Errorf("report printed %q", got)
	}
}

func advancedFailure(t *testing.T, eng engine.Engine, r repo) {
	stopped := execute(t, eng, r, helpers+`
def pipeline(ctx):
    run(sh("first", "true", cache = False))
    run(sh("breaks", "exit 1", cache = False))
    run(sh("never", "true", cache = False))
`)
	if got, want := statuses(stopped), "first=passed breaks=failed"; got != want {
		t.Errorf("results: %s; want %s, with nothing run after the failure", got, want)
	}
	if stopped.OK || !strings.Contains(stopped.Error, `step "breaks" failed`) {
		t.Errorf("OK = %v, error = %q", stopped.OK, stopped.Error)
	}

	gaveUp := execute(t, eng, r, helpers+`
def pipeline(ctx):
    run(sh("fine", "true", cache = False))
    fail("the script gave up on purpose")
`)
	if gaveUp.OK || !strings.Contains(gaveUp.Error, "the script gave up on purpose") {
		t.Errorf("OK = %v, error = %q; want the script's own words", gaveUp.OK, gaveUp.Error)
	}
}

// claims holds an engine to what Can says. An engine that claims an ability
// and lacks it is worse than one that says it has none.
func claims(t *testing.T, eng engine.Engine, r repo) {
	can := eng.Can()
	run := engine.Run{Mode: plan.Advanced, Plan: &plan.Plan{Steps: []plan.Step{{Name: "gpu", On: map[string]string{"gpu": "true"}}}}}
	missing := strings.Join(can.Missing(run), "; ")
	if can.Placement == strings.Contains(missing, "on, prefer and avoid") {
		t.Errorf("Placement = %v, but for a step that asks for a label Missing says %q", can.Placement, missing)
	}
	if can.Durable == strings.Contains(missing, "cannot pick up where it was") {
		t.Errorf("Durable = %v, but for an advanced run Missing says %q", can.Durable, missing)
	}
	// Whether an engine that places steps puts them where they ask cannot be
	// asked of every engine alike: it depends on what machines it has. Such an
	// engine tests that itself.
}
