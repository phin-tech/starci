//go:build temporal

package temporal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
)

// The workflow is run under the SDK's test environment, with the real Next
// and a stand-in for running a step, so that what it decides is tested apart
// from containers and a server.

const diamond = `
def ci(ctx):
    a = step(name = "a", image = "alpine:3", run = "true")
    b = step(name = "b", image = "alpine:3", run = "true", needs = [a])
    c = step(name = "c", image = "alpine:3", run = "true", needs = [a])
    d = step(name = "d", image = "alpine:3", run = "true", needs = [b, c])
    return [a, b, c, d]
`

func basicInput(t *testing.T, src string) runInput {
	t.Helper()
	c := plan.Context{Repo: "test/repo", SHA: "abc", Ref: "main", Event: plan.EventManual}
	p, err := plan.Eval("ci.star", []byte(src), c)
	if err != nil {
		t.Fatal(err)
	}
	return runInput{ID: "run-1", Mode: plan.Basic, Plan: p, Context: c, CloneURL: "https://example.com/repo.git"}
}

// standIn runs no step: it answers each with the status named for it, and
// counts how often each was asked for.
type standIn struct {
	mu     sync.Mutex
	status map[string]string
	broken map[string]bool
	calls  map[string]int
	order  []string
}

func (s *standIn) Step(ctx context.Context, job stepJob) (plan.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := job.Asked.Step.Name
	s.calls[name]++
	s.order = append(s.order, name)
	if s.broken[name] {
		return plan.Result{}, errors.New("no machine could start it")
	}
	status := s.status[name]
	if status == "" {
		status = "passed"
	}
	return job.Asked.Answer(plan.Result{Status: status, Machine: "stand-in"}), nil
}

func runIt(t *testing.T, in runInput, s *standIn) engine.Result {
	t.Helper()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(runWorkflow, workflow.RegisterOptions{Name: workflowName})
	a := &activities{}
	env.RegisterActivityWithOptions(a.Next, activity.RegisterOptions{Name: nextName})
	env.RegisterActivityWithOptions(s.Step, activity.RegisterOptions{Name: stepName})
	env.ExecuteWorkflow(workflowName, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow: %v", err)
	}
	var r engine.Result
	if err := env.GetWorkflowResult(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func statuses(r engine.Result) string {
	var out []string
	for _, s := range r.Steps {
		out = append(out, s.Name+"="+s.Status)
	}
	return strings.Join(out, " ")
}

func TestWorkflowRunsAPlanInOrder(t *testing.T) {
	s := &standIn{}
	r := runIt(t, basicInput(t, diamond), s)
	if !r.OK || statuses(r) != "a=passed b=passed c=passed d=passed" {
		t.Fatalf("got ok=%v %s", r.OK, statuses(r))
	}
	if s.order[0] != "a" || s.order[3] != "d" {
		t.Errorf("ran in the order %v", s.order)
	}
}

func TestWorkflowSkipsWhatNeedsAFailureAndDoesNotRetryIt(t *testing.T) {
	s := &standIn{status: map[string]string{"b": "failed"}}
	r := runIt(t, basicInput(t, diamond), s)
	if r.OK || statuses(r) != "a=passed b=failed c=passed d=skipped" {
		t.Fatalf("got ok=%v %s", r.OK, statuses(r))
	}
	if s.calls["b"] != 1 {
		t.Errorf("a step that ran and failed was run %d times", s.calls["b"])
	}
	if s.calls["d"] != 0 {
		t.Errorf("a step that needs a failure was run")
	}
}

func TestWorkflowReportsAStepThatCouldNotBeRun(t *testing.T) {
	s := &standIn{broken: map[string]bool{"c": true}}
	r := runIt(t, basicInput(t, diamond), s)
	if r.OK || statuses(r) != "a=passed b=passed c=failed d=skipped" {
		t.Fatalf("got ok=%v %s", r.OK, statuses(r))
	}
	if s.calls["c"] != attempts {
		t.Errorf("a step no machine could start was tried %d times, want %d", s.calls["c"], attempts)
	}
	for _, res := range r.Steps {
		if res.Name == "c" && !strings.Contains(res.Detail, "could not be run") {
			t.Errorf("detail %q does not say it could not be run", res.Detail)
		}
	}
}

func TestWorkflowFollowsAnAdvancedScript(t *testing.T) {
	src := `
def pipeline(ctx):
    first = run(step(name = "probe", image = "alpine:3", run = "true"), allow_failure = True)
    if first.ok:
        parallel([step(name = "x", image = "alpine:3", run = "true"), step(name = "y", image = "alpine:3", run = "true")])
    else:
        run(step(name = "never", image = "alpine:3", run = "true"))
`
	c := plan.Context{Repo: "test/repo", SHA: "abc", Ref: "main", Event: plan.EventManual}
	in := runInput{ID: "run-2", Mode: plan.Advanced, Filename: "ci.star", Source: []byte(src), Context: c, CloneURL: "https://example.com/repo.git"}
	s := &standIn{}
	r := runIt(t, in, s)
	if !r.OK || statuses(r) != "probe=passed x=passed y=passed" {
		t.Fatalf("got ok=%v %s (error %q)", r.OK, statuses(r), r.Error)
	}

	s = &standIn{status: map[string]string{"probe": "failed"}}
	r = runIt(t, in, s)
	if s.calls["x"] != 0 {
		t.Errorf("took the branch for a pass after a failure: %s", statuses(r))
	}
}

func TestFairnessKeyFitsTemporalsLimit(t *testing.T) {
	if k := fairnessKey("sam/starci"); k != "sam/starci" {
		t.Errorf("a short name should be its own key, got %q", k)
	}
	long := strings.Repeat("x", 100)
	if k := fairnessKey(long); len(k) > 64 || k != fairnessKey(long) {
		t.Errorf("a long name's key %q is too long or not stable", k)
	}
}

var _ = temporal.Priority{} // the SDK this is built against has fairness keys
