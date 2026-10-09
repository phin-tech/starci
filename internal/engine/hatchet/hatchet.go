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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	"github.com/hatchet-dev/hatchet/pkg/client/types"
	sdk "github.com/hatchet-dev/hatchet/sdks/go"

	"git.phin.tech/sam/starci/internal/forge"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	"git.phin.tech/sam/starci/internal/trigger"
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
	// Exclusive names runs of which only one may be in progress, a newer
	// one giving way: how a schedule set to skip does not overlap itself.
	// Always set; a run that is not exclusive with anything is given a
	// value of its own.
	Exclusive string `json:"exclusive"`
	// Report, if set, is where the outcome is posted when the run ends.
	Report *trigger.Report `json:"report,omitempty"`
}

// ScriptInput is an advanced script and what it is being run for. The script
// travels as source: it is evaluated on a worker, as the run unfolds.
type ScriptInput struct {
	Filename   string          `json:"filename"`
	Source     string          `json:"source"`
	Context    plan.Context    `json:"context"`
	CloneURL   string          `json:"clone_url"`
	Supersedes string          `json:"supersedes"`
	Exclusive  string          `json:"exclusive"`
	Report     *trigger.Report `json:"report,omitempty"`
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
	// receiving are the tasks that act on deliveries from a forge. A worker
	// has them only if it was given the forge's secret, so a delivery is
	// never handed to a worker that could not check it.
	receiving []*sdk.StandaloneTask
	// timers is set on a worker that fires schedules.
	timers *crons
}

func (t Tasks) All() []sdk.WorkflowBase {
	all := []sdk.WorkflowBase{t.Step, t.Pipeline, t.Script}
	for _, r := range t.receiving {
		all = append(all, r)
	}
	return all
}

// Reception is what a worker needs in order to act on deliveries.
type Reception struct {
	Hooks      map[string]trigger.Hook
	AllowForks bool
}

// HookEvent is the event an engine-side webhook for a forge raises. It
// carries a delivery whose signature Hatchet has already checked.
func HookEvent(forgeName string) string { return "starci:hook:" + forgeName }

// DeliveryResult is what a delivery led to, in a sentence.
type DeliveryResult struct {
	Said string `json:"said"`
}

var (
	one              int32 = 1
	cancelInProgress       = types.CancelInProgress
	cancelNewest           = types.CancelNewest
	notEqual               = types.WorkerLabelComparator_NOT_EQUAL
)

// Reporter posts a run's outcome to the forge it came from. A worker that
// holds no credentials for that forge has none to give, and says so.
type Reporter func(ctx context.Context, to trigger.Report, s forge.Status) error

// report posts the outcome of a run that asked for one. It is called from
// inside the run's own task rather than by whoever submitted it, so that the
// outcome is posted even if the submitter is long gone, and is posted again
// if the task is.
func report(ctx sdk.DurableContext, post Reporter, to *trigger.Report, s forge.Status) {
	if to == nil {
		return
	}
	if post == nil {
		ctx.Log("this worker has no forge credentials; the outcome was not posted to " + to.Repo)
		return
	}
	// The run's context may already be cancelled, which is exactly when a
	// status still needs to be posted.
	bg, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if err := post(bg, *to, s); err != nil {
		ctx.Log("could not post the outcome: " + err.Error())
	}
}

var superseded = forge.Status{State: "error", Description: "superseded by a newer run"}

// NewTasks defines the tasks on the client. Register them on a worker to
// execute runs, or use them from any process to trigger and await runs. cfg,
// post and reception matter only to a worker; reception may be nil.
func NewTasks(client *sdk.Client, cfg step.Config, post Reporter, reception *Reception) Tasks {
	// A newer run for the same branch makes the one before it pointless.
	// And of runs that are exclusive with one another, the one in progress
	// stays and a newer one is turned away.
	supersede := sdk.WithConcurrency(
		&sdk.Concurrency{Expression: "input.supersedes", MaxRuns: &one, LimitStrategy: &cancelInProgress},
		&sdk.Concurrency{Expression: "input.exclusive", MaxRuns: &one, LimitStrategy: &cancelNewest},
	)

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
				report(ctx, post, in.Report, superseded)
				return RunResult{}, sdk.NewNonRetryableError(errors.New("cancelled"))
			}
			if err != nil {
				report(ctx, post, in.Report, forge.Status{State: "error", Description: err.Error()})
				return RunResult{}, sdk.NewNonRetryableError(err)
			}
			report(ctx, post, in.Report, trigger.Outcome(results, ""))
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
				report(ctx, post, in.Report, superseded)
				return RunResult{}, sdk.NewNonRetryableError(errors.New("cancelled"))
			}
			out := RunResult{OK: err == nil, Steps: results}
			if err != nil {
				out.Error = err.Error()
			}
			report(ctx, post, in.Report, trigger.Outcome(results, out.Error))
			return out, nil
		},
		sdk.WithExecutionTimeout(time.Hour),
		supersede,
	)
	tasks := Tasks{Step: stepTask, Pipeline: pipeline, Script: script}
	if reception == nil || len(reception.Hooks) == 0 {
		return tasks
	}

	receiver := &trigger.Receiver{Hooks: reception.Hooks, AllowForks: reception.AllowForks}
	// A delivery leads to a run by the same door any other run comes in by.
	receiver.Submit = submitter(func() Tasks { return tasks })
	// Schedules a script declares become Hatchet's crons, and each cron
	// starts the task that works out what the schedule wants done now.
	timers := &crons{client: client}
	receiver.Schedules = timers
	tasks.receiving = append(tasks.receiving, scheduledTask(client, receiver, timers, post, cfg.StateDir))
	tasks.timers = timers
	acted := func(said string, err error) (DeliveryResult, error) {
		if errors.Is(err, forge.ErrSignature) {
			// It will be no more genuine the second time.
			return DeliveryResult{}, sdk.NewNonRetryableError(err)
		}
		return DeliveryResult{Said: said}, err
	}

	// Raised by any door: `starci serve`, a function on a cloud provider.
	// The signature is checked here, by the worker, with the secret it holds.
	tasks.receiving = append(tasks.receiving, client.NewStandaloneTask("ci-delivery",
		func(ctx sdk.Context, in trigger.Delivery) (DeliveryResult, error) {
			receiver.Log = func(format string, args ...any) { ctx.Log(fmt.Sprintf(format, args...)) }
			return acted(receiver.Deliver(ctx, in))
		},
		sdk.WithWorkflowEvents(trigger.DeliveryEvent),
		sdk.WithRetries(3),
	))
	// Raised by Hatchet's own webhook endpoint, which has checked the
	// signature and hands over the body alone.
	for name := range reception.Hooks {
		tasks.receiving = append(tasks.receiving, client.NewStandaloneTask("ci-hook-"+name,
			func(ctx sdk.Context, in map[string]any) (DeliveryResult, error) {
				body, err := json.Marshal(in)
				if err != nil {
					return DeliveryResult{}, sdk.NewNonRetryableError(err)
				}
				receiver.Log = func(format string, args ...any) { ctx.Log(fmt.Sprintf(format, args...)) }
				return acted(receiver.DeliverVerified(ctx, name, body))
			},
			sdk.WithWorkflowEvents(HookEvent(name)),
			sdk.WithRetries(3),
		))
	}
	return tasks
}

// submitter adapts a function to trigger.Submitter. It takes the tasks late,
// because the tasks that receive deliveries are among them.
type submitter func() Tasks

func (s submitter) Submit(ctx context.Context, r trigger.Run) error { return s().Submit(ctx, r) }

// Submit starts a run and returns as soon as Hatchet has it. It is what a
// webhook receiver calls: the run reports its own outcome.
func (t Tasks) Submit(ctx context.Context, r trigger.Run) error {
	var err error
	if r.Mode == plan.Advanced {
		_, err = t.Script.RunNoWait(ctx, ScriptInput{
			Filename: r.Filename, Source: string(r.Source), Context: r.Context,
			CloneURL: r.CloneURL, Supersedes: r.Supersedes, Exclusive: OrUnique(r.Exclusive), Report: &r.Report,
		})
	} else {
		_, err = t.Pipeline.RunNoWait(ctx, PipelineInput{
			Plan: *r.Plan, CloneURL: r.CloneURL, Supersedes: r.Supersedes, Exclusive: OrUnique(r.Exclusive), Report: &r.Report,
		})
	}
	return err
}

// OrUnique returns name, or if it is empty a value nothing else has. Runs
// are grouped by these names, and an empty one would put every run that has
// none into one group.
func OrUnique(name string) string {
	if name == "" {
		return "only-" + uuid.NewString()
	}
	return name
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
