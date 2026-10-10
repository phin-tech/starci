package plan

import (
	"reflect"
	"strings"
	"testing"
)

func TestAStepIsGivenAnotherStepsOutputs(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    build = step("build", image = "i", run = "r", inputs = ["src"], outputs = ["dist/", "bin/app", "dist"])
    return [
        build,
        step("test", image = "i", run = "r", inputs = ["tests", build]),
        step("ship", image = "i", run = "r", inputs = [build]),
    ]
`)
	build, test, ship := p.Steps[0], p.Steps[1], p.Steps[2]
	if want := []string{"bin/app", "dist"}; !reflect.DeepEqual(build.Outputs, want) {
		t.Errorf("build's outputs = %v, want %v: cleaned, sorted, each once", build.Outputs, want)
	}
	// Being given a step's outputs means coming after it, without saying so.
	if !reflect.DeepEqual(test.Inputs, []string{"tests"}) || !reflect.DeepEqual(test.From, []string{"build"}) || !reflect.DeepEqual(test.Needs, []string{"build"}) {
		t.Errorf("test = inputs %v, from %v, needs %v", test.Inputs, test.From, test.Needs)
	}
	// A step may take nothing from the repository at all.
	if len(ship.Inputs) != 0 || !reflect.DeepEqual(ship.From, []string{"build"}) {
		t.Errorf("ship = inputs %v, from %v; want only what build produced", ship.Inputs, ship.From)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("the plan is not valid: %v", err)
	}
}

func TestOutputsThatCannotBe(t *testing.T) {
	for src, fragment := range map[string]string{
		`step("a", image = "i", run = "r", outputs = ["."])`:                              "outputs must be files or directories inside",
		`step("a", image = "i", run = "r", outputs = ["../up"])`:                          "outputs must be files or directories inside",
		`step("a", image = "i", run = "r", outputs = ["/abs"])`:                           "outputs must be files or directories inside",
		`step("a", image = "i", run = "r", outputs = ["dist/*.js"])`:                      "without wildcards",
		`step("a", image = "i", run = "r", outputs = [1])`:                                "outputs must be files or directories inside",
		`step("b", image = "i", run = "r", inputs = [step("a", image = "i", run = "r")])`: `step "a" is given as an input, but declares no outputs`,
		`step("b", image = "i", run = "r", inputs = [1])`:                                 "inputs must be paths, or steps that declare outputs",
	} {
		wantError(t, "def ci(ctx):\n    return ["+src+"]\n", fragment)
	}

	// A step whose outputs are wanted has to be part of the run.
	wantError(t, `
def ci(ctx):
    build = step("build", image = "i", run = "r", outputs = ["dist"])
    return [step("test", image = "i", run = "r", inputs = [build])]
`, `needs "build", which ci(ctx) did not return`)
}

func TestRunPlanHandsOnWhatAStepProduced(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    build = step("build", image = "i", run = "r", outputs = ["dist", "bin"])
    return [build, step("test", image = "i", run = "r", inputs = ["tests", build])]
`)
	r := &fakeRunner{}
	results, err := RunPlan(p, r)
	if err != nil || !Passed(results) {
		t.Fatalf("RunPlan: %v, %+v", err, results)
	}
	want := []Given{
		{Step: "build", Path: "bin", Digest: "digest of build/bin"},
		{Step: "build", Path: "dist", Digest: "digest of build/dist"},
	}
	if !reflect.DeepEqual(r.given["test"], want) {
		t.Errorf("test was given %+v, want %+v", r.given["test"], want)
	}
	// The plan itself is not written on: it may be run again.
	if p.Steps[1].Given != nil {
		t.Error("running the plan changed it")
	}
}

func TestRunPlanDoesNotRunAStepWithNothingToBeGiven(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    build = step("build", image = "i", run = "r", outputs = ["dist"])
    return [build, step("test", image = "i", run = "r", inputs = [build])]
`)
	// Passed, by an engine that lost what it left.
	r := &fakeRunner{outcomes: map[string][]Result{"build": {{Status: "passed", Outputs: map[string]string{}}}}}
	results, err := RunPlan(p, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.trace(); got != "build" {
		t.Errorf("ran %s, want only build", got)
	}
	if results[1].Status != "failed" || !strings.Contains(results[1].Detail, "outputs were not kept") {
		t.Errorf("test = %+v, want it failed, saying why", results[1])
	}
}

func TestAPipelineHandsOnWhatAStepProduced(t *testing.T) {
	r := &fakeRunner{}
	_, err := pipeline(t, `
def pipeline(ctx):
    build = step("build", image = "i", run = "r", outputs = ["dist"])
    run(build)
    parallel([step("test " + n, image = "i", run = "r", inputs = ["tests", build]) for n in ["a", "b"]])
`, r)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	want := []Given{{Step: "build", Path: "dist", Digest: "digest of build/dist"}}
	if !reflect.DeepEqual(r.given["test a"], want) || !reflect.DeepEqual(r.given["test b"], want) {
		t.Errorf("the tests were given %+v and %+v, want %+v", r.given["test a"], r.given["test b"], want)
	}
}

func TestAPipelineCannotGiveWhatHasNotBeenMade(t *testing.T) {
	const script = `
build = step("build", image = "i", run = "r", outputs = ["dist"])
test = step("test", image = "i", run = "r", inputs = [build])
`
	// Never run.
	r := &fakeRunner{}
	_, err := pipeline(t, script+`
def pipeline(ctx):
    run(test)
`, r)
	if err == nil || !strings.Contains(err.Error(), "build has not passed in this run") {
		t.Errorf("err = %v, want it to say build has not been run", err)
	}
	if len(r.batches) != 0 {
		t.Errorf("ran %s, want nothing", r.trace())
	}

	// Run, and failed, and the script carried on.
	r = &fakeRunner{outcomes: map[string][]Result{"build": {{Status: "failed"}}}}
	_, err = pipeline(t, script+`
def pipeline(ctx):
    run(build, allow_failure = True)
    run(test)
`, r)
	if err == nil || !strings.Contains(err.Error(), "build has not passed in this run") {
		t.Errorf("err = %v, want it to say build did not pass", err)
	}

	// Run again and passed: it is the latest run whose outputs are given.
	r = &fakeRunner{outcomes: map[string][]Result{"build": {
		{Status: "failed"},
		{Status: "passed", Outputs: map[string]string{"dist": "second"}},
	}}}
	_, err = pipeline(t, script+`
def pipeline(ctx):
    if not run(build, allow_failure = True).ok:
        run(build)
    run(test)
`, r)
	if err != nil || len(r.given["test"]) != 1 || r.given["test"][0].Digest != "second" {
		t.Errorf("err = %v, test was given %+v; want the second build's output", err, r.given["test"])
	}
}

// An engine checks each step on its own as it hands it to a machine, by
// which time what the step is given is written on it and the steps it came
// from are not there to be looked up.
func TestAStepGivenOnlyOutputsIsValidOnItsOwn(t *testing.T) {
	p := Plan{Steps: []Step{{Name: "use", Image: "i", Run: "r", Inputs: []string{},
		Given: []Given{{Step: "build", Path: "dist", Digest: "d"}}}}}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
