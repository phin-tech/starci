// Package hatchet executes runs on Hatchet: steps become tasks that any
// connected worker with the right labels can take, and a run that loses its
// worker is picked up by another.
//
// Three generic tasks, rather than one registered workflow per repository,
// because the shape of a run is only known once its script has been evaluated:
//
//	ci-step      runs one step on the worker that took it
//	ci-pipeline  walks a basic plan, starting a ci-step child per step
//	ci-script    evaluates an advanced script, starting a ci-step child each
//	             time the script runs a step
package hatchet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	"github.com/hatchet-dev/hatchet/pkg/client/types"
	sdk "github.com/hatchet-dev/hatchet/sdks/go"

	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

const (
	StepTask     = "ci-step"
	PipelineTask = "ci-pipeline"
	ScriptTask   = "ci-script"
)

type StepInput struct {
	CloneURL string    `json:"clone_url"`
	SHA      string    `json:"sha"`
	Step     plan.Step `json:"step"`
}

type PipelineInput struct {
	Plan     plan.Plan `json:"plan"`
	CloneURL string    `json:"clone_url"`
	// Supersedes names what this run replaces: a new run with the same value
	// cancels any still in progress. A branch, so that a push makes the run
	// for the previous push pointless; or the commit, for a one-off run that
	// should not disturb anything.
	Supersedes string `json:"supersedes"`
}

// ScriptInput is an advanced script and what it is being run for. The script
// travels as source: it is evaluated on a worker, as the run unfolds.
type ScriptInput struct {
	Filename   string       `json:"filename"`
	Source     string       `json:"source"`
	Context    plan.Context `json:"context"`
	CloneURL   string       `json:"clone_url"`
	Supersedes string       `json:"supersedes"`
}

// RunResult is how a run ended, in either mode.
type RunResult struct {
	OK    bool          `json:"ok"`
	Steps []plan.Result `json:"steps"`
	// Error is why an advanced script ended early: a failed step it did not
	// allow for, its own fail(), or a mistake in the script.
	Error string `json:"error,omitempty"`
}

// Tasks is the set of tasks a worker registers and a client triggers.
type Tasks struct {
	Step, Pipeline, Script *sdk.StandaloneTask
}

func (t Tasks) All() []sdk.WorkflowBase {
	return []sdk.WorkflowBase{t.Step, t.Pipeline, t.Script}
}

var (
	one              int32 = 1
	cancelInProgress       = types.CancelInProgress
	notEqual               = types.WorkerLabelComparator_NOT_EQUAL
)

// NewTasks defines the tasks on the client. Register them on a worker to
// execute runs, or use them from any process to trigger and await runs. cfg
// matters only to a worker.
func NewTasks(client *sdk.Client, cfg step.Config) Tasks {
	// A newer run for the same branch makes the one before it pointless.
	supersede := sdk.WithConcurrency(&sdk.Concurrency{
		Expression:    "input.supersedes",
		MaxRuns:       &one,
		LimitStrategy: &cancelInProgress,
	})

	stepTask := client.NewStandaloneTask(StepTask,
		func(ctx sdk.Context, in StepInput) (plan.Result, error) {
			r := step.Run(ctx, cfg, in.CloneURL, in.SHA, in.Step, func(line string) {
				// Hatchet rejects an empty log message; a blank line still
				// matters for reading test output, so send it as a space.
				if line == "" {
					line = " "
				}
				ctx.Log(line)
			})
			if r.Status != "passed" {
				// An error is what marks the task failed in Hatchet, so a
				// broken step is red in the dashboard instead of a green task
				// carrying a bad result. It would fail the same way again on
				// the same inputs, so it is never retried.
				return plan.Result{}, sdk.NewNonRetryableError(errors.New(stepFailed + r.Detail))
			}
			return r, nil
		},
		sdk.WithExecutionTimeout(30*time.Minute),
	)

	pipeline := client.NewStandaloneDurableTask(PipelineTask,
		func(ctx sdk.DurableContext, in PipelineInput) (RunResult, error) {
			results, err := plan.RunPlan(&in.Plan, children(ctx, client, stepTask, in.CloneURL, in.Plan.SHA))
			if ctx.Err() != nil {
				return RunResult{}, sdk.NewNonRetryableError(errors.New("cancelled"))
			}
			if err != nil {
				return RunResult{}, sdk.NewNonRetryableError(err)
			}
			return RunResult{OK: plan.Passed(results), Steps: results}, nil
		},
		sdk.WithExecutionTimeout(time.Hour),
		supersede,
	)

	// The script is not checkpointed; its children are. If this task is
	// restarted partway, the script is evaluated again from the top, and each
	// batch of children it asks for is answered from Hatchet's record of the
	// run instead of being started a second time. That only works because the
	// script asks for the same children in the same order when given the same
	// answers, which a Starlark script cannot fail to do.
	script := client.NewStandaloneDurableTask(ScriptTask,
		func(ctx sdk.DurableContext, in ScriptInput) (RunResult, error) {
			runner := children(ctx, client, stepTask, in.CloneURL, in.Context.SHA)
			results, err := plan.RunPipeline(ctx, in.Filename, []byte(in.Source), in.Context, runner)
			if ctx.Err() != nil {
				return RunResult{}, sdk.NewNonRetryableError(errors.New("cancelled"))
			}
			out := RunResult{OK: err == nil, Steps: results}
			if err != nil {
				out.Error = err.Error()
			}
			return out, nil
		},
		sdk.WithExecutionTimeout(time.Hour),
		supersede,
	)
	return Tasks{Step: stepTask, Pipeline: pipeline, Script: script}
}

// childRunner runs steps as ci-step children of the task that asked for them.
type childRunner struct {
	ctx      sdk.DurableContext
	client   *sdk.Client
	step     *sdk.StandaloneTask
	cloneURL string
	sha      string
}

func children(ctx sdk.DurableContext, client *sdk.Client, stepTask *sdk.StandaloneTask, cloneURL, sha string) plan.Runner {
	return &childRunner{ctx: ctx, client: client, step: stepTask, cloneURL: cloneURL, sha: sha}
}

func (c *childRunner) Run(steps []plan.Step) ([]plan.Result, error) {
	single := plan.Plan{SHA: c.sha}
	inputs := make([]sdk.RunManyOpt, len(steps))
	for i, s := range steps {
		// Steps of a basic plan were validated as a whole; these may come
		// one at a time from a script, so each is checked on its own. Its
		// needs are the caller's business, not the engine's.
		s.Needs = nil
		single.Steps = []plan.Step{s}
		if err := single.Validate(); err != nil {
			return nil, err
		}
		inputs[i] = sdk.RunManyOpt{Input: StepInput{CloneURL: c.cloneURL, SHA: c.sha, Step: steps[i]}}
		if labels := placement(s); len(labels) > 0 {
			inputs[i].Opts = append(inputs[i].Opts, sdk.WithDesiredWorkerLabels(labels))
		}
	}
	refs, err := c.step.RunMany(c.ctx, inputs)
	if err != nil {
		return nil, fmt.Errorf("starting steps: %w", err)
	}
	settled := make(chan struct{})
	go cancelWithParent(c.ctx, c.client, refs, settled)
	defer close(settled)

	out := make([]plan.Result, len(steps))
	for i, s := range steps {
		out[i] = await(&refs[i], s.Name)
	}
	return out, nil
}

// cancelWithParent cancels the steps a run started if the run itself is
// cancelled before they settle. Hatchet stops the parent but leaves its
// children running, and a superseded run's steps are exactly the work that
// should stop.
func cancelWithParent(ctx sdk.DurableContext, client *sdk.Client, refs []sdk.WorkflowRunRef, settled <-chan struct{}) {
	select {
	case <-settled:
		return
	case <-ctx.Done():
	}
	var ids []uuid.UUID
	for _, ref := range refs {
		if id, err := uuid.Parse(ref.RunId); err == nil {
			ids = append(ids, id)
		}
	}
	// The parent's own context is finished; this needs one that is not.
	bg, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	_, _ = client.Runs().Cancel(bg, rest.V1CancelTaskRequest{ExternalIds: &ids})
}

// placement turns a step's worker, prefer and avoid into Hatchet's rules.
func placement(s plan.Step) map[string]*sdk.DesiredWorkerLabel {
	labels := map[string]*sdk.DesiredWorkerLabel{}
	for k, v := range s.Worker {
		labels[k] = &sdk.DesiredWorkerLabel{Value: v, Required: true}
	}
	for k, v := range s.Prefer {
		labels[k] = &sdk.DesiredWorkerLabel{Value: v, Weight: 10}
	}
	for k, v := range s.Avoid {
		labels[k] = &sdk.DesiredWorkerLabel{Value: v, Required: true, Comparator: &notEqual}
	}
	return labels
}

func await(ref *sdk.WorkflowRunRef, name string) plan.Result {
	res, err := ref.Result()
	if err != nil {
		return plan.Result{Name: name, Status: "failed", Detail: reason(err)}
	}
	var out plan.Result
	if err := res.TaskOutput(StepTask).Into(&out); err != nil {
		return plan.Result{Name: name, Status: "failed", Detail: "unreadable result: " + err.Error()}
	}
	return out
}

// stepFailed marks an error as the step's own failure, so that reason can
// recover what the step said from whatever the engine wrapped around it.
const stepFailed = "step failed: "

func reason(err error) string {
	text := err.Error()
	i := strings.LastIndex(text, stepFailed)
	if i < 0 {
		return strings.TrimSpace(text)
	}
	text = text[i+len(stepFailed):]
	// The engine hands a child's error back as the tail of a JSON string, so
	// newlines and quotes arrive escaped and a closing quote trails it.
	if plain, err := strconv.Unquote(`"` + strings.TrimSuffix(text, `"`) + `"`); err == nil {
		text = plain
	}
	return strings.TrimSpace(text)
}
