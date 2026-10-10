package plan

import (
	"reflect"
	"testing"
)

func TestAStepNamesItsSecrets(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [step("publish", image = "i", run = "r", secrets = ["NPM_TOKEN", "A_KEY", "NPM_TOKEN"])]
`)
	if want := []string{"A_KEY", "NPM_TOKEN"}; !reflect.DeepEqual(p.Steps[0].Secrets, want) {
		t.Errorf("secrets = %v, want %v: sorted, each once", p.Steps[0].Secrets, want)
	}
	if p.Steps[0].Withheld {
		t.Error("a push was marked as not to be given secrets")
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestSecretsThatCannotBe(t *testing.T) {
	for src, fragment := range map[string]string{
		`step("a", image = "i", run = "r", secrets = ["has space"])`:           "secrets must be names",
		`step("a", image = "i", run = "r", secrets = ["../etc/passwd"])`:       "secrets must be names",
		`step("a", image = "i", run = "r", secrets = ["1ST"])`:                 "secrets must be names",
		`step("a", image = "i", run = "r", secrets = [""])`:                    "secrets must be names",
		`step("a", image = "i", run = "r", secrets = [1])`:                     "secrets must be names",
		`step("a", image = "i", run = "r", env = {"T": "x"}, secrets = ["T"])`: "T is in both env and secrets",
		// Names the program that starts the container would act on.
		`step("a", image = "i", run = "r", secrets = ["DOCKER_HOST"])`: "a secret cannot be called DOCKER_HOST",
		`step("a", image = "i", run = "r", secrets = ["PATH"])`:        "a secret cannot be called PATH",
		`step("a", image = "i", run = "r", secrets = ["LD_PRELOAD"])`:  "a secret cannot be called LD_PRELOAD",
	} {
		wantError(t, "def ci(ctx):\n    return ["+src+"]\n", fragment)
	}
}

const publishScript = `
def ci(ctx):
    return [
        step("test", image = "i", run = "r"),
        step("publish", image = "i", run = "r", secrets = ["NPM_TOKEN"]),
    ]
`

func TestAForkIsGivenNoSecrets(t *testing.T) {
	withheld := func(c Context) []bool {
		t.Helper()
		p, err := Eval("ci.star", []byte(publishScript), c)
		if err != nil {
			t.Fatal(err)
		}
		return []bool{p.Steps[0].Withheld, p.Steps[1].Withheld}
	}
	fork := Context{Event: EventPullRequest, PR: &PullRequest{Number: 1, Base: "main", Head: "x", Fork: true}}
	own := Context{Event: EventPullRequest, PR: &PullRequest{Number: 1, Base: "main", Head: "x"}}

	// Only the step that asks for secrets, and only from a fork.
	if got := withheld(fork); !reflect.DeepEqual(got, []bool{false, true}) {
		t.Errorf("from a fork: withheld = %v, want only publish", got)
	}
	if got := withheld(own); !reflect.DeepEqual(got, []bool{false, false}) {
		t.Errorf("from the repository's own branch: withheld = %v, want neither", got)
	}
}

func TestAnAdvancedScriptsStepsAreWithheldFromAFork(t *testing.T) {
	const script = `
def pipeline(ctx):
    run(step("test", image = "i", run = "r"))
    run(step("publish", image = "i", run = "r", secrets = ["NPM_TOKEN"]), allow_failure = True)
`
	for name, c := range map[string]struct {
		pr   *PullRequest
		want bool
	}{
		"a fork":     {&PullRequest{Number: 1, Fork: true}, true},
		"not a fork": {&PullRequest{Number: 1}, false},
		"not a PR":   {nil, false},
	} {
		r := &withholding{}
		if _, err := RunPipeline(t.Context(), "ci.star", []byte(script), Context{PR: c.pr}, r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.withheld["test"] || r.withheld["publish"] != c.want {
			t.Errorf("%s: withheld = %v, want publish %v and test never", name, r.withheld, c.want)
		}
	}
}

// withholding is a runner that notes which steps reached it marked as not
// to be given secrets.
type withholding struct{ withheld map[string]bool }

func (w *withholding) Run(steps []Step) ([]Result, error) {
	if w.withheld == nil {
		w.withheld = map[string]bool{}
	}
	out := make([]Result, len(steps))
	for i, s := range steps {
		w.withheld[s.Name] = s.Withheld
		out[i] = Result{Name: s.Name, Status: "passed"}
	}
	return out, nil
}

// A plan that arrives as JSON is held to the same names.
func TestAPlanCannotNameAReservedSecret(t *testing.T) {
	p := Plan{Steps: []Step{{Name: "a", Image: "i", Run: "r", Inputs: []string{"."}, Secrets: []string{"DOCKER_HOST"}}}}
	if err := p.Validate(); err == nil {
		t.Error("a plan with a secret called DOCKER_HOST is valid")
	}
}
