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
// from a path on this machine, which means its workers are on this machine.
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
	t.Run("a step is given what another produced", func(t *testing.T) { outputs(t, eng, repo) })
	t.Run("an advanced script hands one step's outputs to the next", func(t *testing.T) { advancedOutputs(t, eng, repo) })
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

func outputs(t *testing.T, eng engine.Engine, r repo) {
	src := helpers + `
def ci(ctx):
    build = sh("build", "mkdir dist && cat src/a.txt src/b.txt > dist/all.txt && ` + unique() + `", inputs = ["src"], outputs = ["dist"])
    return [
        build,
        sh("use", "cat dist/all.txt; ls", inputs = ["tests", build]),
        sh("blind", "test ! -e dist", inputs = ["tests"], needs = [build]),
    ]
`
	first := execute(t, eng, r, src)
	if got, want := statuses(first), "build=passed use=passed blind=passed"; got != want {
		t.Fatalf("results:\n  %s\nwant:\n  %s\n%s", got, want, step(first, "use").Detail)
	}
	// What build left, beside the step's own inputs and nothing else.
	if got := step(first, "use").Output; got != "alpha\nbeta\ndist\ntests\n" {
		t.Errorf("use printed %q", got)
	}
	// A step that is not run again still hands on what it produced, and the
	// step given the same thing is not run again either.
	again := execute(t, eng, r, src)
	if !again.OK || !step(again, "build").Cached || !step(again, "use").Cached {
		t.Errorf("second run: %s, build cached = %v, use cached = %v; want both reused",
			statuses(again), step(again, "build").Cached, step(again, "use").Cached)
	}
}

func advancedOutputs(t *testing.T, eng engine.Engine, r repo) {
	result := execute(t, eng, r, helpers+`
def pipeline(ctx):
    build = sh("build", "cp src/a.txt out.txt", inputs = ["src"], outputs = ["out.txt"], cache = False)
    run(build)
    parallel([sh("use " + n, "cat out.txt", inputs = [build], cache = False) for n in ["one", "two"]])
`)
	if got, want := statuses(result), "build=passed use one=passed use two=passed"; got != want {
		t.Fatalf("results:\n  %s\nwant:\n  %s\n%s %s", got, want, result.Error, step(result, "use one").Detail)
	}
	if got := step(result, "use two").Output; got != "alpha\n" {
		t.Errorf("use two printed %q", got)
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
	run := engine.Run{Mode: plan.Advanced, Plan: &plan.Plan{Steps: []plan.Step{{Name: "gpu", Worker: map[string]string{"gpu": "true"}}}}}
	missing := strings.Join(can.Missing(run), "; ")
	if can.Placement == strings.Contains(missing, "worker, prefer and avoid") {
		t.Errorf("Placement = %v, but for a step that asks for a worker Missing says %q", can.Placement, missing)
	}
	if can.Durable == strings.Contains(missing, "cannot pick up where it was") {
		t.Errorf("Durable = %v, but for an advanced run Missing says %q", can.Durable, missing)
	}

	if !can.Placement {
		return
	}
	fleet, ok := eng.(engine.Fleet)
	if !ok {
		t.Fatal("the engine places steps on workers but cannot say what workers it has")
	}
	workers, err := fleet.Workers(context.Background())
	if err != nil || len(workers) == 0 {
		t.Fatalf("Workers = %v, %v; want at least the one running these tests", workers, err)
	}
	// A step that asks for a label this worker has, runs; nothing here can
	// test that one asking for a label nobody has waits, without waiting.
	var label, value string
	for label, value = range workers[0].Labels {
		break
	}
	if label == "" {
		t.Skip("the worker has no labels to ask for")
	}
	result := execute(t, eng, r, helpers+`
def ci(ctx):
    return [sh("placed", "true", cache = False, worker = {"`+label+`": "`+value+`"})]
`)
	if !result.OK {
		t.Errorf("a step asking for %s=%s, which a worker offers, did not run: %s", label, value, statuses(result))
	}
}
