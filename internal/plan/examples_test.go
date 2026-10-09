package plan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The examples are documentation, and documentation that does not run is
// worse than none. Every script in examples/ is evaluated here.

func exampleFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", dir, "*.star"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found in examples/%s (%v)", dir, err)
	}
	return files
}

var exampleContexts = map[string]Context{
	"nothing known":   {Repo: "sam/app", SHA: "0123456789abcdef"},
	"a code change":   {Repo: "sam/app", SHA: "0123456789abcdef", Ref: "feature/x", ChangedFiles: []string{"src/app.py", "packages/api/src/main.ts", "ui/index.ts"}},
	"a docs change":   {Repo: "sam/app", SHA: "0123456789abcdef", Ref: "main", ChangedFiles: []string{"README.md", "docs/guide.md"}},
	"a shared change": {Repo: "sam/app", SHA: "0123456789abcdef", Ref: "main", ChangedFiles: []string{"libs/common/util.ts"}},
	"a release":       {Repo: "sam/app", SHA: "0123456789abcdef", Ref: "release/2.0", ChangedFiles: []string{"api/server.py"}},
}

func TestBasicExamplesProduceValidPlans(t *testing.T) {
	for _, file := range exampleFiles(t, "basic") {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(file), func(t *testing.T) {
			if mode, err := Mode(file, src); err != nil || mode != Basic {
				t.Fatalf("Mode = %q, %v; want basic", mode, err)
			}
			ran := false
			for name, c := range exampleContexts {
				p, err := Eval(file, src, c)
				if err != nil {
					t.Fatalf("for %s: %v", name, err)
				}
				if err := p.Validate(); err != nil {
					t.Fatalf("for %s: the plan is not valid: %v", name, err)
				}
				ran = ran || len(p.Steps) > 0
			}
			if !ran {
				t.Error("the example never produces a step for any commit tried")
			}
		})
	}
}

func TestBasicExamplesDoWhatTheySay(t *testing.T) {
	plan := func(file string, c Context) []string {
		t.Helper()
		path := filepath.Join("..", "..", "examples", "basic", file)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Eval(path, src, c)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var names []string
		for _, s := range p.Steps {
			names = append(names, s.Name)
		}
		return names
	}
	check := func(what string, got []string, want string) {
		t.Helper()
		if strings.Join(got, ", ") != want {
			t.Errorf("%s: steps = %q, want %q", what, strings.Join(got, ", "), want)
		}
	}

	check("docs-only on a docs commit", plan("docs-only.ci.star", exampleContexts["a docs change"]), "")
	check("docs-only on a code commit", plan("docs-only.ci.star", exampleContexts["a code change"]), "test")

	check("monorepo, one package touched", plan("monorepo.ci.star", exampleContexts["a code change"]), "test api")
	check("monorepo, shared library touched", plan("monorepo.ci.star", exampleContexts["a shared change"]), "test api, test web, test worker")

	check("branches on a feature branch", plan("branches.ci.star", exampleContexts["a code change"]), "test")
	check("branches on main", plan("branches.ci.star", exampleContexts["a docs change"]), "test, publish")
	check("branches on a release branch", plan("branches.ci.star", exampleContexts["a release"]), "test, changelog")

	check("helpers, one component touched", plan("helpers.ci.star", exampleContexts["a release"]), "api lint, api test, api build")
	if got := plan("matrix.ci.star", exampleContexts["nothing known"]); len(got) != 5 {
		t.Errorf("matrix: %d steps, want 5 (3 pythons x 2 databases, less one)", len(got))
	}
	check("polyglot, python and typescript touched", plan("polyglot.ci.star", exampleContexts["a code change"]), "python, typescript")
	check("terraform, no stack touched", plan("terraform.ci.star", exampleContexts["a code change"]), "fmt")
	check("terraform, two stacks touched", plan("terraform.ci.star", Context{ChangedFiles: []string{"stacks/prod/main.tf", "stacks/dev/vars.tf", "stacks/prod/out.tf", "stacks/README.md"}}), "fmt, validate dev, validate prod")
	if got := plan("go.ci.star", exampleContexts["nothing known"]); len(got) != 5 {
		t.Errorf("go: %d steps, want vet, test and three builds", len(got))
	}
}

func TestAdvancedExamples(t *testing.T) {
	failed, passed := Result{Status: "failed"}, Result{Status: "passed"}
	main := Context{Repo: "sam/app", SHA: "0123456789abcdef", Ref: "main", ChangedFiles: []string{"src/app.go"}}
	branch := Context{Repo: "sam/app", SHA: "0123456789abcdef", Ref: "feature/x", ChangedFiles: []string{"src/app.go"}}

	cases := []struct {
		file     string
		ctx      Context
		outcomes map[string][]Result
		want     string // the batches run, in order
		wantErr  string
	}{
		{
			file:     "fanout.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"list tests": {{Status: "passed", Output: "test_a.py\ntest_b.py\n"}}},
			want:     "list tests > pytest test_a.py+pytest test_b.py",
		},
		{
			file:     "retry-flaky.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"test browser": {failed, passed}},
			want:     "test unit+test api+test browser+test migrations > test browser > note flaky",
		},
		{
			file:     "retry-flaky.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"test api": {failed}},
			want:     "test unit+test api+test browser+test migrations > test api > test api",
			wantErr:  "still failing after 3 attempts: api",
		},
		{
			file:     "shards.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"count tests": {{Status: "passed", Output: "450\n"}}},
			want:     "count tests > tests 1/3+tests 2/3+tests 3/3",
		},
		{
			file:     "shards.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"count tests": {{Status: "passed", Output: "12\n"}}},
			want:     "count tests > tests 1/1",
		},
		{file: "deploy-gate.pipeline.star", ctx: branch, want: "lint+test"},
		{file: "deploy-gate.pipeline.star", ctx: main, want: "lint+test > deploy > smoke test"},
		{
			file:     "deploy-gate.pipeline.star",
			ctx:      main,
			outcomes: map[string][]Result{"smoke test": {failed}},
			want:     "lint+test > deploy > smoke test > roll back",
			wantErr:  "the smoke test failed; rolled back",
		},
		{
			file:     "deploy-gate.pipeline.star",
			ctx:      main,
			outcomes: map[string][]Result{"test": {failed}},
			want:     "lint+test",
			wantErr:  `step "test" failed`,
		},
		{
			file: "canary.pipeline.star",
			ctx:  main,
			want: "build > release to canary > check canary > release to early > check early > release to half > check half > release to everyone > check everyone",
		},
		{
			file:     "canary.pipeline.star",
			ctx:      main,
			outcomes: map[string][]Result{"check early": {failed}},
			want:     "build > release to canary > check canary > release to early > check early > withdraw",
			wantErr:  "error rate too high at 10% (early); withdrawn",
		},
		{file: "canary.pipeline.star", ctx: branch, want: ""},
		{
			file:     "affected.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"affected packages": {{Status: "passed", Output: "./a\n./b\n./c\n./d\n./e\n./f\n"}}},
			want:     "affected packages > test batch 1+test batch 2",
		},
		{file: "affected.pipeline.star", ctx: Context{SHA: "0123456789abcdef"}, want: "test everything"},
		{
			file:     "tolerated.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"spelling": {failed}},
			want:     "bundle size+unused exports+spelling > types+tests > advisory",
		},
		{
			file:     "tolerated.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"tests": {failed}},
			want:     "bundle size+unused exports+spelling > types+tests",
			wantErr:  `step "tests" failed`,
		},
		{file: "first-failure.pipeline.star", ctx: branch, want: "format+lint > types+unit > integration"},
		{
			file:     "first-failure.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"format": {failed}},
			want:     "format+lint",
			wantErr:  `step "format" failed`,
		},
		{
			file: "coverage-gate.pipeline.star",
			ctx:  branch,
			outcomes: map[string][]Result{
				"test core": {{Status: "passed", Output: "....\n91\n"}},
				"test api":  {{Status: "passed", Output: "..\n84\n"}},
				"test cli":  {{Status: "passed", Output: ".\n77\n"}},
			},
			want: "test core+test api+test cli",
		},
		{
			file: "coverage-gate.pipeline.star",
			ctx:  branch,
			outcomes: map[string][]Result{
				"test core": {{Status: "passed", Output: "91\n"}},
				"test api":  {{Status: "passed", Output: "84\n"}},
				"test cli":  {{Status: "passed", Output: "52\n"}},
			},
			want:    "test core+test api+test cli",
			wantErr: "coverage below 70%: cli at 52%",
		},
		{
			file: "coverage-gate.pipeline.star",
			ctx:  branch,
			outcomes: map[string][]Result{
				"test core": {{Status: "passed", Output: "75\n"}},
				"test api":  {{Status: "passed", Output: "74\n"}},
				"test cli":  {{Status: "passed", Output: "73\n"}},
			},
			want:    "test core+test api+test cli",
			wantErr: "overall coverage is 74%, below 80%",
		},
		{
			file:     "wait-until-ready.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"probe 1": {failed}, "probe 2": {failed}},
			want:     "create preview > probe 1 > probe 2 > probe 3 > end to end > destroy preview",
		},
		{
			file: "wait-until-ready.pipeline.star",
			ctx:  branch,
			outcomes: map[string][]Result{
				"probe 1": {failed}, "probe 2": {failed}, "probe 3": {failed}, "probe 4": {failed}, "probe 5": {failed},
				"probe 6": {failed}, "probe 7": {failed}, "probe 8": {failed}, "probe 9": {failed}, "probe 10": {failed},
			},
			want:    "create preview > probe 1 > probe 2 > probe 3 > probe 4 > probe 5 > probe 6 > probe 7 > probe 8 > probe 9 > probe 10 > destroy preview",
			wantErr: "never became healthy after 10 tries",
		},
		{
			file:     "wait-until-ready.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"end to end": {failed}},
			want:     "create preview > probe 1 > end to end > destroy preview",
			wantErr:  "end to end tests failed against the preview",
		},
		{
			file:     "choose-by-step.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"classify": {{Status: "passed", Output: "ui\nmigrations unknown-word\n"}}},
			want:     "unit > classify > visual regression+migration dry run",
		},
		{file: "choose-by-step.pipeline.star", ctx: branch, want: "unit > classify"},
		{
			file:     "pi-gamebook.pipeline.star",
			ctx:      branch,
			outcomes: map[string][]Result{"list tests": {{Status: "passed", Output: "a.test.ts\nnotes.txt\nb.test.ts\n"}}, "test b.test.ts": {failed, passed}},
			want:     "check > list tests > test a.test.ts+test b.test.ts > test b.test.ts",
		},
	}

	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.file] = true
		name := c.file
		if c.wantErr != "" {
			name += " (" + c.wantErr + ")"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "examples", "advanced", c.file)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode, err := Mode(path, src); err != nil || mode != Advanced {
				t.Fatalf("Mode = %q, %v; want advanced", mode, err)
			}
			r := &fakeRunner{outcomes: c.outcomes}
			_, err = RunPipeline(context.Background(), path, src, c.ctx, r)
			if c.wantErr == "" && err != nil {
				t.Fatalf("pipeline: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
			if got := r.trace(); got != c.want {
				t.Errorf("ran  %s\nwant %s", got, c.want)
			}
		})
	}
	for _, file := range exampleFiles(t, "advanced") {
		if !covered[filepath.Base(file)] {
			t.Errorf("examples/advanced/%s has no case here", filepath.Base(file))
		}
	}
}
