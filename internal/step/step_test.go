package step

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

const image = "alpine:3"

// These tests run real containers against a real git repository. They are
// skipped where that is not possible.
func needDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs containers; skipped with -short")
	}
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
}

// repo makes a git repository holding files and returns its path and the
// commit. Each call to commit adds another commit on top.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) commit(files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		path := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "change")
	return r.git("rev-parse", "HEAD")
}

func run(t *testing.T, cfg Config, r *repo, sha string, s plan.Step) plan.Result {
	t.Helper()
	if s.Image == "" {
		s.Image = image
	}
	if s.Inputs == nil {
		s.Inputs = []string{"."}
	}
	return Run(context.Background(), cfg, r.dir, sha, s, func(string) {})
}

func TestStepSeesOnlyItsInputs(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src/a.txt": "a", "docs/readme.md": "readme", "secret.txt": "s"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}

	got := run(t, cfg, r, sha, plan.Step{
		Name:   "look",
		Inputs: []string{"src"},
		Run:    "find . -type f | sort; test ! -e .git && test ! -e secret.txt && test ! -e docs",
	})
	if got.Status != "passed" {
		t.Fatalf("status = %s, detail: %s", got.Status, got.Detail)
	}
	if strings.TrimSpace(got.Output) != "./src/a.txt" {
		t.Errorf("the step saw:\n%s\nwant only ./src/a.txt", got.Output)
	}
}

func TestPassIsReusedUntilAnInputChanges(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	s := plan.Step{Name: "read", Inputs: []string{"src"}, Run: "cat src/a.txt", Cache: true}

	first := r.commit(map[string]string{"src/a.txt": "one\n", "README.md": "v1"})
	if got := run(t, cfg, r, first, s); got.Status != "passed" || got.Cached || got.Output != "one\n" {
		t.Fatalf("first run = %+v, want a fresh pass printing one", got)
	}

	// A commit that leaves the input alone reuses the pass, output included.
	docsOnly := r.commit(map[string]string{"README.md": "v2"})
	if got := run(t, cfg, r, docsOnly, s); !got.Cached || got.Output != "one\n" {
		t.Errorf("after a docs-only commit = %+v, want the earlier pass reused", got)
	}

	// Renaming the step is not a reason to run it again.
	renamed := s
	renamed.Name = "read the file"
	if got := run(t, cfg, r, docsOnly, renamed); !got.Cached || got.Name != "read the file" {
		t.Errorf("renamed step = %+v, want the earlier pass under the new name", got)
	}

	// Changing what it reads, how it runs, or its environment is.
	changed := r.commit(map[string]string{"src/a.txt": "two\n"})
	if got := run(t, cfg, r, changed, s); got.Cached || got.Output != "two\n" {
		t.Errorf("after the input changed = %+v, want a fresh run printing two", got)
	}
	other := s
	other.Run = "cat src/a.txt; true"
	if got := run(t, cfg, r, changed, other); got.Cached {
		t.Error("a different command reused another command's pass")
	}
	withEnv := s
	withEnv.Env = map[string]string{"MODE": "strict"}
	if got := run(t, cfg, r, changed, withEnv); got.Cached {
		t.Error("a different environment reused another environment's pass")
	}
}

func TestCacheFalseAlwaysRuns(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	s := plan.Step{Name: "live", Run: "true", Cache: false}
	run(t, cfg, r, sha, s)
	if got := run(t, cfg, r, sha, s); got.Cached {
		t.Error("a step with cache = False was reused")
	}
}

func TestFailuresAreReportedAndNeverReused(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	s := plan.Step{Name: "broken", Run: "echo about to fail; exit 7", Cache: true}

	for attempt := 1; attempt <= 2; attempt++ {
		got := run(t, cfg, r, sha, s)
		if got.Status != "failed" || got.Cached {
			t.Fatalf("attempt %d = %+v, want a fresh failure", attempt, got)
		}
		if !strings.Contains(got.Detail, "exit code 7") || !strings.Contains(got.Detail, "about to fail") {
			t.Errorf("detail = %q, want the exit code and the output", got.Detail)
		}
		if got.Output != "about to fail\n" {
			t.Errorf("output = %q, want what the step printed", got.Output)
		}
	}
}

func TestStepsThatCannotStartFailClearly(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}

	cases := []struct {
		name string
		sha  string
		step plan.Step
		want string
	}{
		{"missing input", sha, plan.Step{Name: "s", Inputs: []string{"nope"}, Run: "true"}, `input "nope" matches nothing in this commit`},
		{"pattern matching nothing", sha, plan.Step{Name: "s", Inputs: []string{"**/*.py"}, Run: "true"}, `input "**/*.py" matches nothing in this commit`},
		{"unknown commit", strings.Repeat("0", 40), plan.Step{Name: "s", Run: "true"}, "fetch of"},
		{"unknown image", sha, plan.Step{Name: "s", Image: "starci.invalid/no-such-image:1", Run: "true"}, "cannot pull image"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := run(t, cfg, r, c.sha, c.step)
			if got.Status != "failed" || !strings.Contains(got.Detail, c.want) {
				t.Fatalf("result = %+v, want a failure mentioning %q", got, c.want)
			}
		})
	}
}

func TestCacheMountPersistsBetweenRuns(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	name := "test" + strings.ReplaceAll(filepath.Base(t.TempDir()), "_", "")
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", "starci-cache-"+name).Run() })

	s := plan.Step{
		Name:   "count",
		Run:    "echo x >> /cache/runs; wc -l < /cache/runs",
		Caches: map[string]string{name: "/cache"},
	}
	first, second := run(t, cfg, r, sha, s), run(t, cfg, r, sha, s)
	if strings.TrimSpace(first.Output) != "1" || strings.TrimSpace(second.Output) != "2" {
		t.Errorf("runs saw %q then %q in the cache, want 1 then 2", first.Output, second.Output)
	}
}

func TestCancelStopsTheContainer(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan plan.Result)
	go func() {
		done <- Run(ctx, cfg, r.dir, sha, plan.Step{Name: "slow", Image: image, Inputs: []string{"."}, Run: "echo up; sleep 120"}, func(line string) {
			if line == "up" {
				close(started)
			}
		})
	}()
	<-started
	cancel()
	got := <-done
	if got.Status != "failed" || got.Detail != "cancelled" {
		t.Errorf("result = %+v, want a cancelled failure", got)
	}
	out, _ := exec.Command("docker", "ps", "-q", "--filter", "name=starci-starci").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("a step container is still running after cancel: %s", out)
	}
}

func TestInputPatterns(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	s := plan.Step{
		Name:   "list",
		Inputs: []string{"src/**/*.py", "tests/test_*.py", "pyproject.toml"},
		Run:    "find . -type f | sort",
		Cache:  true,
	}

	first := r.commit(map[string]string{
		"src/app.py":          "a",
		"src/pkg/deep/mod.py": "b",
		"src/pkg/data.json":   "{}",
		"src/notes.md":        "n",
		"tests/test_app.py":   "t",
		"tests/helpers.py":    "h",
		"pyproject.toml":      "p",
		"README.md":           "r",
	})
	got := run(t, cfg, r, first, s)
	if got.Status != "passed" {
		t.Fatalf("status = %s, detail: %s", got.Status, got.Detail)
	}
	want := "./pyproject.toml\n./src/app.py\n./src/pkg/deep/mod.py\n./tests/test_app.py\n"
	if got.Output != want {
		t.Errorf("the step saw:\n%swant:\n%s", got.Output, want)
	}

	// A new file no pattern matches leaves the result standing.
	unmatched := r.commit(map[string]string{"src/pkg/more.json": "{}", "docs/guide.md": "g", "tests/conftest.py": "c"})
	if got := run(t, cfg, r, unmatched, s); !got.Cached {
		t.Errorf("after adding files no pattern matches = %+v, want the earlier pass reused", got)
	}

	// A new file a pattern does match is a new input, with no change to the script.
	matched := r.commit(map[string]string{"src/pkg/new.py": "n"})
	got = run(t, cfg, r, matched, s)
	if got.Cached || !strings.Contains(got.Output, "./src/pkg/new.py") {
		t.Errorf("after adding a matching file = %+v, want a fresh run that sees it", got)
	}

	// So is an edit to a matched file, and a deletion.
	edited := r.commit(map[string]string{"src/pkg/deep/mod.py": "changed"})
	if got := run(t, cfg, r, edited, s); got.Cached {
		t.Error("an edit to a matched file reused the earlier pass")
	}
}

func TestPatternStarStaysInOneDirectory(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src/top.py": "a", "src/sub/inner.py": "b"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	got := run(t, cfg, r, sha, plan.Step{Name: "list", Inputs: []string{"src/*.py"}, Run: "find . -type f | sort"})
	if strings.TrimSpace(got.Output) != "./src/top.py" {
		t.Errorf("src/*.py gave the step:\n%s\nwant only ./src/top.py", got.Output)
	}
}
