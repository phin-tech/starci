package plan

import "testing"

func TestAStepAsksToBeTriedAgain(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [step("flaky", image = "i", run = "r", retries = 2), step("plain", image = "i", run = "r")]
`)
	if p.Steps[0].Retries != 2 || p.Steps[1].Retries != 0 {
		t.Errorf("retries = %d and %d", p.Steps[0].Retries, p.Steps[1].Retries)
	}
	wantError(t, "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", retries = -1)]\n", "retries must be between 0 and 5")
	wantError(t, "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", retries = 6)]\n", "retries must be between 0 and 5")
	bad := Plan{Steps: []Step{{Name: "a", Image: "i", Run: "r", Inputs: []string{"."}, Retries: 9}}}
	if err := bad.Validate(); err == nil {
		t.Error("a plan with nine retries is valid")
	}
}

func TestAScriptSeesHowManyTriesAStepTook(t *testing.T) {
	r := &fakeRunner{outcomes: map[string][]Result{
		"flaky": {{Status: "passed", Attempts: 2, Flaky: true}},
		"solid": {{Status: "passed"}},
		"bad":   {{Status: "failed", Attempts: 3}},
	}}
	_, err := pipeline(t, helpers+`
def pipeline(ctx):
    flaky, solid, bad = run(s("flaky")), run(s("solid")), run(s("bad"), allow_failure = True)
    if not (flaky.flaky and flaky.attempts == 2):
        fail("flaky: %s %d" % (flaky.flaky, flaky.attempts))
    if solid.flaky or solid.attempts != 1:
        fail("solid: %s %d" % (solid.flaky, solid.attempts))
    if bad.flaky or bad.attempts != 3:
        fail("bad: %s %d" % (bad.flaky, bad.attempts))
`, r)
	if err != nil {
		t.Error(err)
	}
}
