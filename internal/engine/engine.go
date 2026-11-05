// Package engine is what starci asks of the system that carries a run out.
//
// starci decides what a run is: which steps, in what order, each given which
// files. Something else gets those steps onto machines and waits for them.
// That something is an engine. Running everything on the machine at hand is
// the one there is; others (Lambda, Temporal, Kubernetes Jobs) would be
// packages beside it.
//
// An engine has one thing it must do, Execute. What a run needs that an
// engine cannot do is not silently dropped: Can says what it can do, and
// Capabilities.Missing says what a particular run would lose.
package engine

import (
	"context"
	"fmt"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

// Engine carries runs out.
type Engine interface {
	// Name is what the engine is called, for telling a person which is in use.
	Name() string
	// Can says which of the things a run may ask for this engine honours.
	Can() Capabilities
	// Execute carries a run out and returns how it went. An error means the
	// run could not be carried out at all; a run whose steps failed is a
	// Result, not an error.
	Execute(ctx context.Context, r Run) (Result, error)
}

// Pool is an engine whose steps run on machines that join it, rather than in
// the process that asked for the run. Serve makes the machine it is called on
// one of them, taking steps until ctx ends. An engine is asked for it with a
// type assertion; the local engine is not one.
type Pool interface {
	Serve(ctx context.Context, m Machine) error
}

// Machine is what a machine brings to a pool.
type Machine struct {
	// Config is how it runs steps. Its Bare is used for every run unless
	// Bare below says otherwise.
	Config step.Config
	// Bare, if set, decides for each run whether this machine runs its bare
	// steps: a machine serving many repositories cannot decide once for all
	// of them (a pull request from a fork, for one).
	Bare func(plan.Context) bool
	// Slots is how many steps it runs at once.
	Slots int
	// Log receives each line a step prints, with the step's number and
	// name. Optional.
	Log func(step, line string)
}

// Run is one run, as an engine is asked to carry it out.
type Run struct {
	// Mode is plan.Basic or plan.Advanced.
	Mode string
	// Plan is the steps, for a basic run.
	Plan *plan.Plan
	// Filename and Source are the script, for an advanced run: it is
	// evaluated by the engine, as the run unfolds.
	Filename string
	Source   []byte
	// Context is what the script is told about the run.
	Context plan.Context
	// CloneURL is where the commit is fetched from, by whichever machine
	// runs a step.
	CloneURL string
}

// Result is how a run went.
type Result struct {
	OK    bool          `json:"ok"`
	Steps []plan.Result `json:"steps"`
	// Error is why an advanced script ended early: a failed step it did not
	// allow for, its own fail(), or a mistake in the script.
	Error string `json:"error,omitempty"`
}

// Capabilities are the things a run may ask of an engine that not every
// engine can do.
type Capabilities struct {
	// Durable: a run survives the loss of the machine carrying it out. An
	// advanced script picks up where it was, without repeating finished
	// steps.
	Durable bool
	// Placement: a step's on, prefer and avoid decide where it runs. Without
	// it, os and arch are still checked by the machine the step lands on.
	Placement bool
}

// Missing lists, in plain sentences, what r asks for that these capabilities
// do not include. It is for telling a person before a run starts, so that
// nothing is quietly not done.
func (c Capabilities) Missing(r Run) []string {
	var missing []string
	if !c.Placement && r.Plan != nil {
		var placed []string
		for _, s := range r.Plan.Steps {
			if labelled(s) {
				placed = append(placed, s.Name)
			}
		}
		if len(placed) > 0 {
			missing = append(missing, fmt.Sprintf("on, prefer and avoid are ignored but for os and arch: there is only one place to run (%s)", strings.Join(placed, ", ")))
		}
	}
	if !c.Durable && r.Mode == plan.Advanced {
		missing = append(missing, "if this is interrupted, the script cannot pick up where it was")
	}
	return missing
}

// labelled reports whether a step places itself by anything but the
// well-known keys, which the machine a step lands on checks for itself.
func labelled(s plan.Step) bool {
	if len(s.Prefer) > 0 {
		return true
	}
	for _, m := range []map[string]string{s.On, s.Avoid} {
		for k := range m {
			if k != plan.PlaceOS && k != plan.PlaceArch {
				return true
			}
		}
	}
	return false
}
