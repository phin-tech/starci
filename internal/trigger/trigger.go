// Package trigger turns a push delivered by a forge into a run.
package trigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/forge"
	"git.phin.tech/sam/starci/internal/plan"
)

// Script is where a repository keeps its CI script.
const Script = ".ci.star"

// Report says where a run's outcome is to be posted when it finishes.
type Report struct {
	Forge string `json:"forge"`
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	// Context is the name the status appears under on the commit.
	Context string `json:"context,omitempty"`
}

// statusContext names the line a run's status goes on. A commit pushed to a
// branch that also has a pull request open gets a run for each, and they
// must not overwrite one another.
func statusContext(event string) string {
	switch event {
	case plan.EventPullRequest:
		return forge.StatusContext + "/pull-request"
	case plan.EventTag:
		return forge.StatusContext + "/tag"
	}
	return forge.StatusContext
}

// Run is everything needed to start a run for a push.
type Run struct {
	Mode       string
	Plan       *plan.Plan // basic mode
	Filename   string     // advanced mode
	Source     []byte     // advanced mode
	Context    plan.Context
	CloneURL   string
	Supersedes string
	// Exclusive, if set, names runs of which there is to be only one at a
	// time, where a newer one gives way to the one in progress. It is the
	// reverse of Supersedes, where the one in progress gives way.
	Exclusive string
	Report    Report
	// Broken is why the script could not be evaluated, for a run that was
	// asked for anyway. There is then nothing to execute, only this to say.
	Broken string
}

// Submitter hands a run to whatever will execute it, without waiting for it.
type Submitter interface {
	Submit(ctx context.Context, r Run) error
}

// Hook is a forge and the secret it signs deliveries with.
type Hook struct {
	Forge  forge.Forge
	Secret []byte
}

// Receiving a delivery is two jobs, done in two places.
//
// A Door is the address a forge delivers to. It does as little as a thing on
// the internet can: it wraps what arrived as a Delivery and passes it on.
// It holds no access to the forge, reads no script, and can start nothing.
// That makes it small enough to be anything: this package's Door behind
// `starci serve`, a function on a cloud provider, or an engine's own webhook
// endpoint. doors/ has the contract and other implementations.
//
// A Receiver is where a Delivery is acted on, on a worker. It checks the
// signature itself, with the secret it holds, so a door does not have to be
// trusted; then it fetches the script, evaluates it, and starts the run.

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

// Door receives deliveries at /hooks/<forge name> and sends each on.
type Door struct {
	// Forges are the forges to accept deliveries for, by name.
	Forges map[string]forge.Forge
	// Secrets, where one is given for a forge, lets the door turn away an
	// unsigned delivery itself instead of passing it on to be turned away.
	// The Receiver checks regardless.
	Secrets map[string][]byte
	// Send passes a delivery on.
	Send func(ctx context.Context, d Delivery) error
	Log  func(format string, args ...any)
}

func (d *Door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/hooks/")
	f, known := d.Forges[name]
	if !ok || !known || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	eventHeaders, signatureHeaders := f.Headers()
	delivery := Delivery{
		Forge:     name,
		Event:     forge.First(r.Header, eventHeaders),
		Signature: forge.First(r.Header, signatureHeaders),
		Body:      string(body),
	}
	if secret := d.Secrets[name]; len(secret) > 0 && !f.Signed(delivery.Signature, body, secret) {
		http.Error(w, forge.ErrSignature.Error(), http.StatusUnauthorized)
		return
	}
	// Passed on even if the forge stops waiting for the answer: a delivery
	// that has arrived should not be lost to an impatient sender.
	ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer stop()
	if err := d.Send(ctx, delivery); err != nil {
		if errors.Is(err, forge.ErrSignature) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if d.Log != nil {
			d.Log("%s: could not pass a delivery on: %v", name, err)
		}
		// Worth the forge trying again.
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "accepted")
}

// Scheduler keeps an engine's timers in step with what scripts declare.
type Scheduler interface {
	// Sync makes the schedules that fire for a repository exactly these:
	// adding, changing and removing as needed.
	Sync(ctx context.Context, forgeName, repo string, schedules []plan.Schedule) error
}

// Receiver acts on deliveries.
type Receiver struct {
	Hooks  map[string]Hook
	Submit Submitter
	Log    func(format string, args ...any)
	// Schedules, if set, is told what a repository's default branch
	// declares each time that branch is pushed to.
	Schedules Scheduler
	// AllowForks runs pull requests that come from other repositories. Off
	// unless asked for: such a pull request is code from someone who cannot
	// push to the repository, and running it means running it on this
	// installation's workers.
	AllowForks bool
}

func (h *Receiver) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

// Deliver checks a delivery and acts on it, saying in a sentence what it led
// to. forge.ErrSignature means it was not genuine. Any other error means the
// forge or the engine could not be reached, and the delivery is worth trying
// again; a broken script is not an error here, it is a failed run.
func (h *Receiver) Deliver(ctx context.Context, d Delivery) (string, error) {
	hook, ok := h.Hooks[d.Forge]
	if !ok {
		return "", fmt.Errorf("no forge named %q is configured here", d.Forge)
	}
	if !hook.Forge.Signed(d.Signature, []byte(d.Body), hook.Secret) {
		return "", forge.ErrSignature
	}
	return h.act(ctx, hook.Forge, d.Event, []byte(d.Body))
}

// DeliverVerified acts on a delivery something else has already checked the
// signature of, such as an engine's own webhook endpoint configured with the
// forge's secret. It has only the body to go on.
func (h *Receiver) DeliverVerified(ctx context.Context, forgeName string, body []byte) (string, error) {
	hook, ok := h.Hooks[forgeName]
	if !ok {
		return "", fmt.Errorf("no forge named %q is configured here", forgeName)
	}
	return h.act(ctx, hook.Forge, "", body)
}

func (h *Receiver) act(ctx context.Context, f forge.Forge, event string, body []byte) (string, error) {
	push, err := f.Parse(event, body)
	if errors.Is(err, forge.ErrIgnored) {
		return "ignored: " + err.Error(), nil
	}
	if err != nil {
		return "ignored: " + err.Error(), nil
	}
	if push.PR != nil && push.PR.Fork && !h.AllowForks {
		h.logf("%s %s: pull request %d is from a fork; not run", f.Name(), push.Repo, push.PR.Number)
		return "ignored: pull requests from forks are not run", nil
	}
	outcome, err := h.handle(ctx, f, push)
	if err != nil {
		h.logf("%s %s@%.8s: %v", f.Name(), push.Repo, push.SHA, err)
		return "", err
	}
	h.logf("%s %s@%.8s (%s): %s", f.Name(), push.Repo, push.SHA, Supersedes(contextOf(push)), outcome)
	return outcome, nil
}

// handle decides what a push leads to and says so in a sentence. An error
// means the forge or the engine could not be reached, which is worth the
// forge retrying; a broken script is not an error here, it is a failed run.
func (h *Receiver) handle(ctx context.Context, f forge.Forge, push forge.Push) (string, error) {
	src, err := f.File(ctx, push.Repo, push.SHA, Script)
	missing := errors.Is(err, forge.ErrNotFound)
	if err != nil && !missing {
		return "", err
	}
	// The default branch is the only place schedules are read from: what it
	// declares now is what fires, and a script that has gone declares none.
	if h.Schedules != nil && push.Event == plan.EventPush && push.DefaultBranch != "" && push.Ref == push.DefaultBranch {
		var declared []plan.Schedule
		if !missing {
			// A script that cannot be read changes nothing: better last
			// week's schedules than none because of a typo.
			if declared, err = plan.Schedules(Script, src); err != nil {
				declared = nil
				h.logf("%s %s: schedules left as they were, the script cannot be read: %s", f.Name(), push.Repo, firstLine(err.Error()))
			}
		}
		if missing || err == nil {
			if serr := h.Schedules.Sync(ctx, f.Name(), push.Repo, declared); serr != nil {
				h.logf("%s %s: could not update schedules: %v", f.Name(), push.Repo, serr)
			}
		}
	}
	if missing {
		return "no " + Script + " at this commit; nothing to do", nil
	}

	line := statusContext(push.Event)
	status := func(state, description string) error {
		return f.SetStatus(ctx, push.Repo, push.SHA, forge.Status{State: state, Description: description, Context: line})
	}
	c := contextOf(push)
	run := Run{
		Context:    c,
		CloneURL:   push.CloneURL,
		Supersedes: Supersedes(c),
		Report:     Report{Forge: f.Name(), Repo: push.Repo, SHA: push.SHA, Context: line},
	}

	run.Mode, err = plan.Mode(Script, src)
	if err == nil && run.Mode == plan.Basic {
		run.Plan, err = plan.Eval(Script, src, c)
	}
	if err != nil {
		// The author needs to see this beside the commit, not in a log here.
		if serr := status("failure", firstLine(err.Error())); serr != nil {
			return "", serr
		}
		return "the script is broken: " + firstLine(err.Error()), nil
	}

	queued := "queued"
	if run.Mode == plan.Basic {
		if len(run.Plan.Steps) == 0 {
			if err := status("success", "nothing to run for this commit"); err != nil {
				return "", err
			}
			return "the plan is empty for this commit", nil
		}
		queued = fmt.Sprintf("queued: %d step(s)", len(run.Plan.Steps))
	} else {
		run.Filename, run.Source = Script, src
	}

	// Pending first: if submitting fails, the forge retries the delivery and
	// the commit is not left showing nothing at all.
	if err := status("pending", queued); err != nil {
		return "", err
	}
	if err := h.Submit.Submit(ctx, run); err != nil {
		_ = status("error", "could not be queued: "+firstLine(err.Error()))
		return "", fmt.Errorf("submitting the run: %w", err)
	}
	return queued, nil
}

// Scheduled works out the run for a schedule that has fired: the default
// branch as it stands now, and what the script there wants done for this
// schedule. found is false when there is nothing for the schedule to do any
// more, because the repository, the script or the declaration has gone, and
// the timer that fired should be removed.
func (h *Receiver) Scheduled(ctx context.Context, forgeName, repo, name string) (run Run, found bool, err error) {
	hook, ok := h.Hooks[forgeName]
	if !ok {
		return Run{}, false, fmt.Errorf("no forge named %q is configured here", forgeName)
	}
	f := hook.Forge
	head, err := f.Head(ctx, repo)
	if errors.Is(err, forge.ErrNotFound) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	src, err := f.File(ctx, repo, head.SHA, Script)
	if errors.Is(err, forge.ErrNotFound) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}

	line := forge.StatusContext + "/schedule-" + name
	c := plan.Context{Repo: repo, SHA: head.SHA, Ref: head.Branch, Event: plan.EventSchedule, Schedule: name}
	run = Run{
		Context:    c,
		CloneURL:   head.CloneURL,
		Supersedes: Supersedes(c),
		Report:     Report{Forge: f.Name(), Repo: repo, SHA: head.SHA, Context: line},
	}

	declared, err := plan.Schedules(Script, src)
	if err == nil {
		still := false
		for _, s := range declared {
			still = still || s.Name == name
		}
		if !still {
			return Run{}, false, nil
		}
		run.Mode, err = plan.Mode(Script, src)
	}
	if err == nil && run.Mode == plan.Basic {
		run.Plan, err = plan.Eval(Script, src, c)
	}
	if err != nil {
		// A script broken since the schedule was declared: say so on the
		// commit, and leave the timer for when it is fixed.
		run.Broken = firstLine(err.Error())
		return run, true, nil
	}
	if run.Mode == plan.Advanced {
		run.Filename, run.Source = Script, src
	}
	return run, true, nil
}

// contextOf is what a script is told about a delivery.
func contextOf(push forge.Push) plan.Context {
	return plan.Context{
		Repo: push.Repo, SHA: push.SHA, Ref: push.Ref, ChangedFiles: push.Changed,
		Event: push.Event, Tag: push.Tag, PR: push.PR,
	}
}

func firstLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	// A Starlark error ends with the line that says what went wrong.
	return strings.TrimSpace(lines[len(lines)-1])
}

// Supersedes names what a run replaces: a newer run with the same name
// cancels one still in progress. Each branch, tag, pull request and schedule
// is its own line of runs; a run that belongs to none of them replaces only a
// repeat of itself.
func Supersedes(c plan.Context) string {
	switch {
	case c.Event == plan.EventPullRequest && c.PR != nil:
		return fmt.Sprintf("%s@pr-%d", c.Repo, c.PR.Number)
	case c.Event == plan.EventTag:
		return c.Repo + "@tag-" + c.Tag
	case c.Event == plan.EventSchedule:
		return c.Repo + "@schedule-" + c.Schedule
	case c.Ref != "":
		return c.Repo + "@" + c.Ref
	}
	return c.Repo + "@" + c.SHA
}

// Outcome is the status to post for a finished run.
func Outcome(results []plan.Result, runErr string) forge.Status {
	passed, failed, skipped := 0, []string{}, 0
	for _, r := range results {
		switch r.Status {
		case "passed":
			passed++
		case "skipped":
			skipped++
		default:
			failed = append(failed, r.Name)
		}
	}
	switch {
	case len(failed) > 0:
		return forge.Status{State: "failure", Description: "failed: " + strings.Join(failed, ", ")}
	case runErr != "":
		return forge.Status{State: "failure", Description: firstLine(runErr)}
	case skipped > 0:
		return forge.Status{State: "failure", Description: fmt.Sprintf("%d step(s) skipped", skipped)}
	}
	return forge.Status{State: "success", Description: fmt.Sprintf("%d step(s) passed", passed)}
}
