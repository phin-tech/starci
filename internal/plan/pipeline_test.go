package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeRunner stands in for an engine and its machines. It carries a run out
// by asking what is next and running all of it at once, answers each step
// from a table keyed by step name, and records what it ran, batch by batch.
type fakeRunner struct {
	// outcomes maps a step name to the results of its successive runs; the
	// last one repeats. A step not listed passes with no output.
	outcomes map[string][]Result
	seen     map[string]int
	batches  [][]string
}

func (f *fakeRunner) drive(next func([]Result) (Decision, error)) ([]Result, error) {
	var results []Result
	for {
		d, err := next(results)
		if err != nil {
			return nil, err
		}
		if d.Done {
			if d.Error != "" {
				return d.Results, errors.New(d.Error)
			}
			return d.Results, nil
		}
		var steps []Step
		for _, a := range d.Run {
			steps = append(steps, a.Step)
		}
		for i, r := range f.run(steps) {
			results = append(results, d.Run[i].Answer(r))
		}
	}
}

func (f *fakeRunner) run(steps []Step) []Result {
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	var names []string
	var out []Result
	for _, s := range steps {
		names = append(names, s.Name)
		r := Result{Status: "passed"}
		if runs := f.outcomes[s.Name]; len(runs) > 0 {
			i := f.seen[s.Name]
			if i >= len(runs) {
				i = len(runs) - 1
			}
			r = runs[i]
		}
		f.seen[s.Name]++
		r.Name = s.Name
		out = append(out, r)
	}
	f.batches = append(f.batches, names)
	return out
}

func (f *fakeRunner) trace() string {
	var parts []string
	for _, b := range f.batches {
		parts = append(parts, strings.Join(b, "+"))
	}
	return strings.Join(parts, " > ")
}

func pipeline(t *testing.T, src string, r *fakeRunner) ([]Result, error) {
	t.Helper()
	return r.drive(func(results []Result) (Decision, error) {
		return NextInPipeline(context.Background(), "ci.star", []byte(src), ctx, results)
	})
}

const helpers = `
def s(name):
    return step(name, image = "i", run = "r")
`

func TestPipelineRunsStepsInOrder(t *testing.T) {
	r := &fakeRunner{}
	results, err := pipeline(t, helpers+`
def pipeline(ctx):
    run(s("build"))
    parallel([s("lint"), s("test")])
    run(s("publish"))
`, r)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if got, want := r.trace(), "build > lint+test > publish"; got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
	if len(results) != 4 {
		t.Errorf("got %d results, want 4", len(results))
	}
}

// The thing a fixed plan cannot do: how wide to fan out is read from a step.
func TestPipelineFansOutOverAStepsOutput(t *testing.T) {
	r := &fakeRunner{outcomes: map[string][]Result{
		"list": {{Status: "passed", Output: "a.py\nb.py\nc.py\n"}},
	}}
	_, err := pipeline(t, helpers+`
def pipeline(ctx):
    files = run(s("list")).output.splitlines()
    parallel([s("test " + f) for f in files])
`, r)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if got, want := r.trace(), "list > test a.py+test b.py+test c.py"; got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
}

func TestPipelineRetriesOnlyWhatFailed(t *testing.T) {
	failed, passed := Result{Status: "failed"}, Result{Status: "passed"}
	r := &fakeRunner{outcomes: map[string][]Result{
		"flaky":  {failed, passed},
		"broken": {failed},
	}}
	_, err := pipeline(t, helpers+`
def pipeline(ctx):
    results = parallel([s("solid"), s("flaky"), s("broken")], allow_failure = True)
    again = [r.name for r in results if not r.ok]
    for attempt in range(2):
        if not again:
            break
        results = parallel([s(n) for n in again], allow_failure = True)
        again = [r.name for r in results if not r.ok]
    if again:
        fail("still failing after retries: " + ", ".join(again))
`, r)
	if got, want := r.trace(), "solid+flaky+broken > flaky+broken > broken"; got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "still failing after retries: broken") {
		t.Errorf("error = %v, want the script's own failure naming broken", err)
	}
}

func TestPipelineStopsAtAFailedStep(t *testing.T) {
	r := &fakeRunner{outcomes: map[string][]Result{"test": {{Status: "failed"}}}}
	results, err := pipeline(t, helpers+`
def pipeline(ctx):
    run(s("build"))
    run(s("test"))
    run(s("publish"))
`, r)
	if err == nil || err.Error() != `step "test" failed` {
		t.Fatalf("error = %v, want step \"test\" failed", err)
	}
	if got, want := r.trace(), "build > test"; got != want {
		t.Errorf("ran %s, want %s: nothing after a failed step should run", got, want)
	}
	if len(results) != 2 || results[1].Status != "failed" {
		t.Errorf("results = %+v, want build and the failed test", results)
	}
}

func TestPipelineCanDecideOnAFailure(t *testing.T) {
	r := &fakeRunner{outcomes: map[string][]Result{"test": {{Status: "failed"}}}}
	_, err := pipeline(t, helpers+`
def pipeline(ctx):
    if run(s("test"), allow_failure = True).ok:
        run(s("publish"))
    else:
        run(s("open-issue"))
`, r)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if got, want := r.trace(), "test > open-issue"; got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
}

// What makes a half-finished run resumable: given the same results, a script
// asks for the same steps in the same order.
func TestPipelineReplaysIdentically(t *testing.T) {
	src := helpers + `
def pipeline(ctx):
    files = run(s("list")).output.splitlines()
    results = parallel([s("test " + f) for f in files], allow_failure = True)
    for r in results:
        if not r.ok:
            run(s("report " + r.name))
`
	outcomes := func() map[string][]Result {
		return map[string][]Result{
			"list":   {{Status: "passed", Output: "a\nb\nc"}},
			"test b": {{Status: "failed"}},
		}
	}
	first := &fakeRunner{outcomes: outcomes()}
	if _, err := pipeline(t, src, first); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	for i := 0; i < 10; i++ {
		again := &fakeRunner{outcomes: outcomes()}
		if _, err := pipeline(t, src, again); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if again.trace() != first.trace() {
			t.Fatalf("replay %d ran %s, first run ran %s", i, again.trace(), first.trace())
		}
	}
	if want := "list > test a+test b+test c > report test b"; first.trace() != want {
		t.Errorf("ran %s, want %s", first.trace(), want)
	}
}

func TestPipelineMistakes(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"running a step at the top of the file", helpers + `
run(s("early"))
def pipeline(ctx):
    pass
`, "not at the top of the file"},
		{"needs", `
def pipeline(ctx):
    a = step("a", image = "i", run = "r")
    run(step("b", image = "i", run = "r", needs = [a]))
`, "needs has no meaning"},
		{"running something that is not a step", `
def pipeline(ctx):
    run("build")
`, "run: for parameter step"},
		{"parallel over non-steps", `
def pipeline(ctx):
    parallel(["a", "b"])
`, "parallel: expects steps"},
		{"a runaway loop", `
def pipeline(ctx):
    x = 0
    for i in range(1000000000):
        x += i
`, "too many steps"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := pipeline(t, c.src, &fakeRunner{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestRunIsRefusedInBasicMode(t *testing.T) {
	wantError(t, helpers+`
def ci(ctx):
    run(s("build"))
    return []
`, "steps can only be run from pipeline(ctx)")
}

func TestPipelineStopsWhenCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&fakeRunner{}).drive(func(results []Result) (Decision, error) {
		return NextInPipeline(cancelled, "ci.star", []byte(`
def pipeline(ctx):
    for i in range(100000000):
        pass
`), ctx, results)
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v, want cancelled", err)
	}
}

func TestMode(t *testing.T) {
	cases := []struct{ name, src, want, wantErr string }{
		{"basic", "def ci(ctx):\n    return []\n", Basic, ""},
		{"advanced", "def pipeline(ctx):\n    pass\n", Advanced, ""},
		{"both", "def ci(ctx):\n    return []\ndef pipeline(ctx):\n    pass\n", "", "one or the other"},
		{"neither", "x = 1\n", "", "must define ci(ctx) or pipeline(ctx)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Mode("ci.star", []byte(c.src))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Mode = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestEvalExplainsAnAdvancedScript(t *testing.T) {
	_, err := Eval("ci.star", []byte("def pipeline(ctx):\n    pass\n"), ctx)
	if err == nil || !strings.Contains(err.Error(), "there is no plan to compute") {
		t.Fatalf("error = %v", err)
	}
}

func TestAPlanSkipsWhatDependsOnAFailure(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    ok = step("ok", image = "i", run = "r")
    bad = step("bad", image = "i", run = "r")
    return [
        ok,
        bad,
        step("after-ok", image = "i", run = "r", needs = [ok]),
        step("after-bad", image = "i", run = "r", needs = [bad]),
        step("after-both", image = "i", run = "r", needs = ["after-ok", "after-bad"]),
    ]
`)
	r := &fakeRunner{outcomes: map[string][]Result{"bad": {{Status: "failed"}}}}
	results, err := r.drive(p.Next)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if got, want := r.trace(), "ok+bad > after-ok"; got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
	var got []string
	for _, res := range results {
		got = append(got, res.Name+"="+res.Status)
	}
	want := "ok=passed bad=failed after-ok=passed after-bad=skipped after-both=skipped"
	if strings.Join(got, " ") != want {
		t.Errorf("results = %s, want %s", strings.Join(got, " "), want)
	}
	if Passed(results) {
		t.Error("Passed() = true for a plan with a failed step")
	}
}

// Steps are known by number, so a script may run two steps with one name and
// each gets its own answer, on every evaluation.
func TestPipelineStepsAreNumbered(t *testing.T) {
	failed, passed := Result{Status: "failed"}, Result{Status: "passed"}
	r := &fakeRunner{outcomes: map[string][]Result{"test": {failed, failed, passed}}}
	results, err := pipeline(t, helpers+`
def pipeline(ctx):
    for attempt in range(3):
        result = run(s("test"), allow_failure = True)
        if result.ok:
            run(s("passed on " + str(result.number)))
            break
`, r)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if got, want := r.trace(), "test > test > test > passed on 3"; got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
	for i, res := range results {
		if res.Number != i+1 {
			t.Errorf("result %d (%s) is #%d", i, res.Name, res.Number)
		}
	}
}

func TestNextAsksOnlyForWhatHasNoResult(t *testing.T) {
	src := helpers + `
def pipeline(ctx):
    run(s("build"))
    parallel([s("a"), s("b"), s("c")])
    run(s("publish"))
`
	next := func(results ...Result) Decision {
		t.Helper()
		d, err := NextInPipeline(context.Background(), "ci.star", []byte(src), ctx, results)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	numbers := func(d Decision) string {
		var parts []string
		for _, a := range d.Run {
			parts = append(parts, fmt.Sprintf("%s#%d", a.Step.Name, a.Number))
		}
		return strings.Join(parts, " ")
	}
	first := next()
	if got := numbers(first); got != "build#1" {
		t.Fatalf("first asked for %s", got)
	}
	built := first.Run[0].Answer(Result{Status: "passed"})
	if got := numbers(next(built)); got != "a#2 b#3 c#4" {
		t.Errorf("after build, asked for %s", got)
	}
	b := Result{Number: 3, Name: "b", Status: "passed"}
	if got := numbers(next(built, b)); got != "a#2 c#4" {
		t.Errorf("with b done, asked for %s", got)
	}
}

// What resuming depends on, checked: the same results must lead to the same
// steps, and a script that asks for something else is stopped.
func TestNextRefusesResultsForOtherSteps(t *testing.T) {
	src := helpers + `
def pipeline(ctx):
    run(s("build"))
`
	cases := []struct {
		name string
		r    Result
		want string
	}{
		{"another name", Result{Number: 1, Name: "lint", Status: "passed"}, `step #1 is now "build", where before it was "lint"`},
		{"another step of the same name", Result{Number: 1, Name: "build", Fingerprint: "0000000000000000", Status: "passed"}, "must ask for the same steps"},
		{"a step never asked for", Result{Number: 2, Name: "x", Status: "passed"}, "did not ask for: #2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			results := []Result{c.r}
			if c.r.Number == 2 {
				results = append(results, Result{Number: 1, Name: "build", Status: "passed"})
			}
			d, err := NextInPipeline(context.Background(), "ci.star", []byte(src), ctx, results)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Done || d.OK || !strings.Contains(d.Error, c.want) {
				t.Errorf("decision = %+v, want the run ended with %q", d, c.want)
			}
		})
	}
}

// A step in a basic plan is next once its own needs are done, not when the
// rest of its layer is.
func TestAPlanStepWaitsOnlyForItsOwnNeeds(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    a = step("a", image = "i", run = "r")
    slow = step("slow", image = "i", run = "r")
    return [a, slow, step("after-a", image = "i", run = "r", needs = [a])]
`)
	d, err := p.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := d.Run[0].Answer(Result{Status: "passed"})
	d, err = p.Next([]Result{a})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, w := range d.Run {
		names = append(names, w.Step.Name)
	}
	if got := strings.Join(names, " "); got != "slow after-a" {
		t.Errorf("with a done, asked for %s, want slow and after-a", got)
	}
}
