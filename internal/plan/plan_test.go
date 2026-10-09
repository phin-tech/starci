package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

var ctx = Context{
	Repo:         "sam/example",
	SHA:          "abc123",
	Ref:          "refs/heads/main",
	ChangedFiles: []string{"ui/src/app.ts", "README.md", "sidecar/main.go", "ui/package.json"},
}

func eval(t *testing.T, src string) (*Plan, error) {
	t.Helper()
	return Eval("ci.star", []byte(src), ctx)
}

func mustEval(t *testing.T, src string) *Plan {
	t.Helper()
	p, err := eval(t, src)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return p
}

func wantError(t *testing.T, src, fragment string) {
	t.Helper()
	_, err := eval(t, src)
	if err == nil {
		t.Fatalf("Eval succeeded, want an error containing %q", fragment)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error = %q, want it to contain %q", err, fragment)
	}
}

func TestStepsAndNeeds(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    build = step("build", image = "node:22", run = "make build", env = {"CI": "1"})
    test = step("test", image = "node:22", run = "make test", needs = [build])
    lint = step("lint", image = "node:22", run = "make lint", needs = ["build"])
    return [build, test, lint]
`)
	if len(p.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(p.Steps))
	}
	if p.Steps[0].Env["CI"] != "1" {
		t.Errorf("build env = %v, want CI=1", p.Steps[0].Env)
	}
	for _, i := range []int{1, 2} {
		if got := p.Steps[i].Needs; len(got) != 1 || got[0] != "build" {
			t.Errorf("step %q needs = %v, want [build]", p.Steps[i].Name, got)
		}
	}
	if p.Repo != ctx.Repo || p.SHA != ctx.SHA {
		t.Errorf("plan carries repo %q sha %q, want %q %q", p.Repo, p.SHA, ctx.Repo, ctx.SHA)
	}
}

// The reason to write CI in a language at all: the graph follows the inputs.
func TestGraphFollowsChangedDirs(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [step("test-" + d, image = "alpine", run = "true") for d in ctx.changed_dirs if d != "."]
`)
	var names []string
	for _, s := range p.Steps {
		names = append(names, s.Name)
	}
	if got, want := strings.Join(names, ","), "test-sidecar,test-ui"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
}

func TestSameInputSamePlan(t *testing.T) {
	src := `
def ci(ctx):
    steps = {}
    for f in ctx.changed_files:
        steps[f] = step("check " + f, image = "alpine", run = "echo " + f)
    return steps.values()
`
	first, _ := json.Marshal(mustEval(t, src))
	for i := 0; i < 20; i++ {
		again, _ := json.Marshal(mustEval(t, src))
		if string(again) != string(first) {
			t.Fatalf("run %d produced a different plan:\n%s\nvs\n%s", i, again, first)
		}
	}
}

func TestInputOrderDoesNotChangeThePlan(t *testing.T) {
	src := `
def ci(ctx):
    return [step(f, image = "alpine", run = "true") for f in ctx.changed_files]
`
	a, _ := Eval("ci.star", []byte(src), Context{ChangedFiles: []string{"b", "a", "c"}})
	b, _ := Eval("ci.star", []byte(src), Context{ChangedFiles: []string{"c", "b", "a"}})
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("plans differ by input order:\n%s\n%s", ja, jb)
	}
}

func TestSandbox(t *testing.T) {
	t.Run("load is refused", func(t *testing.T) {
		wantError(t, `load("other.star", "x")
def ci(ctx):
    return []
`, "not supported")
	})
	t.Run("no file access", func(t *testing.T) {
		wantError(t, `
def ci(ctx):
    open("/etc/passwd")
    return []
`, "undefined: open")
	})
	t.Run("ctx is read-only", func(t *testing.T) {
		wantError(t, `
def ci(ctx):
    ctx.changed_files.append("x")
    return []
`, "frozen")
	})
	t.Run("a runaway script is stopped", func(t *testing.T) {
		wantError(t, `
def ci(ctx):
    x = 0
    for i in range(1000000000):
        x += i
    return []
`, "too many steps")
	})
}

func TestInvalidPlans(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"no ci function", `x = 1`, "must define a function ci(ctx)"},
		{"not a list", `
def ci(ctx):
    return 3
`, "must return a list of steps"},
		{"not steps", `
def ci(ctx):
    return ["build"]
`, "must return only steps"},
		{"duplicate names", `
def ci(ctx):
    return [step("a", image = "i", run = "r"), step("a", image = "i", run = "r")]
`, `two steps are named "a"`},
		{"needs a step that was not returned", `
def ci(ctx):
    a = step("a", image = "i", run = "r")
    return [step("b", image = "i", run = "r", needs = [a])]
`, `step "b" needs "a"`},
		{"cycle", `
def ci(ctx):
    return [
        step("a", image = "i", run = "r", needs = ["b"]),
        step("b", image = "i", run = "r", needs = ["a"]),
    ]
`, "cycle: a -> b -> a"},
		{"empty command", `
def ci(ctx):
    return [step("a", image = "i", run = "")]
`, "must not be empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantError(t, c.src, c.want) })
	}
}

func TestInputsAndCaches(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [
        step("default", image = "i", run = "r"),
        step("narrow", image = "i", run = "r", inputs = ["src/", "bun.lock", "./src", "test"]),
        step("everything", image = "i", run = "r", inputs = ["src", "."]),
        step("live", image = "i", run = "r", cache = False, caches = {"bun": "/root/.bun/install/cache"}),
    ]
`)
	want := map[string]string{"default": ".", "narrow": "bun.lock,src,test", "everything": ".", "live": "."}
	for _, s := range p.Steps {
		if got := strings.Join(s.Inputs, ","); got != want[s.Name] {
			t.Errorf("step %q inputs = %q, want %q", s.Name, got, want[s.Name])
		}
	}
	if !p.Steps[0].Cache || p.Steps[3].Cache {
		t.Errorf("cache flags = %v, %v; want true, false", p.Steps[0].Cache, p.Steps[3].Cache)
	}
	if got := p.Steps[3].Caches["bun"]; got != "/root/.bun/install/cache" {
		t.Errorf("cache mount = %q", got)
	}
}

func TestInvalidInputsAndCaches(t *testing.T) {
	cases := []struct{ name, args, want string }{
		{"escapes the repository", `inputs = ["../secrets"]`, "must be a path inside the repository"},
		{"absolute", `inputs = ["/etc"]`, "must be a path inside the repository"},
		{"empty list", `inputs = []`, "must not be empty"},
		{"not a path", `inputs = [3]`, "inputs must be paths"},
		{"relative cache path", `caches = {"bun": "cache"}`, "needs an absolute path"},
		{"cache name with a slash", `caches = {"../x": "/c"}`, "may only use letters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantError(t, "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", "+c.args+")]\n", c.want)
		})
	}
}
