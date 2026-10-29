package plan

import (
	"strings"
	"testing"
)

func TestAStepWithoutAnImageIsBare(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [
        step("bare", run = "go vet ./...", caches = {"mod": "GOMODCACHE"}),
        step("list", run = ["go", "test", "./..."]),
        step("image", image = "golang:1", run = "go vet ./..."),
    ]
`)
	bare, list, img := p.Steps[0], p.Steps[1], p.Steps[2]
	if !bare.Bare() || img.Bare() {
		t.Fatalf("bare = %v, %v; want true, false", bare.Bare(), img.Bare())
	}
	if strings.Join(list.Argv, " ") != "go test ./..." || list.Run != "" {
		t.Errorf("a list run = %q / %q", list.Argv, list.Run)
	}
	if got := list.Command(); got != "'go' 'test' './...'" {
		t.Errorf("command = %q", got)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestABareStepIsReusedOnlyWhenItSaysSo(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [
        step("unsaid", run = "make"),
        step("asked", run = "make", cache = True),
        step("image", image = "i", run = "make"),
    ]
`)
	want := map[string]bool{"unsaid": false, "asked": true, "image": true}
	for _, s := range p.Steps {
		if s.Cache != want[s.Name] {
			t.Errorf("step %q: cache = %v, want %v", s.Name, s.Cache, want[s.Name])
		}
	}
}

func TestWorkerIsNoLongerAKeyword(t *testing.T) {
	wantError(t, `
def ci(ctx):
    return [step("a", run = "r", worker = {"gpu": "yes"})]
`, "unexpected keyword argument \"worker\"")
}

func TestWhatABareStepCannotHave(t *testing.T) {
	cases := []struct{ name, args, want string }{
		{"packages", `packages = ["curl"]`, "packages need an image"},
		{"services", `services = [service("db", image = "postgres")]`, "services need an image"},
		{"a cache at a path", `caches = {"c": "/root/.cache"}`, "names the environment variable"},
		{"an empty argument", `run = ["go", ""]`, "empty argument"},
		{"an empty list", `run = []`, "must not be empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "def ci(ctx):\n    return [step(\"a\", " + c.args + ")]\n"
			if !strings.Contains(c.args, "run =") {
				src = "def ci(ctx):\n    return [step(\"a\", run = \"r\", " + c.args + ")]\n"
			}
			wantError(t, src, c.want)
		})
	}
}

func TestValidateHoldsABareStepToTheSameRules(t *testing.T) {
	p := &Plan{Steps: []Step{{Name: "a", Run: "r", Inputs: []string{"."}, Packages: []string{"curl"}}}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "packages need an image") {
		t.Fatalf("Validate() = %v", err)
	}
	p = &Plan{Steps: []Step{{Name: "a", Run: "r", Argv: []string{"x"}, Image: "i", Inputs: []string{"."}}}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("a step with both a command and a list: Validate() = %v", err)
	}
}

func TestAToolMayBeBuiltForAnotherSystem(t *testing.T) {
	sum := strings.Repeat("a", 64)
	p := mustEval(t, `
gh = tool("gh", url = "https://example.com/gh_{os}_{arch}.tgz", sha256 = {"arm64": "`+sum+`", "darwin/arm64": "`+sum+`"})
def ci(ctx):
    return [step("a", run = "gh", tools = [gh])]
`)
	gh := p.Steps[0].Tools[0]
	if _, ok := gh.Sum("linux/arm64"); !ok {
		t.Error("an architecture alone should stand for Linux")
	}
	if _, ok := gh.Sum("darwin/arm64"); !ok {
		t.Error("darwin/arm64 is not found")
	}
	if _, ok := gh.Sum("darwin/amd64"); ok {
		t.Error("darwin/amd64 was found, but is not listed")
	}
	wantError(t, `
gh = tool("gh", url = "https://example.com/gh", sha256 = {"windows/arm64": "`+sum+`"})
def ci(ctx):
    return []
`, "os/arch")
}
