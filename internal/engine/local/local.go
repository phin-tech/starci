// Package local executes runs on the machine it is called on, with nothing
// but Docker: no queue, no other workers, and no record to resume from if the
// process dies. It is for trying a script before pushing it, and it is the
// smallest thing that satisfies plan.Runner.
package local

import (
	"context"
	"errors"
	"sync"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

// Runner runs each batch of steps as concurrent containers on this machine.
type Runner struct {
	Ctx      context.Context
	Config   step.Config
	CloneURL string
	SHA      string
	// Slots is how many steps run at once. A batch larger than this queues.
	Slots int
	// Log receives each line a step prints, with the step's name. Optional.
	Log func(stepName, line string)
}

func (r *Runner) Run(steps []plan.Step) ([]plan.Result, error) {
	slots := r.Slots
	if slots < 1 {
		slots = 1
	}
	gate := make(chan struct{}, slots)
	results := make([]plan.Result, len(steps))

	var wg sync.WaitGroup
	for i, s := range steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			log := func(string) {}
			if r.Log != nil {
				log = func(line string) { r.Log(s.Name, line) }
			}
			results[i] = step.Run(r.Ctx, r.Config, r.CloneURL, r.SHA, s, log)
		}()
	}
	wg.Wait()
	return results, nil
}

// Engine carries a run out on this machine, start to finish, in the process
// that asked. It can do the one thing every engine must and none of the
// optional ones: there is nowhere else to place a step, no other run to
// supersede, and no record to resume from.
type Engine struct {
	Config step.Config
	// Slots is how many steps run at once.
	Slots int
	// Log receives each line a step prints, with the step's name. Optional.
	Log func(stepName, line string)
}

var _ engine.Engine = (*Engine)(nil)

func (e *Engine) Name() string { return "local" }

func (e *Engine) Can() engine.Capabilities { return engine.Capabilities{} }

func (e *Engine) Execute(ctx context.Context, r engine.Run) (engine.Result, error) {
	runner := &Runner{Ctx: ctx, Config: e.Config, CloneURL: r.CloneURL, SHA: r.Context.SHA, Slots: e.Slots, Log: e.Log}
	if r.Mode == plan.Advanced {
		results, err := plan.RunPipeline(ctx, r.Filename, r.Source, r.Context, runner)
		out := engine.Result{OK: err == nil, Steps: results}
		if err != nil {
			out.Error = err.Error()
		}
		return out, nil
	}
	if r.Plan == nil {
		return engine.Result{}, errors.New("a basic run needs a plan")
	}
	results, err := plan.RunPlan(r.Plan, runner)
	if err != nil {
		return engine.Result{}, err
	}
	return engine.Result{OK: plan.Passed(results), Steps: results}, nil
}
