package plan

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner stands in for the machines. It answers each step from a table
// keyed by step name, and records what it was asked to run, batch by batch.
type fakeRunner struct {
	// outcomes maps a step name to the results of its successive runs; the
	// last one repeats. A step not listed passes with no output.
	outcomes map[string][]Result
	seen     map[string]int
	batches  [][]string
}

func (f *fakeRunner) Run(steps []Step) ([]Result, error) {
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
	return out, nil
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
	return RunPipeline(context.Background(), "ci.star", []byte(src), ctx, r)
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
	_, err := RunPipeline(cancelled, "ci.star", []byte(`
def pipeline(ctx):
    for i in range(100000000):
        pass
`), ctx, &fakeRunner{})
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
