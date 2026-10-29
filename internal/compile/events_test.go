package compile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func compileEvents(t *testing.T, src string) string {
	t.Helper()
	out, err := GitHubEvents("ci.star", []byte(src), Options{Source: "ci.star"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// jobLine returns the `if:` of a job, or "" if it has none.
func jobCondition(t *testing.T, out, id string) string {
	t.Helper()
	i := strings.Index(out, "\n  "+id+":\n")
	if i < 0 {
		t.Fatalf("no job %q in:\n%s", id, out)
	}
	rest := out[i+1:]
	if j := strings.Index(rest, "    steps:\n"); j >= 0 {
		rest = rest[:j]
	}
	m := regexp.MustCompile(`(?m)^    if: \$\{\{ (.*) \}\}$`).FindStringSubmatch(rest)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestAScriptThatIgnoresEventsRunsOnAllOfThem(t *testing.T) {
	out := compileEvents(t, `
def ci(ctx):
    return [step("test", image = "alpine:3", run = "true")]
`)
	wants(t, out, "on:\n  push:\n    branches: ['**']\n    tags: ['**']\n  pull_request:\n  workflow_dispatch:\n")
	if cond := jobCondition(t, out, "test"); cond != "" {
		t.Errorf("a step that always runs was given the condition %q", cond)
	}
	if strings.Contains(out, "schedule:") {
		t.Error("a script with no schedule got a schedule trigger")
	}
}

func TestStepsForSomeEventsBecomeConditionalJobs(t *testing.T) {
	out := compileEvents(t, `
schedule("nightly", cron = "0 3 * * *")
schedule("weekly", cron = "0 4 * * 1")

def ci(ctx):
    test = step("test", image = "alpine:3", run = "true")
    steps = [test]
    if ctx.event == "tag":
        steps.append(step("publish", image = "alpine:3", run = "true", needs = [test]))
    if ctx.event == "push" and ctx.ref == "main":
        steps.append(step("deploy", image = "alpine:3", run = "true", needs = [test]))
    if ctx.event == "push":
        steps.append(step("on any push", image = "alpine:3", run = "true"))
    if ctx.event == "pull_request" and not ctx.pr.fork:
        steps.append(step("preview", image = "alpine:3", run = "true"))
    if ctx.event == "pull_request":
        steps.append(step("label", image = "alpine:3", run = "true"))
    if ctx.schedule == "nightly":
        steps.append(step("soak", image = "alpine:3", run = "true"))
    if ctx.event in ("schedule", "manual"):
        steps.append(step("audit", image = "alpine:3", run = "true"))
    return steps
`)
	wants(t, out, "  schedule:\n    - cron: \"0 3 * * *\"\n    - cron: \"0 4 * * 1\"\n  workflow_dispatch:\n")
	for id, want := range map[string]string{
		"test":        "",
		"publish":     "(github.event_name == 'push' && github.ref_type == 'tag')",
		"deploy":      "(github.event_name == 'push' && github.ref == 'refs/heads/main')",
		"on-any-push": "(github.event_name == 'push' && github.ref_type == 'branch')",
		// A script that refuses a stranger's pull request still refuses.
		"preview": "(github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)",
		"label":   "github.event_name == 'pull_request'",
		"soak":    "(github.event_name == 'schedule' && github.event.schedule == '0 3 * * *')",
		"audit":   "(github.event_name == 'schedule' && github.event.schedule == '0 3 * * *') || (github.event_name == 'schedule' && github.event.schedule == '0 4 * * 1') || github.event_name == 'workflow_dispatch'",
	} {
		if got := jobCondition(t, out, id); got != want {
			t.Errorf("job %s runs when\n  %s\nwant\n  %s", id, got, want)
		}
	}
	wants(t, notes(out),
		"The script looks at the branch. Whether it is main carries over",
		"Whether it comes from a fork carries over",
	)
}

// notes returns the header's notes as running text, undoing the wrapping.
func notes(out string) string {
	header := out[:strings.Index(out, "\nname: CI")]
	return strings.Join(strings.Fields(strings.ReplaceAll(header, "#", " ")), " ")
}

func TestAJobWaitingOnAConditionalOneStillRuns(t *testing.T) {
	out := compileEvents(t, `
def ci(ctx):
    test = step("test", image = "alpine:3", run = "true")
    if ctx.event == "tag":
        build = step("build", image = "alpine:3", run = "true")
        return [test, build, step("report", image = "alpine:3", run = "true", needs = [test, build])]
    return [test, step("report", image = "alpine:3", run = "true", needs = [test])]
`)
	// report needs build only for a tag. On a push, build is skipped, and a
	// job that needs a skipped one is skipped too unless it says otherwise.
	wants(t, out, "    needs: [test, build]\n    if: ${{ !failure() && !cancelled() }}\n")
}

func TestOnlyTheEventsThatLeadSomewhereTrigger(t *testing.T) {
	out := compileEvents(t, `
def ci(ctx):
    if ctx.event == "push" and ctx.ref == "main":
        return [step("deploy", image = "alpine:3", run = "true")]
    return []
`)
	wants(t, out, "on:\n  push:\n    branches: [\"main\"]\n\njobs:\n")
	for _, absent := range []string{"pull_request:", "tags:", "workflow_dispatch:", "schedule:"} {
		if strings.Contains(out, absent) {
			t.Errorf("the workflow triggers on %s though the script plans nothing for it", absent)
		}
	}
	if cond := jobCondition(t, out, "deploy"); cond != "" {
		t.Errorf("deploy has the condition %q, though it runs whenever the workflow does", cond)
	}
	wants(t, notes(out), "The script plans nothing for a push to another branch")
}

func TestAStepCannotBeTwoThings(t *testing.T) {
	_, err := GitHubEvents("ci.star", []byte(`
def ci(ctx):
    command = "make release" if ctx.event == "tag" else "make"
    return [step("build", image = "alpine:3", run = command)]
`), Options{})
	if err == nil || !strings.Contains(err.Error(), `step "build" is not the same for a tag as for a push to main`) {
		t.Fatalf("error = %v", err)
	}
}

func TestEveryBasicExampleCompilesForEvents(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "examples", "basic", "*.star"))
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		_, err = GitHubEvents(file, src, Options{})
		if err != nil && strings.Contains(err.Error(), "artifacts are not compiled") {
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(file), err)
		}
	}
}
