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

func TestNetworkIsOnUnlessAStepTurnsItOff(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [
        step("default", image = "i", run = "r"),
        step("sealed", image = "i", run = "r", network = False),
    ]
`)
	if p.Steps[0].Offline || !p.Steps[1].Offline {
		t.Errorf("offline = %v, %v; want false, true", p.Steps[0].Offline, p.Steps[1].Offline)
	}
	// A plan from before the field existed keeps the network.
	out, err := json.Marshal(p.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "offline") {
		t.Errorf("a step with the network mentions it in its plan: %s", out)
	}
	wantError(t, "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", network = \"none\")]\n", "network")
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

func TestLayers(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    a = step("a", image = "i", run = "r")
    b = step("b", image = "i", run = "r", needs = [a])
    c = step("c", image = "i", run = "r")
    d = step("d", image = "i", run = "r", needs = [b, c])
    return [d, c, b, a]
`)
	var got []string
	for _, layer := range p.Layers() {
		var names []string
		for _, s := range layer {
			names = append(names, s.Name)
		}
		got = append(got, strings.Join(names, ","))
	}
	if want := "c,a | b | d"; strings.Join(got, " | ") != want {
		t.Errorf("layers = %s, want %s", strings.Join(got, " | "), want)
	}
}

func TestValidateRejectsWhatEvalWould(t *testing.T) {
	good := mustEval(t, `
def ci(ctx):
    return [step("a", image = "i", run = "r")]
`)
	if err := good.Validate(); err != nil {
		t.Fatalf("a plan from Eval should validate: %v", err)
	}
	bad := []struct {
		name string
		edit func(*Step)
		want string
	}{
		{"input outside the repository", func(s *Step) { s.Inputs = []string{"../x"} }, "inside the repository"},
		{"no inputs", func(s *Step) { s.Inputs = nil }, "inputs must not be empty"},
		{"relative cache path", func(s *Step) { s.Caches = map[string]string{"c": "rel"} }, "needs an absolute path"},
		{"unknown need", func(s *Step) { s.Needs = []string{"zz"} }, `needs "zz"`},
		{"no command", func(s *Step) { s.Run = "" }, "must not be empty"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			p := *good
			p.Steps = append([]Step(nil), good.Steps...)
			c.edit(&p.Steps[0])
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestWorkerPlacement(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [step("train", image = "i", run = "r",
                 on = {"gpu": "true"}, prefer = {"speed": "fast"}, avoid = {"network": "home"})]
`)
	s := p.Steps[0]
	if s.On["gpu"] != "true" || s.Prefer["speed"] != "fast" || s.Avoid["network"] != "home" {
		t.Errorf("placement = on %v, prefer %v, avoid %v", s.On, s.Prefer, s.Avoid)
	}
	wantError(t, `
def ci(ctx):
    return [step("a", image = "i", run = "r", on = {"gpu": "true"}, avoid = {"gpu": "true"})]
`, `label "gpu" is in both on and avoid`)
}

func TestInputPatternsAreKeptAsWritten(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [step("a", image = "i", run = "r", inputs = ["src/**/*.py", "tests/test_*.py", "pyproject.toml"])]
`)
	if got, want := strings.Join(p.Steps[0].Inputs, " "), "pyproject.toml src/**/*.py tests/test_*.py"; got != want {
		t.Errorf("inputs = %s, want %s", got, want)
	}
}

const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestTools(t *testing.T) {
	p := mustEval(t, `
bun = tool(
    "bun",
    url = "https://example.com/bun-{arch}.zip",
    sha256 = {"amd64": "`+sum+`"},
    arch_names = {"amd64": "x64"},
    bin = ["bun-linux-{arch}"],
)
plain = tool("jq", url = "https://example.com/jq", sha256 = {"arm64": "`+sum+`"})

def ci(ctx):
    return [step("test", image = "i", run = "bun test", tools = [bun, plain])]
`)
	tools := p.Steps[0].Tools
	if len(tools) != 2 || tools[0].Name != "bun" || tools[0].ArchNames["amd64"] != "x64" || tools[0].Bin[0] != "bun-linux-{arch}" {
		t.Errorf("tools = %+v", tools)
	}
	if got := tools[1].Bin; len(got) != 1 || got[0] != "." {
		t.Errorf("default bin = %v, want [.]", got)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestInvalidTools(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"no checksum", `tool("a", url = "https://x/a", sha256 = {})`, "must give a checksum"},
		{"short checksum", `tool("a", url = "https://x/a", sha256 = {"amd64": "abc"})`, "64 lowercase hex digits"},
		{"unknown architecture", `tool("a", url = "https://x/a", sha256 = {"x86": "` + sum + `"})`, "keyed by amd64 or arm64"},
		{"not a url", `tool("a", url = "/tmp/a", sha256 = {"amd64": "` + sum + `"})`, "must be an http(s) address"},
		{"bin outside the archive", `tool("a", url = "https://x/a", sha256 = {"amd64": "` + sum + `"}, bin = ["../bin"])`, "inside the archive"},
		{"bad name", `tool("a b", url = "https://x/a", sha256 = {"amd64": "` + sum + `"})`, "may only use letters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantError(t, "t = "+c.src+"\ndef ci(ctx):\n    return []\n", c.want)
		})
	}
	wantError(t, `
a = tool("a", url = "https://x/a", sha256 = {"amd64": "`+sum+`"})
def ci(ctx):
    return [step("s", image = "i", run = "r", tools = [a, a])]
`, `tool "a" is listed twice`)
	wantError(t, `
def ci(ctx):
    return [step("s", image = "i", run = "r", tools = ["bun"])]
`, "tools must hold tool(...) values")
}

func TestPackages(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [step("s", image = "debian:12", run = "r", packages = ["libpq-dev", "git", "git", "curl=8.5.0-2"])]
`)
	if got, want := strings.Join(p.Steps[0].Packages, " "), "curl=8.5.0-2 git libpq-dev"; got != want {
		t.Errorf("packages = %s, want %s (sorted, once each)", got, want)
	}
	for _, bad := range []string{`"-o Dpkg::Options"`, `"git; rm -rf /"`, `"Git"`, `""`, `3`} {
		wantError(t, "def ci(ctx):\n    return [step(\"s\", image = \"i\", run = \"r\", packages = ["+bad+"])]\n", "packages must be apt package names")
	}
}

func TestEvalTrackedReportsWhatTheScriptRead(t *testing.T) {
	read := func(src string) string {
		t.Helper()
		_, got, err := EvalTracked("ci.star", []byte(src), ctx)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	if got := read("def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\")]\n"); got != "" {
		t.Errorf("a script that ignores ctx read %q", got)
	}
	if got := read("def ci(ctx):\n    if ctx.ref == \"main\" and ctx.changed_files:\n        return []\n    return []\n"); got != "ref" {
		t.Errorf("read = %q, want only ref: the second test is not reached off main", got)
	}
	if got := read("def ci(ctx):\n    return [step(d + ctx.sha, image = \"i\", run = \"r\") for d in ctx.changed_dirs]\n"); got != "changed_dirs,sha" {
		t.Errorf("read = %q, want changed_dirs,sha", got)
	}
}

func TestScriptsAreToldTheEvent(t *testing.T) {
	src := `
def ci(ctx):
    names = [ctx.event]
    if ctx.event == "tag":
        names.append("release " + ctx.tag)
    if ctx.pr:
        names.append("pr {} into {} from {}{}".format(ctx.pr.number, ctx.pr.base, ctx.pr.head, " (fork)" if ctx.pr.fork else ""))
    if ctx.schedule:
        names.append("schedule " + ctx.schedule)
    return [step(n, image = "i", run = "r") for n in names]
`
	cases := []struct {
		c    Context
		want string
	}{
		// With nothing said, a run is a push.
		{Context{}, "push"},
		{Context{Event: EventPush, Ref: "main"}, "push"},
		{Context{Event: EventTag, Tag: "v1.2.0"}, "tag, release v1.2.0"},
		{Context{Event: EventPullRequest, Ref: "fix", PR: &PullRequest{Number: 7, Base: "main", Head: "fix"}}, "pull_request, pr 7 into main from fix"},
		{Context{Event: EventPullRequest, PR: &PullRequest{Number: 8, Base: "main", Head: "patch", Fork: true}}, "pull_request, pr 8 into main from patch (fork)"},
		{Context{Event: EventSchedule, Schedule: "nightly"}, "schedule, schedule nightly"},
		{Context{Event: EventManual}, "manual"},
	}
	for _, c := range cases {
		p, err := Eval("ci.star", []byte(src), c.c)
		if err != nil {
			t.Fatalf("%+v: %v", c.c, err)
		}
		var names []string
		for _, s := range p.Steps {
			names = append(names, s.Name)
		}
		if got := strings.Join(names, ", "); got != c.want {
			t.Errorf("for %s: steps = %q, want %q", c.c.Event, got, c.want)
		}
	}
}

func TestSchedulesAreDeclaredAtTheTop(t *testing.T) {
	src := `
schedule("nightly", cron = "0 3 * * *")
schedule("weekly-audit", cron = "30  6 * * 1")

def ci(ctx):
    return []
`
	got, err := Schedules("ci.star", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// Unless a script says otherwise: a firing is skipped while the last run
	// is still going, and a missed one is not made up.
	if len(got) != 2 || got[0] != (Schedule{"nightly", "0 3 * * *", OverlapSkip, false}) || got[1] != (Schedule{"weekly-audit", "30 6 * * 1", OverlapSkip, false}) {
		t.Errorf("schedules = %+v", got)
	}
	set, err := Schedules("ci.star", []byte(`
schedule("backup", cron = "0 2 * * *", overlap = "replace", catch_up = True)
schedule("poll", cron = "*/5 * * * *", overlap = "allow")
def ci(ctx):
    return []
`))
	if err != nil || set[0] != (Schedule{"backup", "0 2 * * *", OverlapReplace, true}) || set[1].Overlap != OverlapAllow || set[1].CatchUp {
		t.Errorf("schedules = %+v, %v", set, err)
	}
	wantError(t, "schedule(\"x\", cron = \"0 3 * * *\", overlap = \"queue\")\ndef ci(ctx):\n    return []\n", `overlap is "skip", "replace" or "allow"`)
	// Declaring schedules does not get in the way of anything else.
	if _, err := Eval("ci.star", []byte(src), Context{}); err != nil {
		t.Errorf("Eval: %v", err)
	}
	if mode, err := Mode("ci.star", []byte(src)); err != nil || mode != Basic {
		t.Errorf("Mode = %q, %v", mode, err)
	}

	bad := []struct{ name, src, want string }{
		{"inside a function", "def ci(ctx):\n    schedule(\"late\", cron = \"0 3 * * *\")\n    return []\n", "declared at the top of the file"},
		{"too few fields", "schedule(\"x\", cron = \"0 3 * *\")\ndef ci(ctx):\n    return []\n", "cron needs five fields"},
		{"not cron", "schedule(\"x\", cron = \"0 3 * * $(reboot)\")\ndef ci(ctx):\n    return []\n", "is not a cron field"},
		{"twice", "schedule(\"x\", cron = \"0 3 * * *\")\nschedule(\"x\", cron = \"0 4 * * *\")\ndef ci(ctx):\n    return []\n", "declared twice"},
		{"bad name", "schedule(\"every night\", cron = \"0 3 * * *\")\ndef ci(ctx):\n    return []\n", "may only use letters"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) { wantError(t, c.src, c.want) })
	}
}

func TestToolsFindsEveryDeclaredTool(t *testing.T) {
	sum := strings.Repeat("a", 64)
	decl := func(name string) string {
		return `tool("` + name + `", url = "https://example.com/` + name + `", sha256 = {"amd64": "` + sum + `"})`
	}
	names := func(tools []Tool) string {
		var out []string
		for _, t := range tools {
			out = append(out, t.Name)
		}
		return strings.Join(out, " ")
	}

	// One no step uses, one only another branch uses, one made inside ci(),
	// and a helper that declares the same tool for every step it builds.
	basic := `
unused = ` + decl("unused") + `
release = ` + decl("release") + `
def lint(name):
    return step(name, image = "i", run = "r", tools = [` + decl("linter") + `])
def ci(ctx):
    steps = [lint("a"), lint("b"), step("t", image = "i", run = "r", tools = [` + decl("inside") + `])]
    if ctx.ref == "refs/heads/main":
        steps.append(step("release", image = "i", run = "r", tools = [release]))
    return steps
`
	tools, err := Tools(".ci.star", []byte(basic))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(tools), "unused release linter inside"; got != want {
		t.Errorf("tools = %q, want %q", got, want)
	}

	// An advanced script has no plan, but its top level is as readable.
	advanced := `
uv = ` + decl("uv") + `
def pipeline(ctx):
    run(step("t", image = "i", run = "r", tools = [uv]))
`
	tools, err = Tools(".ci.star", []byte(advanced))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(tools); got != "uv" {
		t.Errorf("tools = %q, want uv", got)
	}

	if _, err := Tools(".ci.star", []byte(`x = 1`)); err == nil || !strings.Contains(err.Error(), "must define ci(ctx) or pipeline(ctx)") {
		t.Errorf("err = %v, want a script with neither function refused", err)
	}
}

func TestServices(t *testing.T) {
	p := mustEval(t, `
db = service("postgres", image = "postgres:17", env = {"POSTGRES_PASSWORD": "pw"},
             ready = "pg_isready -h localhost")

def ci(ctx):
    return [
        step("plain", image = "i", run = "r"),
        step("tests", image = "i", run = "r", services = [
            db,
            service("store", image = "minio/minio", args = ["server", "/data"]),
        ]),
    ]
`)
	// A plan from before the field existed is unchanged.
	out, err := json.Marshal(p.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "services") {
		t.Errorf("a step with no services mentions them in its plan: %s", out)
	}
	got := p.Steps[1].Services
	if len(got) != 2 {
		t.Fatalf("services = %+v, want two", got)
	}
	if got[0].Name != "postgres" || got[0].Image != "postgres:17" || got[0].Env["POSTGRES_PASSWORD"] != "pw" || got[0].Ready != "pg_isready -h localhost" {
		t.Errorf("first service = %+v", got[0])
	}
	if strings.Join(got[1].Args, " ") != "server /data" {
		t.Errorf("args = %q", got[1].Args)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	one := func(services string) string {
		return "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", services = [" + services + "])]\n"
	}
	// The name is a host name.
	wantError(t, one(`service("Post_gres", image = "i")`), "host name")
	wantError(t, one(`service("db", image = "")`), "image")
	wantError(t, one(`service("db", image = "i", ready = "a\nb")`), "one line")
	wantError(t, one(`service("db", image = "i"), service("db", image = "j")`), "listed twice")
	wantError(t, one(`"postgres:17"`), "service(...)")

	// A file is one path in the repository, put at one place in the container.
	given := mustEval(t, one(`service("db", image = "i", ready_within = 300, files = {"./db/init.sql": "/docker-entrypoint-initdb.d/01.sql", "db/conf": "/etc/db"})`)).Steps[0].Services[0]
	if given.ReadyWithin != 300 || given.Files["db/init.sql"] != "/docker-entrypoint-initdb.d/01.sql" || strings.Join(given.FilePaths(), " ") != "db/conf db/init.sql" {
		t.Errorf("service = %+v", given)
	}
	wantError(t, one(`service("db", image = "i", files = {"db/*.sql": "/init"})`), "one file or directory")
	wantError(t, one(`service("db", image = "i", files = {"../secrets": "/init"})`), "inside the repository")
	wantError(t, one(`service("db", image = "i", files = {"db/init.sql": "init.sql"})`), "absolute path")
	wantError(t, one(`service("db", image = "i", ready_within = 7200)`), "at most an hour")

	// And a plan that arrives as JSON is held to the same.
	p.Steps[1].Services[1].Name = "postgres"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("Validate of a plan with one service name twice = %v", err)
	}
}
