// Package engine is what starci asks of the system that carries a run out.
//
// starci decides what a run is: which steps, in what order, each given which
// files. Something else has to get those steps onto machines, wait for them,
// and survive a machine going away. That something is an engine. Hatchet is
// one; running everything on the machine at hand is another.
//
// An engine has one thing it must do, Execute, and several it may do. Each
// optional ability is its own small interface, and a caller asks for it with
// a type assertion:
//
//	if fleet, ok := eng.(engine.Fleet); ok { ... }
//
// What a run needs that an engine cannot do is not silently dropped: Can
// says what it can do, and Capabilities.Missing says what a particular run
// would lose.
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
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

	// Supersedes names what this run replaces: a newer run with the same
	// name cancels one still in progress. Empty means it replaces nothing.
	Supersedes string
	// Exclusive names runs of which there is to be only one at a time, a
	// newer one giving way to the one in progress. It is the reverse of
	// Supersedes. Empty means it is exclusive with nothing.
	Exclusive string

	// Report, if set, is where the outcome is posted when the run ends, by
	// whatever carried it out.
	Report *Report

	// Broken is why the script could not be evaluated, for a run that was
	// asked for anyway. There is then nothing to execute, only this to say.
	Broken string
}

// Report says where a run's outcome is to be posted.
type Report struct {
	Forge string `json:"forge"`
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	// Context is the name the status appears under on the commit.
	Context string `json:"context,omitempty"`
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
	// Placement: a step's worker, prefer and avoid decide where it runs.
	Placement bool
	// Supersedes and Exclusive: Run.Supersedes and Run.Exclusive are honoured.
	Supersedes bool
	Exclusive  bool
}

// Missing lists, in plain sentences, what r asks for that these capabilities
// do not include. It is for telling a person before a run starts, so that
// nothing is quietly not done.
func (c Capabilities) Missing(r Run) []string {
	var missing []string
	if !c.Placement && r.Plan != nil {
		var placed []string
		for _, s := range r.Plan.Steps {
			if len(s.Worker) > 0 || len(s.Prefer) > 0 || len(s.Avoid) > 0 {
				placed = append(placed, s.Name)
			}
		}
		if len(placed) > 0 {
			missing = append(missing, fmt.Sprintf("worker, prefer and avoid are ignored: there is only one place to run (%s)", strings.Join(placed, ", ")))
		}
	}
	if !c.Durable && r.Mode == plan.Advanced {
		missing = append(missing, "if this is interrupted, the script cannot pick up where it was")
	}
	return missing
}

// The optional abilities.

// Starter is an engine that can be handed a run and left with it. The run
// posts its own outcome, to Run.Report.
type Starter interface {
	Start(ctx context.Context, r Run) error
}

// Scheduler is an engine that can fire schedules.
type Scheduler interface {
	// Sync makes the schedules that fire for a repository exactly these:
	// adding, changing and removing as needed.
	Sync(ctx context.Context, forgeName, repo string, schedules []plan.Schedule) error
}

// Host is an engine that work is done for by workers, and that can make this
// process one of them.
type Host interface {
	// Work does steps on this machine until ctx is done.
	Work(ctx context.Context) error
}

// Fleet is an engine that knows its workers.
type Fleet interface {
	Workers(ctx context.Context) ([]Worker, error)
	// Shun stops a worker being given new work, or lets it be given work
	// again. What it has already been given, it finishes.
	Shun(ctx context.Context, worker string, shun bool) error
}

// Relay is an engine that deliveries from a forge can be passed to.
type Relay interface {
	// Relay passes on a delivery a door has received, for a worker to act on.
	Relay(ctx context.Context, d Delivery) error
	// AcceptWebhooks has the engine receive a forge's deliveries itself,
	// checking them against secret, and returns the path on the engine's
	// address that the forge should deliver to.
	AcceptWebhooks(ctx context.Context, forgeName string, secret []byte) (path string, err error)
}

// DeliveryEvent is the name a door gives the event it raises.
const DeliveryEvent = "starci:delivery"

// Delivery is one webhook delivery, as a door passes it on: enough to check
// it and act on it, and nothing interpreted.
type Delivery struct {
	// Forge is which forge it came from, as the door's address said:
	// "forgejo" or "github".
	Forge string `json:"forge"`
	// Event is the kind of delivery, from the forge's event header.
	Event string `json:"event"`
	// Signature is the forge's signature header, as it arrived.
	Signature string `json:"signature"`
	// Body is the request body, byte for byte: the signature is over it.
	Body string `json:"body"`
}

// Worker is a machine doing work for an engine.
type Worker struct {
	Name string
	// Shunned is true when it is finishing what it has and being given
	// nothing new.
	Shunned bool
	// Labels are what it offers, which steps ask for by worker, prefer and
	// avoid.
	Labels map[string]string
}

// CanTake reports whether some worker that is taking work satisfies a step's
// hard rules. Preferences are not counted: they never stop a step running.
func CanTake(workers []Worker, s plan.Step) bool {
	for _, w := range workers {
		if w.Shunned {
			continue
		}
		ok := true
		for k, v := range s.Worker {
			ok = ok && w.Labels[k] == v
		}
		for k, v := range s.Avoid {
			ok = ok && w.Labels[k] != v
		}
		if ok {
			return true
		}
	}
	return false
}

// Placement describes a step's hard rules in a few words.
func Placement(s plan.Step) string {
	var parts []string
	for _, k := range sortedKeys(s.Worker) {
		parts = append(parts, k+"="+s.Worker[k])
	}
	for _, k := range sortedKeys(s.Avoid) {
		parts = append(parts, "not "+k+"="+s.Avoid[k])
	}
	if len(parts) == 0 {
		return "any worker"
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
