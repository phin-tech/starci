// Package local executes runs on the machine it is called on, with nothing
// but Docker: no queue, no other workers, and no record to resume from if the
// process dies. It is for trying a script before pushing it, and it is the
// smallest thing that satisfies plan.Runner.
package local

import (
	"context"
	"sync"

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
