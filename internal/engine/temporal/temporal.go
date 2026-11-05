//go:build temporal

// Package temporal carries runs out on Temporal: a run is a workflow, and
// machines anywhere that join the pool (Serve) run its steps.
//
// The workflow is a loop over plan.Next. Asking what is next is an activity,
// so Starlark never runs under the workflow's own rules, and each answer and
// each step's result is kept in the workflow's history. Each step is an
// activity that runs step.Run on whichever machine takes it.
//
// What it adds to the local engine: steps run on other machines, a pool of
// them shared by every repository that starts runs on it, with dispatch
// shared fairly between repositories (Temporal's fairness keys, which the
// server has to have turned on).
//
// Durability: a run's answers and results are in its workflow's history, so
// a run whose machines are lost carries on when another takes its tasks.
// Finished steps are not run again; the step a lost machine was running is
// run again on another, after the heartbeat timeout.
//
// Placement: a step goes to the task queue named by its on, and a machine
// serves the queue of every set of its labels, its os and arch among them,
// so a step is taken only by a machine that has all it asks for. prefer and
// avoid have no equivalent and are not claimed.
//
// Artifacts: a machine with no shared store refuses a step that keeps or is
// handed one, since another machine could not reach it.
//
// It is compiled only with -tags temporal
// (docs/adr/0004-an-engine-sdk-behind-a-build-tag.md).
package temporal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	"git.phin.tech/sam/starci/internal/tracing"
)

// Names the workflow and activities are registered under. Changing one
// strands every run in flight.
const (
	workflowName = "starci.Run"
	nextName     = "starci.Next"
	stepName     = "starci.Step"
)

// Tuning. Nothing in a script says how long a step may take; these are
// starci's own choices, as in the compiled Temporal workflow.
const (
	// stepTimeout is how long one attempt at a step may take.
	stepTimeout = time.Hour
	// heartbeatTimeout is how long a machine may be silent before its step
	// is given to another. A running step heartbeats on its own.
	heartbeatTimeout = 30 * time.Second
	// attempts is how many times a step that could not be run at all (the
	// machine lost) is tried. A step that ran and failed is a result, and is
	// never run again.
	attempts = 3
	// waitTimeout is how long a step may wait for a machine that has what
	// its on asks for, before the run is told none came.
	waitTimeout = time.Hour
	// maxLabels bounds a machine's labels, os and arch included: it serves
	// a queue for every set of them.
	maxLabels = 6
)

// DefaultQueue is the task queue runs and steps go to when none is named.
const DefaultQueue = "starci"

// Engine starts runs on a Temporal server, and makes machines of the
// computers that call Serve.
type Engine struct {
	Client client.Client
	// Queue is the task queue for runs and their steps. Default DefaultQueue.
	Queue string
	// ID names a run's workflow. Runs with one ID are one run: starting
	// again while it is going attaches to it. Default: a new id each time.
	ID func(engine.Run) string
}

var (
	_ engine.Engine = (*Engine)(nil)
	_ engine.Pool   = (*Engine)(nil)
)

func (e *Engine) Name() string { return "temporal" }

// Can claims durability, shown by killing a machine partway through a step
// of an advanced script (TestKilledMachine), and placement by on. prefer and
// avoid have no equivalent.
func (e *Engine) Can() engine.Capabilities {
	return engine.Capabilities{Durable: true, Placement: true}
}

func (e *Engine) queue() string {
	if e.Queue == "" {
		return DefaultQueue
	}
	return e.Queue
}

// stepQueue is the task queue for a step that asks for on: the run's own
// queue if it asks for nothing, else the run's queue and the labels, sorted.
// The compiled Temporal workflow names them the same way.
func stepQueue(base string, on map[string]string) string {
	if len(on) == 0 {
		return base
	}
	keys := make([]string, 0, len(on))
	for k := range on {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + on[k]
	}
	return base + "/" + strings.Join(parts, ",")
}

// nextQueue is where asking what is next goes, apart from the steps, so that
// a machine whose slots are all running steps still answers it.
func nextQueue(base string) string { return base + "/next" }

// servedQueues are the queues a machine with these labels serves: one for
// every set of them, the empty set (the run's own queue) included.
func servedQueues(base string, labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var queues []string
	for mask := 0; mask < 1<<len(keys); mask++ {
		on := map[string]string{}
		for i, k := range keys {
			if mask&(1<<i) != 0 {
				on[k] = labels[k]
			}
		}
		queues = append(queues, stepQueue(base, on))
	}
	return queues
}

// runInput is what a run's workflow is started with, and all it knows. A
// script cannot load other files, so its source is the whole of it.
type runInput struct {
	// ID is the run's, for its trace: the same on every evaluation.
	ID       string       `json:"id"`
	Mode     string       `json:"mode"`
	Plan     *plan.Plan   `json:"plan,omitempty"`
	Filename string       `json:"filename,omitempty"`
	Source   []byte       `json:"source,omitempty"`
	Context  plan.Context `json:"context"`
	CloneURL string       `json:"clone_url"`
}

// nextInput is what the Next activity is asked with.
type nextInput struct {
	Run     runInput      `json:"run"`
	Results []plan.Result `json:"results,omitempty"`
}

// stepJob is what the Step activity is handed: one step, and what a machine
// needs to run it.
type stepJob struct {
	Asked    plan.Asked      `json:"asked"`
	Context  plan.Context    `json:"context"`
	CloneURL string          `json:"clone_url"`
	Trace    tracing.Carrier `json:"trace"`
}

func (e *Engine) Execute(ctx context.Context, r engine.Run) (engine.Result, error) {
	if r.Mode != plan.Advanced && r.Plan == nil {
		return engine.Result{}, errors.New("a basic run needs a plan")
	}
	id := "starci-" + uuid.NewString()
	if e.ID != nil {
		id = e.ID(r)
	}
	in := runInput{ID: id, Mode: r.Mode, Plan: r.Plan, Filename: r.Filename, Source: r.Source, Context: r.Context, CloneURL: r.CloneURL}
	traced := tracing.Run{ID: id, Began: time.Now(), Repo: r.Context.Repo, SHA: r.Context.SHA, Mode: r.Mode}

	wr, err := e.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        id,
		TaskQueue: e.queue(),
		// A run that is already going is attached to, not started again.
		WorkflowExecutionErrorWhenAlreadyStarted: false,
	}, workflowName, in)
	if err != nil {
		return engine.Result{}, fmt.Errorf("starting the run on Temporal: %w", err)
	}

	var result engine.Result
	done := make(chan error, 1)
	go func() { done <- wr.Get(context.Background(), &result) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		// Stopping the run stops its steps; wait for them to be cleared
		// away, as the local engine does.
		if cerr := e.Client.CancelWorkflow(context.Background(), wr.GetID(), wr.GetRunID()); cerr != nil {
			return engine.Result{}, fmt.Errorf("cancelling the run on Temporal: %w", cerr)
		}
		err = <-done
	}
	if err != nil {
		return engine.Result{}, fmt.Errorf("the run on Temporal (workflow %s): %w", wr.GetID(), err)
	}
	traced.OK, traced.Error = result.OK, result.Error
	if !result.OK && result.Error == "" {
		traced.Error = "a step did not pass"
	}
	tracing.End(ctx, traced)
	return result, nil
}

// Serve makes this computer one of the pool's machines: it takes runs, and
// the steps whose on its labels satisfy, until ctx ends.
func (e *Engine) Serve(ctx context.Context, m engine.Machine) error {
	labels := map[string]string{plan.PlaceOS: runtime.GOOS, plan.PlaceArch: runtime.GOARCH}
	for k, v := range m.Labels {
		labels[k] = v
	}
	if len(labels) > maxLabels {
		return fmt.Errorf("a machine may have at most %d labels, os and arch among them; this one has %d", maxLabels, len(labels))
	}
	// A machine killed partway through a step left that step's services
	// running. They are this machine's, by its name.
	step.Sweep(m.Config.Machine)

	slots := m.Slots
	if slots < 1 {
		slots = 1
	}
	// The machine's limit on steps is a gate in the step activity, not
	// Temporal's slots: those are reserved before each poll, so slots
	// shared between this machine's queues would leave some queues unpolled
	// until another's long poll ended. A step waiting at the gate holds its
	// task, heartbeating, so a machine can hold at most a queue's worth of
	// slots for each queue it serves.
	a := &activities{m: m, gate: make(chan struct{}, slots)}
	var workers []worker.Worker
	start := func(queue string, register func(worker.Worker)) error {
		w := worker.New(e.Client, queue, worker.Options{MaxConcurrentActivityExecutionSize: slots})
		register(w)
		if err := w.Start(); err != nil {
			return fmt.Errorf("joining the pool on queue %s: %w", queue, err)
		}
		workers = append(workers, w)
		return nil
	}
	stopAll := func() {
		for _, w := range workers {
			w.Stop()
		}
	}
	base := e.queue()
	// Runs, and asking what is next, on queues of their own.
	if err := start(nextQueue(base), func(w worker.Worker) {
		w.RegisterActivityWithOptions(a.Next, activity.RegisterOptions{Name: nextName})
	}); err != nil {
		stopAll()
		return err
	}
	for _, q := range servedQueues(base, labels) {
		q := q
		err := start(q, func(w worker.Worker) {
			if q == base {
				w.RegisterWorkflowWithOptions(runWorkflow, workflow.RegisterOptions{Name: workflowName})
			}
			w.RegisterActivityWithOptions(a.Step, activity.RegisterOptions{Name: stepName})
		})
		if err != nil {
			stopAll()
			return err
		}
	}
	<-ctx.Done()
	stopAll()
	return nil
}

// runWorkflow is a run: ask what is next, run the steps it names that are
// not running yet, and ask again as each one finishes, until the run is over.
// It reads no clock and makes no ids of its own, so that it does the same
// thing each time Temporal replays it.
func runWorkflow(ctx workflow.Context, in runInput) (engine.Result, error) {
	base := workflow.GetInfo(ctx).TaskQueueName
	nextCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           nextQueue(base),
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: attempts},
	})
	stepOptions := workflow.ActivityOptions{
		ScheduleToStartTimeout: waitTimeout,
		StartToCloseTimeout:    stepTimeout,
		HeartbeatTimeout:       heartbeatTimeout,
		// A cancelled step is waited for, so that its containers are gone
		// before the run says it is over.
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: attempts},
		// Repositories sharing a pool take turns on it, where the server
		// has fairness turned on. Ignored where it does not.
		Priority: temporal.Priority{FairnessKey: fairnessKey(in.Context.Repo)},
	}

	var results []plan.Result
	started := map[int]bool{}
	running := map[int]workflow.Future{}
	cancelled := false
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) { cancelled = true })

	for {
		var d plan.Decision
		if err := workflow.ExecuteActivity(nextCtx, nextName, nextInput{Run: in, Results: results}).Get(ctx, &d); err != nil {
			if cancelled || ctx.Err() != nil {
				return stopped(ctx, results, running), nil
			}
			return engine.Result{}, fmt.Errorf("asking what is next: %w", err)
		}
		if d.Done {
			return engine.Result{OK: d.OK, Steps: d.Results, Error: d.Error}, nil
		}
		for _, asked := range d.Run {
			if started[asked.Number] {
				continue
			}
			started[asked.Number] = true
			carrier := tracing.Ask(in.ID)
			// The time a step was asked for is the workflow's, not the
			// clock's, so that every replay hands out the same job.
			carrier.Asked = workflow.Now(ctx)
			opts := stepOptions
			opts.TaskQueue = stepQueue(base, asked.Step.On)
			stepCtx := workflow.WithActivityOptions(ctx, opts)
			f := workflow.ExecuteActivity(stepCtx, stepName, stepJob{Asked: asked, Context: in.Context, CloneURL: in.CloneURL, Trace: carrier})
			running[asked.Number] = f
			sel.AddFuture(f, func(f workflow.Future) {
				delete(running, asked.Number)
				var r plan.Result
				if err := f.Get(ctx, &r); err != nil {
					if cancelled {
						return
					}
					r = asked.Answer(plan.Result{Status: "failed", Detail: notRun(asked.Step, err)})
				}
				results = append(results, r)
			})
		}
		if len(running) == 0 {
			return engine.Result{}, errors.New("the run is waiting on steps it has not asked for")
		}
		sel.Select(ctx)
		if cancelled {
			return stopped(ctx, results, running), nil
		}
	}
}

// notRun says why a step could not be run at all, on any machine, in as many
// attempts as it was given.
func notRun(s plan.Step, err error) string {
	var timeout *temporal.TimeoutError
	if errors.As(err, &timeout) && timeout.TimeoutType() == enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START {
		return fmt.Sprintf("could not be run: no machine with %s took it within %s", describeOn(s.On), waitTimeout)
	}
	return "could not be run: " + err.Error()
}

func describeOn(on map[string]string) string {
	if len(on) == 0 {
		return "nothing in particular asked of it"
	}
	keys := make([]string, 0, len(on))
	for k := range on {
		keys = append(keys, k+" = "+on[k])
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// stopped ends a run that was cancelled: its steps are waited for until they
// have cleared away what they started, and what had finished is reported.
func stopped(ctx workflow.Context, results []plan.Result, running map[int]workflow.Future) engine.Result {
	numbers := make([]int, 0, len(running))
	for n := range running {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers) // map order is not the same on every replay
	for _, n := range numbers {
		_ = running[n].Get(ctx, nil)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Number < results[j].Number })
	return engine.Result{Error: "cancelled", Steps: results}
}

// fairnessKey stands for a repository in Temporal's fairness keys, which are
// at most 64 bytes.
func fairnessKey(repo string) string {
	if len(repo) <= 64 {
		return repo
	}
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:16])
}

// activities are what a machine does for the pool.
type activities struct {
	m engine.Machine
	// gate holds a place for each step running on this machine.
	gate chan struct{}
}

// Next decides what a run does next. It evaluates the script on the machine
// that takes it, from the run's input and the results so far, and keeps
// nothing.
func (a *activities) Next(ctx context.Context, in nextInput) (plan.Decision, error) {
	r := in.Run
	if r.Mode == plan.Advanced {
		return plan.NextInPipeline(ctx, r.Filename, r.Source, r.Context, in.Results)
	}
	if r.Plan == nil {
		return plan.Decision{}, temporal.NewNonRetryableApplicationError("a basic run needs a plan", "starci", nil)
	}
	return r.Plan.Next(in.Results)
}

// Step runs one step on this machine.
func (a *activities) Step(ctx context.Context, job stepJob) (plan.Result, error) {
	stop := heartbeat(ctx)
	defer stop()
	if a.gate != nil {
		select {
		case a.gate <- struct{}{}:
			defer func() { <-a.gate }()
		case <-ctx.Done():
			return plan.Result{}, ctx.Err()
		}
	}

	cfg := a.m.Config
	if a.m.Bare != nil {
		cfg.Bare = a.m.Bare(job.Context)
	}
	s := job.Asked.Step
	if why := unshared(cfg, s); why != "" {
		return job.Asked.Answer(plan.Result{Status: "failed", Machine: cfg.Machine, Detail: why}), nil
	}
	log := func(string) {}
	if a.m.Log != nil {
		label := fmt.Sprintf("#%d %s", job.Asked.Number, s.Name)
		log = func(line string) { a.m.Log(label, line) }
	}
	stepCtx, end := job.Trace.Step(ctx, s.Name)
	result := step.Run(stepCtx, cfg, job.CloneURL, job.Context.SHA, s, log)
	end(result)
	if err := ctx.Err(); err != nil {
		// Cancelled, or this attempt ran out of time: not a result.
		return plan.Result{}, err
	}
	return job.Asked.Answer(result), nil
}

// unshared says why this machine refuses s, or "": a step that keeps
// something, or is handed something another kept, needs a store every
// machine reaches.
func unshared(cfg step.Config, s plan.Step) string {
	if len(s.Keep) == 0 && len(s.Artifacts) == 0 {
		return ""
	}
	if _, ok := cfg.Shared.(step.Blobs); ok {
		return ""
	}
	return "this machine has no store shared with the rest of the pool (STARCI_STORE), so what this step keeps or is handed could not travel between machines"
}

// heartbeat tells Temporal this machine is alive while a step runs, which is
// also how it hears that the step is to be cancelled.
func heartbeat(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeatTimeout / 6)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}
