package plan

import (
	"context"
	"strings"
	"testing"
)

const keeps = `
def ci(ctx):
    build = step("build", image = "i", run = "make", keep = ["out/app"])
    return [build, step("test", image = "i", run = "./app", artifacts = {"app": artifact(build, "out/app")})]
`

// A step handed another's artifact in a basic plan needs that step, and is
// given the real pointer once the step has kept it.
func TestAPlanHandsOnWhatAStepKept(t *testing.T) {
	p := mustEval(t, keeps)
	test := p.Steps[1]
	if len(test.Needs) != 1 || test.Needs[0] != "build" {
		t.Errorf("test needs %v, want build", test.Needs)
	}
	d, err := p.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Run) != 1 || d.Run[0].Step.Name != "build" {
		t.Fatalf("first asked for %+v, want build alone", d.Run)
	}
	built := d.Run[0].Answer(Result{Status: "passed", Artifacts: map[string]Pointer{"out/app": {Hash: "abc", Size: 3}}})
	d, err = p.Next([]Result{built})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Run) != 1 || d.Run[0].Step.Artifacts["app"].Hash != "abc" {
		t.Fatalf("then asked for %+v, want test handed the pointer build kept", d.Run)
	}
	// The result answers the step as it ran, with the pointer in place.
	tested := d.Run[0].Answer(Result{Status: "passed"})
	d, err = p.Next([]Result{built, tested})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Done || !d.OK {
		t.Errorf("decision = %+v, want the run over and passed", d)
	}
}

// What a script hands on and what it keeps are checked before anything runs.
func TestArtifactMistakes(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"handing on what a step does not keep", `
def ci(ctx):
    build = step("build", image = "i", run = "make", keep = ["out/app"])
    return [build, step("test", image = "i", run = "r", artifacts = {"app": artifact(build, "out/other")})]
`, `which "build" does not keep`},
		{"keeping a path outside the workspace", `
def ci(ctx):
    return [step("build", image = "i", run = "make", keep = ["../app"])]
`, "inside the workspace"},
		{"putting an artifact outside the workspace", `
def ci(ctx):
    build = step("build", image = "i", run = "make", keep = ["app"])
    return [build, step("test", image = "i", run = "r", artifacts = {"/bin/app": artifact(build, "app")})]
`, "inside the workspace"},
		{"something that is not an artifact", `
def ci(ctx):
    return [step("test", image = "i", run = "r", artifacts = {"app": "out/app"})]
`, "maps a path in the workspace to an artifact"},
		{"an artifact of a step not in the plan", `
def ci(ctx):
    return [step("test", image = "i", run = "r", artifacts = {"app": artifact("build", "app")})]
`, `needs "build"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantError(t, c.src, c.want) })
	}
}

// In an advanced script, what a step kept is on its result, and is handed on
// as it is.
func TestPipelineHandsOnWhatAStepKept(t *testing.T) {
	src := `
def pipeline(ctx):
    build = run(step("build", image = "i", run = "make", keep = ["out"]))
    run(step("test", image = "i", run = "r", artifacts = {"bin": build.artifacts["out"]}))
`
	d, err := NextInPipeline(context.Background(), "ci.star", []byte(src), ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	built := d.Run[0].Answer(Result{Status: "passed", Artifacts: map[string]Pointer{"out": {Hash: "def"}}})
	d, err = NextInPipeline(context.Background(), "ci.star", []byte(src), ctx, []Result{built})
	if err != nil {
		t.Fatal(err)
	}
	if d.Error != "" || len(d.Run) != 1 || d.Run[0].Step.Artifacts["bin"].Hash != "def" {
		t.Fatalf("decision = %+v, want test handed build's pointer", d)
	}
}

func TestArtifactIsForBasicPlans(t *testing.T) {
	d, err := NextInPipeline(context.Background(), "ci.star", []byte(`
def pipeline(ctx):
    artifact("build", "out")
`), ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Error == "" || !strings.Contains(d.Error, "result.artifacts") {
		t.Errorf("error = %q, want one pointing at result.artifacts", d.Error)
	}
}
