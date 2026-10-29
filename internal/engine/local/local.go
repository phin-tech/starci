// Package local executes runs on the machine it is called on, with Docker for
// steps in an image: no queue, no other machines, and no record to resume
// from if the process dies. It is how starci runs, on a laptop or in another
// CI tool's job, and the smallest loop over plan.Next there is.
package local

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	"git.phin.tech/sam/starci/internal/tracing"
)

// Engine carries a run out on this machine, start to finish, in the process
// that asked. It can do the one thing every engine must and has none of the
// capabilities: there is nowhere else to place a step and no record to
// resume from.
type Engine struct {
	Config step.Config
	// Slots is how many steps run at once.
	Slots int
	// Log receives each line a step prints, with the step's number and
	// name. Optional.
	Log func(step, line string)
}

var _ engine.Engine = (*Engine)(nil)

func (e *Engine) Name() string { return "local" }

func (e *Engine) Can() engine.Capabilities { return engine.Capabilities{} }

func (e *Engine) Execute(ctx context.Context, r engine.Run) (engine.Result, error) {
	run := tracing.Run{ID: uuid.NewString(), Began: time.Now(), Repo: r.Context.Repo, SHA: r.Context.SHA, Mode: r.Mode}
	next := func(results []plan.Result) (plan.Decision, error) {
		return plan.NextInPipeline(ctx, r.Filename, r.Source, r.Context, results)
	}
	if r.Mode != plan.Advanced {
		if r.Plan == nil {
			return engine.Result{}, errors.New("a basic run needs a plan")
		}
		next = r.Plan.Next
	}
	d, err := e.drive(ctx, r.CloneURL, r.Context.SHA, run.ID, next)
	if err != nil {
		return engine.Result{}, err
	}
	run.OK, run.Error = d.OK, d.Error
	if !d.OK && d.Error == "" {
		run.Error = "a step did not pass"
	}
	tracing.End(ctx, run)
	return engine.Result{OK: d.OK, Steps: d.Results, Error: d.Error}, nil
}

// drive asks next what to run, starts each step it has not started yet, and
// asks again as each one finishes, until the run is over. At most Slots steps
// run at once; the rest wait their turn.
func (e *Engine) drive(ctx context.Context, cloneURL, sha, runID string, next func([]plan.Result) (plan.Decision, error)) (plan.Decision, error) {
	slots := e.Slots
	if slots < 1 {
		slots = 1
	}
	gate := make(chan struct{}, slots)
	finished := make(chan plan.Result)
	var wg sync.WaitGroup
	// Whatever is still running when the run ends is stopped and waited for.
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var results []plan.Result
	started := map[int]bool{}
	running := 0
	for {
		d, err := next(results)
		if err != nil || d.Done {
			return d, err
		}
		for _, asked := range d.Run {
			if started[asked.Number] {
				continue
			}
			started[asked.Number] = true
			running++
			// Asked for now; a step waiting for a slot says so in its trace.
			carrier := tracing.Ask(runID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				gate <- struct{}{}
				defer func() { <-gate }()
				s := asked.Step
				log := func(string) {}
				if e.Log != nil {
					label := fmt.Sprintf("#%d %s", asked.Number, s.Name)
					log = func(line string) { e.Log(label, line) }
				}
				stepCtx, end := carrier.Step(ctx, s.Name)
				result := step.Run(stepCtx, e.Config, cloneURL, sha, s, log)
				end(result)
				select {
				case finished <- asked.Answer(result):
				case <-ctx.Done():
				}
			}()
		}
		if running == 0 {
			return d, errors.New("the run is waiting on steps it has not asked for")
		}
		select {
		case r := <-finished:
			results = append(results, r)
			running--
		case <-ctx.Done():
			sort.Slice(results, func(i, j int) bool { return results[i].Number < results[j].Number })
			return plan.Decision{Done: true, Error: "cancelled", Results: results}, nil
		}
	}
}
