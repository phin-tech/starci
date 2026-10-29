package compile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const argoInit = "registry.example/starci-init@sha256:0000"

func compileArgo(t *testing.T, src string) string {
	t.Helper()
	out, err := Argo("ci.star", []byte(src), Options{Source: "ci.star", InitImage: argoInit, BareImage: "registry.example/bare:1"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestArgoNeedsInitImage(t *testing.T) {
	_, err := Argo("ci.star", []byte(`
def ci(ctx):
    return [step("build", image = "alpine:3", run = "true")]
`), Options{})
	if err == nil || !strings.Contains(err.Error(), "--init-image") {
		t.Fatalf("error = %v", err)
	}
}

func TestArgoTasksDependOnWhatTheyNeed(t *testing.T) {
	out := compileArgo(t, `
def ci(ctx):
    return [
        step("build", image = "alpine:3", run = "make"),
        step("test", image = "alpine:3", run = "make test", needs = ["build"]),
    ]
`)
	wants(t, out, "kind: \"WorkflowTemplate\"", "- name: \"test\"\n            dependencies:\n              - \"build\"")
	wants(t, out, `image: "alpine:3"`)
	if strings.Contains(out, "when:") {
		t.Errorf("a step that runs on every event has a condition:\n%s", out)
	}
}

func TestArgoStepsForSomeEventsHaveConditions(t *testing.T) {
	out := compileArgo(t, `
def ci(ctx):
    steps = [step("build", image = "alpine:3", run = "true")]
    if ctx.event == "tag":
        steps.append(step("publish", image = "alpine:3", run = "true", needs = ["build"]))
    return steps
`)
	wants(t, out, `when: "{{=workflow.parameters.event == 'tag'}}"`)
}

func TestArgoInputsAreTheOnlyThingCheckedOut(t *testing.T) {
	out := compileArgo(t, `
def ci(ctx):
    return [step("lint", image = "alpine:3", run = "true", inputs = ["src", "go.mod"])]
`)
	wants(t, out, `git sparse-checkout set --no-cone '/go.mod' '/src'`, "rm -rf .git")
}

func TestArgoServicesAreNativeSidecars(t *testing.T) {
	out := compileArgo(t, `
def ci(ctx):
    return [step("test", image = "alpine:3", run = "true", services = [service("db", image = "postgres:16", ready = "pg_isready")])]
`)
	wants(t, out, `restartPolicy: "Always"`, "startupProbe:", `"pg_isready"`, `ip: "127.0.0.1"`, `- "db"`)
}

func TestArgoPlacement(t *testing.T) {
	out := compileArgo(t, `
def ci(ctx):
    return [step("train", image = "alpine:3", run = "true", on = {"gpu": "a100", "os": "linux"}, avoid = {"spot": "yes"}, prefer = {"zone": "a"})]
`)
	wants(t, out, `gpu: "a100"`, `kubernetes.io/os: "linux"`, `operator: "NotIn"`, `operator: "In"`, "preferredDuringSchedulingIgnoredDuringExecution")
}

func TestArgoRefusesWhatAPodCannotKeepTo(t *testing.T) {
	for name, tc := range map[string]struct{ script, want string }{
		"offline":  {`step("a", image = "alpine:3", run = "true", network = False)`, "network = False"},
		"packages": {`step("a", image = "debian:12", run = "true", packages = ["curl"])`, "packages"},
		"template": {`step("a", image = "alpine:3", run = "echo {{ nope }}")`, `"{{"`},
		"keep":     {`step("a", image = "alpine:3", run = "make", keep = ["out"])`, "artifacts"},
		"bare":     {`step("a", run = "true")`, "--bare-image"},
	} {
		_, err := Argo("ci.star", []byte("def ci(ctx):\n    return ["+tc.script+"]\n"), Options{InitImage: argoInit})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestEveryBasicExampleCompilesForArgo(t *testing.T) {
	scripts, _ := filepath.Glob(filepath.Join("..", "..", "examples", "basic", "*.star"))
	for _, file := range scripts {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Argo(file, src, Options{InitImage: argoInit, BareImage: "registry.example/bare:1"})
		if err != nil && !strings.Contains(err.Error(), "cannot carry over") {
			t.Errorf("%s: %v", filepath.Base(file), err)
		}
	}
}

// Argo's own decoder and validator, strictly, over what every example makes.
// They are a Go program that needs Argo's module, downloaded, so this runs
// when asked.
func TestArgoOwnValidatorAcceptsTheExamples(t *testing.T) {
	if os.Getenv("STARCI_TEST_ARGO") == "" {
		t.Skip("set STARCI_TEST_ARGO=1 to validate the examples with Argo's own code (needs the network)")
	}
	dir := t.TempDir()
	prog, err := os.ReadFile(filepath.Join("testdata", "argo", "validate.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"main.go": string(prog),
		"go.mod":  "module argocheck\n\ngo 1.26\n\nrequire github.com/argoproj/argo-workflows/v3 " + argoWorkflowsVersion + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", "argocheck", "."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	files := []string{}
	scripts, _ := filepath.Glob(filepath.Join("..", "..", "examples", "basic", "*.star"))
	for _, file := range scripts {
		src, _ := os.ReadFile(file)
		out, err := Argo(file, src, Options{InitImage: argoInit, BareImage: "registry.example/bare:1"})
		if err != nil {
			continue // refused, said so
		}
		at := filepath.Join(dir, strings.TrimSuffix(filepath.Base(file), ".ci.star")+".yml")
		if err := os.WriteFile(at, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, at)
	}
	if len(files) < 10 {
		t.Fatalf("only %d examples compiled", len(files))
	}
	cmd := exec.Command(filepath.Join(dir, "argocheck"), files...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
