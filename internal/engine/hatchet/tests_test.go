package hatchet

import (
	"encoding/json"
	"errors"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

// Which tests failed has to survive being carried in an error, with all an
// engine does to one on the way.
func TestAFailedStepsTestsSurviveTheError(t *testing.T) {
	tests := &plan.Tests{Passed: 3, Failed: 2, Skipped: 1, Cases: []plan.TestCase{
		{Name: "t.one", File: "t.py", Status: "failed", Message: `said "no"`, Seconds: 1.5},
		{Name: "t.two", Status: "failed"},
		{Name: "t.three", Status: "passed"},
	}}
	sent := stepFailed + "exit code 1 after 2.0s\nlast line" + testsTail(tests)
	// As Hatchet hands a child's error back: the tail of a JSON string.
	quoted, _ := json.Marshal("something went wrong: " + sent)
	detail, got := withoutTests(reason(errors.New(string(quoted))))
	if detail != "exit code 1 after 2.0s\nlast line" {
		t.Errorf("detail = %q", detail)
	}
	if got == nil || got.Passed != 3 || got.Failed != 2 || got.Skipped != 1 || len(got.Cases) != 2 || got.Cases[0] != tests.Cases[0] || !got.Truncated {
		t.Errorf("tests = %+v; want the counts, the two failures, and a mark that the rest were left behind", got)
	}
	// A failure with no reports is the sentence it always was.
	if detail, got := withoutTests("exit code 1"); detail != "exit code 1" || got != nil {
		t.Errorf("%q, %+v", detail, got)
	}
	// Cut short on the way, what is left of the tail is not shown to anyone.
	cut := sent[:len(sent)-7]
	if detail, got := withoutTests(reason(errors.New(cut))); got != nil || detail != "exit code 1 after 2.0s\nlast line" {
		t.Errorf("a tail that was cut short: %q, %+v", detail, got)
	}
	if testsTail(nil) != "" {
		t.Error("a step with no reports has a tail")
	}
}
