package compile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

const bareScript = `
def ci(ctx):
    return [step("vet", run = "go vet ./...", cache = True, caches = {"mod": "GOMODCACHE"})]
`

func TestABareStepRunsOnTheRunnerItself(t *testing.T) {
	out := compileScript(t, bareScript, Options{})
	wants(t, out,
		"env -i",
		`GOMODCACHE="$RUNNER_TEMP/starci-cache/mod"`,
		`/bin/sh -c "$STARCI_RUN"`,
	)
	if strings.Contains(out, "docker run") {
		t.Errorf("a bare step is started in a container:\n%s", out)
	}

	p, err := plan.Eval("ci.star", []byte(bareScript), plan.Context{})
	if err != nil {
		t.Fatal(err)
	}
	circle, err := CircleCI(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"env -i", `/bin/sh -c "$STARCI_RUN"`} {
		if !strings.Contains(circle, f) {
			t.Errorf("the CircleCI configuration is missing %q:\n%s", f, circle)
		}
	}
	if strings.Contains(circle, "docker run") {
		t.Errorf("a bare step is started in a container:\n%s", circle)
	}
}

func TestABareStepWithoutTheNetworkIsNotCompiled(t *testing.T) {
	p, err := plan.Eval("ci.star", []byte(`
def ci(ctx):
    return [step("a", run = "true", network = False)]
`), plan.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GitHub(p, Options{}); err == nil || !strings.Contains(err.Error(), "cannot be kept off it") {
		t.Errorf("GitHub: %v", err)
	}
	if _, err := CircleCI(p, Options{}); err == nil || !strings.Contains(err.Error(), "cannot be kept off it") {
		t.Errorf("CircleCI: %v", err)
	}
}

// The shell a bare step compiles to is run, to see that it does what a
// machine would: the step's own environment, the machine's programs, its cache.
func TestTheShellOfABareStepRuns(t *testing.T) {
	s := plan.Step{Name: "s", Env: map[string]string{"MINE": "it's mine"}}
	cache := filepath.Join(t.TempDir(), "cache")
	lines := bareRun(s, nil, map[string]string{"MY_CACHE": cache})
	cmd := exec.Command("/bin/sh", "-c", strings.Join(lines, "\n"))
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "STARCI_TEST_SECRET=hunter2",
		`STARCI_RUN=test -z "$STARCI_TEST_SECRET" && test "$MINE" = "it's mine" && git --version >/dev/null && test -d "$MY_CACHE" && test -d "$HOME"`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\n\n%s", err, out, strings.Join(lines, "\n"))
	}
}
