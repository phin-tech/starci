package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// A script is written in one of two modes, told apart by the function it
// defines.
//
// Basic: ci(ctx) returns the steps, and the whole graph is known before
// anything runs.
//
// Advanced: pipeline(ctx) runs steps itself with run() and parallel(), sees
// their results, and decides what to do next:
//
//	def pipeline(ctx):
//	    files = run(step("list", image = "alpine", run = "ls tests")).output.splitlines()
//	    parallel([step("test " + f, image = "py", run = "pytest tests/" + f) for f in files])
//
// An advanced script is still deterministic in the sense that matters for
// running it durably: it is a pure function of ctx and of the results it has
// been handed. Evaluate it again with the same results and it takes the same
// path, which is how a run interrupted halfway is resumed. What it gives up is
// a graph that can be known in advance.
const (
	Basic    = "basic"
	Advanced = "advanced"
)

// Result is what a script sees of a step it ran.
type Result struct {
	Name    string  `json:"name"`
	Status  string  `json:"status"` // passed, failed or skipped
	Output  string  `json:"output"`
	Seconds float64 `json:"seconds"`
	Worker  string  `json:"worker"`
	Cached  bool    `json:"cached"`
	Detail  string  `json:"detail"`
}

func (r Result) ok() bool { return r.Status == "passed" }

// Runner executes steps on behalf of a running script. Run starts all the
// given steps together and returns their results in the same order. An error
// means the steps could not be run at all, not that one of them failed.
type Runner interface {
	Run(steps []Step) ([]Result, error)
}

const runnerKey = "starci.runner"

// Mode reports which mode the script is written in.
func Mode(filename string, src []byte) (string, error) {
	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, newThread("mode"), filename, src, predeclared())
	if err != nil {
		return "", describe(err)
	}
	_, basic := globals["ci"].(starlark.Callable)
	_, advanced := globals["pipeline"].(starlark.Callable)
	switch {
	case basic && advanced:
		return "", fmt.Errorf("%s: defines both ci(ctx) and pipeline(ctx); a script is one or the other", filename)
	case basic:
		return Basic, nil
	case advanced:
		return Advanced, nil
	}
	return "", fmt.Errorf("%s: must define ci(ctx) or pipeline(ctx)", filename)
}

// RunPipeline runs an advanced script to completion, executing its steps
// through r. It returns every result the script was handed, in the order the
// steps were started, along with the error that ended it, if one did: a step
// that failed without allow_failure, a call to fail(), or a mistake in the
// script.
func RunPipeline(ctx context.Context, filename string, src []byte, c Context, r Runner) ([]Result, error) {
	thread := newThread("pipeline")
	rec := &recorder{runner: r}
	thread.SetLocal(runnerKey, rec)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("cancelled")
		case <-done:
		}
	}()

	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, filename, src, predeclared())
	if err != nil {
		return rec.results, describe(err)
	}
	fn, ok := globals["pipeline"].(starlark.Callable)
	if !ok {
		return rec.results, fmt.Errorf("%s: must define a function pipeline(ctx)", filename)
	}
	rec.started = true
	if _, err := starlark.Call(thread, fn, starlark.Tuple{contextValue(c)}, nil); err != nil {
		var failed *stepsFailed
		if errors.As(err, &failed) {
			return rec.results, failed
		}
		return rec.results, describe(err)
	}
	return rec.results, nil
}

// recorder sits between the script and the Runner, keeping every result in
// the order the steps were started.
type recorder struct {
	runner  Runner
	results []Result
	// started is false while the file's top level is being evaluated, where
	// running a step is refused: the top level also runs just to find out
	// which mode a script is in.
	started bool
}

// stepsFailed is the error that ends a pipeline when a step fails and the
// script did not ask to carry on.
type stepsFailed struct{ names []string }

func (e *stepsFailed) Error() string {
	if len(e.names) == 1 {
		return fmt.Sprintf("step %q failed", e.names[0])
	}
	return fmt.Sprintf("%d steps failed: %s", len(e.names), strings.Join(e.names, ", "))
}

func newThread(name string) *starlark.Thread {
	thread := &starlark.Thread{
		Name:  name,
		Print: func(*starlark.Thread, string) {}, // output would be the one impure thing a script can do
		Load: func(_ *starlark.Thread, module string) (starlark.StringDict, error) {
			return nil, fmt.Errorf("load(%q): loading other files is not supported", module)
		},
	}
	thread.SetMaxExecutionSteps(maxSteps)
	return thread
}

func predeclared() starlark.StringDict {
	return starlark.StringDict{
		"step":     starlark.NewBuiltin("step", stepBuiltin),
		"tool":     starlark.NewBuiltin("tool", toolBuiltin),
		"run":      starlark.NewBuiltin("run", runBuiltin),
		"parallel": starlark.NewBuiltin("parallel", parallelBuiltin),
	}
}

func runBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s *stepValue
	allowFailure := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "step", &s, "allow_failure?", &allowFailure); err != nil {
		return nil, err
	}
	results, err := execute(thread, b.Name(), []Step{s.Step}, allowFailure)
	if err != nil {
		return nil, err
	}
	return resultValue(results[0]), nil
}

func parallelBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var list starlark.Iterable
	allowFailure := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "steps", &list, "allow_failure?", &allowFailure); err != nil {
		return nil, err
	}
	var steps []Step
	it := list.Iterate()
	defer it.Done()
	var v starlark.Value
	for it.Next(&v) {
		s, ok := v.(*stepValue)
		if !ok {
			return nil, fmt.Errorf("parallel: expects steps, got %s", v.Type())
		}
		steps = append(steps, s.Step)
	}
	if len(steps) == 0 {
		return starlark.NewList(nil), nil
	}
	results, err := execute(thread, b.Name(), steps, allowFailure)
	if err != nil {
		return nil, err
	}
	out := make([]starlark.Value, len(results))
	for i, r := range results {
		out[i] = resultValue(r)
	}
	return starlark.NewList(out), nil
}

func execute(thread *starlark.Thread, fn string, steps []Step, allowFailure bool) ([]Result, error) {
	rec, _ := thread.Local(runnerKey).(*recorder)
	if rec == nil {
		return nil, fmt.Errorf("%s: steps can only be run from pipeline(ctx); ci(ctx) returns them instead", fn)
	}
	if !rec.started {
		return nil, fmt.Errorf("%s: steps can only be run inside pipeline(ctx), not at the top of the file", fn)
	}
	for _, s := range steps {
		if len(s.Needs) > 0 {
			return nil, fmt.Errorf("step %q: needs has no meaning in pipeline(ctx); run steps in the order you need them", s.Name)
		}
	}
	results, err := rec.runner.Run(steps)
	if err != nil {
		return nil, err
	}
	if len(results) != len(steps) {
		return nil, fmt.Errorf("%s: ran %d steps but got %d results", fn, len(steps), len(results))
	}
	rec.results = append(rec.results, results...)

	if !allowFailure {
		var failed []string
		for _, r := range results {
			if !r.ok() {
				failed = append(failed, r.Name)
			}
		}
		if len(failed) > 0 {
			return nil, &stepsFailed{names: failed}
		}
	}
	return results, nil
}

func resultValue(r Result) starlark.Value {
	return starlarkstruct.FromStringDict(starlark.String("result"), starlark.StringDict{
		"name":    starlark.String(r.Name),
		"ok":      starlark.Bool(r.ok()),
		"status":  starlark.String(r.Status),
		"output":  starlark.String(r.Output),
		"seconds": starlark.Float(r.Seconds),
		"cached":  starlark.Bool(r.Cached),
		"worker":  starlark.String(r.Worker),
		"detail":  starlark.String(r.Detail),
	})
}
