package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// Gate is a place where an advanced script stops and waits for a person:
//
//	answer = wait_for("approve deploy", timeout = "4h")
//
// It is known by what the run is for and the name the script gives it, which
// is what whoever answers has to say. Two runs for the same commit and the
// same event that wait at a gate of the same name are answered together.
type Gate struct {
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	Event string `json:"event"`
	Name  string `json:"name"`
	// Seconds is how long to wait. A gate nobody answers in that time is
	// taken to have been refused.
	Seconds int `json:"seconds"`
}

// A gate's name is printed in a command for a person to copy into a shell,
// so it is held to what means nothing to one.
var gateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.:-]{0,99}$`)

// DefaultGateWait is how long a gate waits when the script does not say.
const DefaultGateWait = 24 * time.Hour

// ID names a gate in one short string, the same wherever it is worked out,
// for an engine to match an answer to the wait it is for.
func (g Gate) ID() string {
	event := g.Event
	if event == "" {
		event = EventPush
	}
	sum := sha256.Sum256([]byte(g.Repo + "\n" + g.SHA + "\n" + event + "\n" + g.Name))
	return hex.EncodeToString(sum[:16])
}

// Answer is what came back from a gate.
type Answer struct {
	Approved bool `json:"approved"`
	// By is who answered, as far as whoever passed the answer on said.
	By string `json:"by,omitempty"`
	// TimedOut is true when nobody answered in time. It is then not
	// approved.
	TimedOut bool `json:"timed_out,omitempty"`
}

// Waiter is a Runner that can also wait at a gate. Not every engine can: one
// has to be able to stop a run, keep it, and be told an answer from outside.
// Wait returns when the gate is answered or its time is up. An error means
// the run could not wait at all.
type Waiter interface {
	Wait(g Gate) (Answer, error)
}

// Waits reports whether a script asks to wait for a person anywhere in it.
// What an advanced script will do is not known until it runs, but whether it
// can come to a gate is, and that is worth saying before a run is started
// on an engine that cannot wait.
func Waits(filename string, src []byte) bool {
	f, err := (&syntax.FileOptions{}).Parse(filename, src, 0)
	if err != nil {
		return false
	}
	found := false
	syntax.Walk(f, func(n syntax.Node) bool {
		if id, ok := n.(*syntax.Ident); ok && id.Name == "wait_for" {
			found = true
		}
		return !found
	})
	return found
}

func waitForBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, timeout string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name, "timeout?", &timeout); err != nil {
		return nil, err
	}
	if !gateName.MatchString(name) {
		return nil, fmt.Errorf("wait_for: a gate needs a name, which is what whoever answers it is shown and types: letters, digits, spaces and - _ . : only, starting with a letter or digit; got %q", name)
	}
	wait := DefaultGateWait
	if timeout != "" {
		d, err := time.ParseDuration(timeout)
		if err != nil || d < time.Second || d%time.Second != 0 {
			return nil, fmt.Errorf("wait_for: timeout %q is not a length of time in whole seconds; write it like 30m or 4h", timeout)
		}
		wait = d
	}
	rec, _ := thread.Local(runnerKey).(*recorder)
	if rec == nil || !rec.started {
		return nil, fmt.Errorf("wait_for: a script can only wait inside pipeline(ctx)")
	}
	waiter, ok := rec.runner.(Waiter)
	if !ok {
		return nil, fmt.Errorf("wait_for(%q): this run is being carried out by something that cannot wait for a person", name)
	}
	answer, err := waiter.Wait(Gate{Repo: rec.run.Repo, SHA: rec.run.SHA, Event: rec.run.Event, Name: name, Seconds: int(wait / time.Second)})
	if err != nil {
		return nil, fmt.Errorf("wait_for(%q): %w", name, err)
	}
	return starlarkstruct.FromStringDict(starlark.String("answer"), starlark.StringDict{
		"approved":  starlark.Bool(answer.Approved),
		"by":        starlark.String(answer.By),
		"timed_out": starlark.Bool(answer.TimedOut),
	}), nil
}
