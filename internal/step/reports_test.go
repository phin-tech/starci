package step

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

const junit = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="unit" file="tests/test_math.py" tests="4">
    <testcase name="test_adds" classname="tests.test_math" time="0.012"/>
    <testcase name="test_divides" classname="tests.test_math" time="0.5">
      <failure message="ZeroDivisionError: division by zero" type="ZeroDivisionError">Traceback (most recent call last):
  File "tests/test_math.py", line 9</failure>
    </testcase>
    <testcase name="test_slow" classname="tests.test_math" time="2"><skipped/></testcase>
    <testsuite name="nested">
      <testcase name="test_inner" classname="tests.test_inner" file="tests/test_inner.py" time="0.1">
        <error type="RuntimeError">boom
and more</error>
      </testcase>
    </testsuite>
  </testsuite>
</testsuites>`

func TestAReportIsRead(t *testing.T) {
	cases, err := parseReport(strings.NewReader(junit))
	if err != nil {
		t.Fatal(err)
	}
	want := []plan.TestCase{
		{Name: "test_adds", Class: "tests.test_math", File: "tests/test_math.py", Seconds: 0.012, Status: "passed"},
		{Name: "test_divides", Class: "tests.test_math", File: "tests/test_math.py", Seconds: 0.5, Status: "failed", Message: "ZeroDivisionError: division by zero"},
		{Name: "test_slow", Class: "tests.test_math", File: "tests/test_math.py", Seconds: 2, Status: "skipped"},
		{Name: "test_inner", Class: "tests.test_inner", File: "tests/test_inner.py", Seconds: 0.1, Status: "failed", Message: "boom"},
	}
	if len(cases) != len(want) {
		t.Fatalf("%d cases, want %d: %+v", len(cases), len(want), cases)
	}
	for i := range want {
		if cases[i] != want[i] {
			t.Errorf("case %d = %+v\nwant     %+v", i, cases[i], want[i])
		}
	}

	// One suite on its own, as Go's and others' writers leave it.
	alone, err := parseReport(strings.NewReader(`<testsuite name="pkg"><testcase name="TestA" classname="pkg" time="1"/></testsuite>`))
	if err != nil || len(alone) != 1 || alone[0].Name != "TestA" || alone[0].Class != "pkg" || alone[0].Display() != "pkg.TestA" {
		t.Errorf("a lone suite = %+v, %v", alone, err)
	}
	if _, err := parseReport(strings.NewReader("this is not xml")); err == nil {
		t.Error("something that is not XML was read as a report")
	}
	// A report that would have the reader fetch or expand something.
	if _, err := parseReport(strings.NewReader(`<!DOCTYPE x [<!ENTITY a "aaaa">]><testsuite><testcase name="&a;"/></testsuite>`)); err == nil {
		t.Error("a report that declares its own entities was read")
	}
}

func report(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReportsAreAddedUpFailuresFirst(t *testing.T) {
	dir := t.TempDir()
	report(t, dir, "a.xml", junit)
	report(t, dir, "out/b.xml", `<testsuite><testcase name="ok" classname="b"/><testcase name="bad" classname="b"><failure message="no"/></testcase></testsuite>`)
	var warned []string
	tests := readReports(dir, []string{"a.xml", "out/b.xml", "missing.xml"}, func(l string) { warned = append(warned, l) })
	if tests == nil || tests.Passed != 2 || tests.Failed != 3 || tests.Skipped != 1 {
		t.Fatalf("%+v, want 2 passed, 3 failed, 1 skipped", tests)
	}
	if got := strings.Join(tests.FailedNames(), " "); got != "tests.test_math.test_divides tests.test_inner.test_inner b.bad" {
		t.Errorf("failed = %s", got)
	}
	for i, c := range tests.Cases {
		if (c.Status == "failed") != (i < 3) {
			t.Errorf("case %d is %s; want the three failures first", i, c.Status)
		}
	}
	// A report the step did not write is worth a word and nothing more.
	if len(warned) != 1 || !strings.Contains(warned[0], "missing.xml") || !strings.Contains(warned[0], "did not write it") {
		t.Errorf("warnings = %q", warned)
	}
	if got := tests.Naming(2); got != "tests.test_math.test_divides, tests.test_inner.test_inner and 1 more" {
		t.Errorf("Naming = %q", got)
	}

	// Nothing read at all is no summary, not an empty one.
	if got := readReports(dir, []string{"missing.xml"}, func(string) {}); got != nil {
		t.Errorf("with no report readable = %+v, want none", got)
	}
	report(t, dir, "bad.xml", "<testsuite><unclosed>")
	warned = nil
	if got := readReports(dir, []string{"bad.xml"}, func(l string) { warned = append(warned, l) }); got != nil || len(warned) != 1 || !strings.Contains(warned[0], "not JUnit XML") {
		t.Errorf("a broken report = %+v, warnings %q", got, warned)
	}
}

func TestMoreTestsThanAreCarried(t *testing.T) {
	var b strings.Builder
	b.WriteString("<testsuite>")
	for i := 0; i < plan.MaxTestCases+50; i++ {
		b.WriteString(`<testcase name="t" classname="c"/>`)
	}
	b.WriteString(`<testcase name="the one that failed" classname="c"><failure message="x"/></testcase></testsuite>`)
	dir := t.TempDir()
	report(t, dir, "big.xml", b.String())
	tests := readReports(dir, []string{"big.xml"}, func(string) {})
	if tests.Passed != plan.MaxTestCases+50 || tests.Failed != 1 || !tests.Truncated || len(tests.Cases) != plan.MaxTestCases {
		t.Fatalf("passed %d, failed %d, truncated %v, %d cases", tests.Passed, tests.Failed, tests.Truncated, len(tests.Cases))
	}
	// The failure was last in the report and is first of what is kept.
	if tests.Cases[0].Status != "failed" {
		t.Error("the failure was among what was left out")
	}
}

// A report is read on the worker's side of the workspace, as an output is.
func TestAReportIsNotReadThroughALink(t *testing.T) {
	outside := t.TempDir()
	report(t, outside, "secret.xml", `<testsuite><testcase name="the worker's secret" classname="c"><failure message="token=hunter2"/></testcase></testsuite>`)
	dir := t.TempDir()
	os.Symlink(filepath.Join(outside, "secret.xml"), filepath.Join(dir, "direct.xml"))
	os.Symlink(outside, filepath.Join(dir, "linked"))
	for _, rel := range []string{"direct.xml", "linked/secret.xml"} {
		var warned []string
		if got := readReports(dir, []string{rel}, func(l string) { warned = append(warned, l) }); got != nil {
			t.Errorf("%s was read: %+v", rel, got)
		}
		if len(warned) != 1 || !strings.Contains(warned[0], "link") || strings.Contains(warned[0], "hunter2") {
			t.Errorf("%s: warnings = %q", rel, warned)
		}
	}
}

func TestAStepsReportsComeBackWithItsResult(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"report.xml": junit})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}

	// Failed, which is when it matters most.
	got := run(t, cfg, r, sha, plan.Step{Name: "unit", Reports: []string{"out/junit.xml"}, Cache: false, Run: "mkdir out && cp report.xml out/junit.xml && exit 1"})
	if got.Status != "failed" || got.Tests == nil || got.Tests.Failed != 2 || got.Tests.Passed != 1 {
		t.Fatalf("%+v, tests %+v; want the failed step to say which tests", got, got.Tests)
	}
	// Passed, and reused: a reused pass carries what it said when it passed.
	pass := plan.Step{Name: "ok", Reports: []string{"junit.xml"}, Cache: true,
		Run: `echo '<testsuite><testcase name="a" classname="c" time="0.25"/></testsuite>' > junit.xml`}
	first := run(t, cfg, r, sha, pass)
	again := run(t, cfg, r, sha, pass)
	if first.Tests == nil || first.Tests.Passed != 1 || !again.Cached || again.Tests == nil || again.Tests.Cases[0].Seconds != 0.25 {
		t.Errorf("first %+v, again %+v (cached %v)", first.Tests, again.Tests, again.Cached)
	}
	// A step that did not write its report passes all the same.
	if got := run(t, cfg, r, sha, plan.Step{Name: "none", Reports: []string{"nope.xml"}, Cache: false, Run: "true"}); got.Status != "passed" || got.Tests != nil {
		t.Errorf("%+v, want a pass with no summary", got)
	}
	// Asking for reports is a reason to run again: the earlier pass has none.
	pass.Reports = nil
	if got := run(t, cfg, r, sha, pass); got.Cached {
		t.Error("a pass with reports was reused for a step that names none")
	}
}
