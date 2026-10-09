// Package trigger turns a push delivered by a forge into a run.
package trigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	Report     Report
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

// Handler receives webhook deliveries at /hooks/<forge name>.
//
// It answers quickly and never waits for a run: it checks the delivery is
// genuine, fetches the script for the pushed commit, and submits the run.
// The script is evaluated here, in the receiver. That is safe to do with a
// script from any repository, because a script cannot do anything but
// describe steps.
type Handler struct {
	Hooks  map[string]Hook
	Submit Submitter
	Log    func(format string, args ...any)
	// AllowForks runs pull requests that come from other repositories. Off
	// unless asked for: such a pull request is code from someone who cannot
	// push to the repository, and running it means running it on this
	// installation's workers.
	AllowForks bool
}

func (h *Handler) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/hooks/")
	hook, known := h.Hooks[name]
	if !ok || !known || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	push, err := hook.Forge.ParsePush(r.Header, body, hook.Secret)
	switch {
	case errors.Is(err, forge.ErrSignature):
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	case errors.Is(err, forge.ErrIgnored):
		fmt.Fprintln(w, "ignored:", err)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if push.PR != nil && push.PR.Fork && !h.AllowForks {
		h.logf("%s %s: pull request %d is from a fork; not run", name, push.Repo, push.PR.Number)
		fmt.Fprintln(w, "ignored: pull requests from forks are not run")
		return
	}

	outcome, err := h.handle(r.Context(), hook.Forge, push)
	if err != nil {
		h.logf("%s %s@%.8s: %v", name, push.Repo, push.SHA, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	h.logf("%s %s@%.8s (%s): %s", name, push.Repo, push.SHA, Supersedes(contextOf(push)), outcome)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, outcome)
}

// handle decides what a push leads to and says so in a sentence. An error
// means the forge or the engine could not be reached, which is worth the
// forge retrying; a broken script is not an error here, it is a failed run.
func (h *Handler) handle(ctx context.Context, f forge.Forge, push forge.Push) (string, error) {
	src, err := f.File(ctx, push.Repo, push.SHA, Script)
	if errors.Is(err, forge.ErrNotFound) {
		return "no " + Script + " at this commit; nothing to do", nil
	}
	if err != nil {
		return "", err
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
