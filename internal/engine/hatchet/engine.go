package hatchet

import (
	"context"
	"errors"
	"fmt"
	hatchetotel "github.com/hatchet-dev/hatchet/sdks/go/opentelemetry"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	sdk "github.com/hatchet-dev/hatchet/sdks/go"
	"github.com/hatchet-dev/hatchet/sdks/go/features"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	"git.phin.tech/sam/starci/internal/tracing"
)

// Options are what an Engine is made with. Everything here matters only to a
// process that will Work; one that only starts runs, or asks after workers,
// needs none of it.
type Options struct {
	// Machine is where this machine keeps its state, and what it shares.
	Machine step.Config
	// Post reports outcomes to the forge a run came from.
	Post Reporter
	// Reception lets this machine act on deliveries and fire schedules.
	Reception *Reception
	// Slots is how many steps to run at once.
	Slots int
	// Labels are what this machine offers to steps that ask.
	Labels map[string]string
	// Traces sends starci's spans to Hatchet as well as wherever else they
	// go, and has Hatchet carry the trace from a run to its steps. Hatchet
	// must have been started with SERVER_OBSERVABILITY_ENABLED.
	Traces bool
	// Dashboard is the address of Hatchet's dashboard, as a person reaches
	// it. With it, the status a run posts links to the run's page there,
	// which has each step's output and how long it took.
	Dashboard string
}

// Engine is Hatchet, as starci uses it. It does everything an engine may:
// it is durable, it places steps by label, it supersedes and excludes, it
// fires schedules, it has workers, and it can receive a forge's deliveries.
type Engine struct {
	client *sdk.Client
	tasks  Tasks
	opts   Options
}

var (
	_ engine.Engine    = (*Engine)(nil)
	_ engine.Starter   = (*Engine)(nil)
	_ engine.Scheduler = (*Engine)(nil)
	_ engine.Host      = (*Engine)(nil)
	_ engine.Fleet     = (*Engine)(nil)
	_ engine.Relay     = (*Engine)(nil)
)

// New connects to Hatchet, which is found through the HATCHET_CLIENT_*
// variables.
func New(o Options) (*Engine, error) {
	client, err := sdk.NewClient()
	if err != nil {
		return nil, err
	}
	return &Engine{client: client, tasks: NewTasks(client, o.Machine, o.Post, o.Reception, o.Dashboard), opts: o}, nil
}

func (e *Engine) Name() string { return "hatchet" }

func (e *Engine) Can() engine.Capabilities {
	return engine.Capabilities{Durable: true, Placement: true, Supersedes: true, Exclusive: true}
}

func (e *Engine) Execute(ctx context.Context, r engine.Run) (engine.Result, error) {
	var out engine.Result
	var res *sdk.TaskResult
	var err error
	if r.Mode == plan.Advanced {
		res, err = e.tasks.Script.Run(ctx, scriptInput(r))
	} else {
		res, err = e.tasks.Pipeline.Run(ctx, pipelineInput(r))
	}
	if err != nil {
		// A cancelled run comes back as an error with nothing in it.
		if strings.TrimSpace(err.Error()) == "" || strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return out, errors.New("the run was cancelled: a newer run replaced it, or one it is exclusive with was already going")
		}
		return out, err
	}
	return out, res.Into(&out)
}

func (e *Engine) Start(ctx context.Context, r engine.Run) error { return e.tasks.Start(ctx, r) }

func (e *Engine) Sync(ctx context.Context, forgeName, repo string, schedules []plan.Schedule) error {
	return (&crons{client: e.client}).Sync(ctx, forgeName, repo, schedules)
}

// Work makes this process a worker until ctx is done.
func (e *Engine) Work(ctx context.Context) error {
	slots := e.opts.Slots
	if slots < 1 {
		slots = 4
	}
	labels := map[string]any{}
	for k, v := range e.opts.Labels {
		labels[k] = v
	}
	worker, err := e.client.NewWorker("starci-"+e.opts.Machine.Worker,
		sdk.WithWorkflows(e.tasks.All()...),
		sdk.WithSlots(slots),
		// Pipelines and scripts only wait on their steps, so they get their
		// own pool and can never occupy the slots those steps need.
		sdk.WithDurableSlots(20),
		sdk.WithLabels(labels),
	)
	if err != nil {
		return err
	}
	if e.opts.Traces {
		// Hatchet's own part of tracing: a span for each task, in a trace it
		// carries from a run to its steps itself, and its collector as one
		// more place starci's spans are sent, which is what puts them on a
		// step's page in the dashboard.
		theirs, err := hatchetotel.NewInstrumentor(hatchetotel.WithTracerProvider(tracing.Provider()))
		if err != nil {
			return fmt.Errorf("setting up traces: %w", err)
		}
		worker.Use(theirs.Middleware())
		defer func() {
			flush, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = theirs.Shutdown(flush)
		}()
	}

	// A worker that fires schedules also looks, as it starts and from time to
	// time, for ones that asked to be caught up and missed a firing while
	// nothing was running to fire it.
	if e.tasks.timers != nil {
		go func() {
			tick := time.NewTicker(10 * time.Minute)
			defer tick.Stop()
			// Not at once: the worker has to be known to Hatchet first.
			wait := time.After(time.Minute)
			for {
				select {
				case <-ctx.Done():
					return
				case <-wait:
				case <-tick.C:
				}
				started, err := e.tasks.CatchUp(ctx, e.client, e.opts.Machine.StateDir, time.Now())
				if err != nil {
					fmt.Fprintln(os.Stderr, "starci: catching up schedules:", err)
				}
				for _, s := range started {
					fmt.Fprintln(os.Stderr, "starci: caught up a missed firing of", s)
				}
			}
		}()
	}
	return worker.StartBlocking(ctx)
}

// listed is a worker as Hatchet describes it, with the id Hatchet knows it by.
type listed struct {
	engine.Worker
	id string
}

func (e *Engine) list(ctx context.Context) ([]listed, error) {
	list, err := e.client.Workers().List(ctx)
	if err != nil {
		return nil, err
	}
	var out []listed
	if list.Rows == nil {
		return out, nil
	}
	for _, w := range *list.Rows {
		// Hatchet keeps a record of every worker that ever connected.
		if w.Status == nil || *w.Status == rest.INACTIVE {
			continue
		}
		l := listed{id: w.Metadata.Id, Worker: engine.Worker{Name: w.Name, Shunned: *w.Status == rest.PAUSED, Labels: map[string]string{}}}
		if w.Labels != nil {
			for _, label := range *w.Labels {
				if label.Value != nil {
					l.Labels[label.Key] = *label.Value
				}
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (e *Engine) Workers(ctx context.Context) ([]engine.Worker, error) {
	all, err := e.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]engine.Worker, len(all))
	for i, w := range all {
		out[i] = w.Worker
	}
	return out, nil
}

func (e *Engine) Shun(ctx context.Context, name string, shun bool) error {
	all, err := e.list(ctx)
	if err != nil {
		return err
	}
	done := 0
	for _, w := range all {
		// A worker is called starci-<name>; either is accepted.
		if w.Name != name && w.Name != "starci-"+name {
			continue
		}
		if shun {
			_, err = e.client.Workers().Pause(ctx, w.id)
		} else {
			_, err = e.client.Workers().Unpause(ctx, w.id)
		}
		if err != nil {
			return err
		}
		done++
	}
	if done == 0 {
		return fmt.Errorf("no connected worker is named %q", name)
	}
	return nil
}

func (e *Engine) Relay(ctx context.Context, d engine.Delivery) error {
	return e.client.Events().Push(ctx, engine.DeliveryEvent, d)
}

// AcceptWebhooks registers a webhook with Hatchet for a forge. Hatchet then
// receives the forge's deliveries itself, checks their signatures against
// secret, and raises the event a worker acts on.
func (e *Engine) AcceptWebhooks(ctx context.Context, forgeName string, secret []byte) (string, error) {
	auth := features.HMACAuth{SigningSecret: string(secret), Algorithm: rest.SHA256, Encoding: rest.HEX}
	source := rest.GENERIC
	switch forgeName {
	case "forgejo":
		auth.SignatureHeaderName = "X-Forgejo-Signature"
	case "github":
		auth.SignatureHeaderName = "X-Hub-Signature-256"
		source = rest.GITHUB
	default:
		return "", fmt.Errorf("unknown forge %q: forgejo or github", forgeName)
	}
	hook, err := e.client.Webhooks().Create(ctx, features.CreateWebhookOpts{
		Name:       "starci-" + forgeName,
		SourceName: source,
		// A constant: every delivery from this forge is the same event, and
		// the worker reads what happened out of it.
		EventKeyExpression: "'" + HookEvent(forgeName) + "'",
		Auth:               auth,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/api/v1/stable/tenants/%s/webhooks/%s", hook.TenantId, hook.Name), nil
}
