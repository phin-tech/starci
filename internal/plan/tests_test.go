package plan

import (
	"reflect"
	"strings"
	"testing"
)

func TestAStepNamesItsReports(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [step("unit", image = "i", run = "r", reports = ["out/junit.xml", "junit.xml", "out/junit.xml"])]
`)
	if want := []string{"junit.xml", "out/junit.xml"}; !reflect.DeepEqual(p.Steps[0].Reports, want) {
		t.Errorf("reports = %v, want %v", p.Steps[0].Reports, want)
	}
	for _, bad := range []string{`"../up.xml"`, `"/abs.xml"`, `"*.xml"`, `1`} {
		wantError(t, "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", reports = ["+bad+"])]\n", "reports must be files inside the working directory")
	}
}

// The README's rerun-what-failed, from the tests themselves and not from
// what a step printed.
func TestAScriptRerunsTheTestsThatFailed(t *testing.T) {
	r := &fakeRunner{outcomes: map[string][]Result{
		"all tests": {{Status: "failed", Tests: &Tests{Passed: 1, Failed: 2, Cases: []TestCase{
			{Name: "test_a", File: "tests/a.py", Status: "failed", Message: "boom", Seconds: 1.5},
			{Name: "test_b", File: "tests/b.py", Status: "failed"},
			{Name: "test_c", File: "tests/c.py", Status: "passed", Seconds: 0.5},
		}}}},
		"again test_a": {{Status: "passed"}},
		"again test_b": {{Status: "failed"}},
	}}
	_, err := pipeline(t, helpers+`
def pipeline(ctx):
    first = run(s("all tests"), allow_failure = True)
    if len(first.tests.passed) != 1 or first.tests.count != 3 or first.tests.failed[0].seconds != 1.5 or first.tests.failed[0].message != "boom":
        fail("the tests were not as reported")
    again = parallel([s("again " + t.name) for t in first.tests.failed], allow_failure = True)
    still = [r.name for r in again if not r.ok]
    if still:
        fail("failed twice: " + ", ".join(still))
`, r)
	if got := r.trace(); got != "all tests > again test_a+again test_b" {
		t.Errorf("ran %s", got)
	}
	if err == nil || !strings.Contains(err.Error(), "failed twice: again test_b") {
		t.Errorf("err = %v", err)
	}

	// A step with no reports has no tests, and a script need not ask first.
	if _, err := pipeline(t, helpers+`
def pipeline(ctx):
    r = run(s("plain"))
    if r.tests.failed or r.tests.passed or r.tests.count != 0:
        fail("tests from nowhere")
`, &fakeRunner{}); err != nil {
		t.Error(err)
	}
}

func TestNamingFailedTests(t *testing.T) {
	var none *Tests
	if none.Naming(3) != "" || (&Tests{Passed: 4}).Naming(3) != "" {
		t.Error("tests were named when none failed")
	}
	// More failed than are carried by name.
	if got := (&Tests{Failed: 40}).Naming(3); got != "40 tests" {
		t.Errorf("%q", got)
	}
}
