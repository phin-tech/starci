package step

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

const testImage = "alpine:3"

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
		s.Image = testImage
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

func TestNetworkFalseLeavesOnlyLoopback(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	// With no network a container has the loopback interface and no other.
	s := plan.Step{Name: "sealed", Run: `test "$(ls /sys/class/net)" = lo`, Cache: true}
	if got := run(t, cfg, r, sha, s); got.Status != "failed" {
		t.Fatalf("a step with the network saw only loopback: %+v", got)
	}
	s.Offline = true
	if got := run(t, cfg, r, sha, s); got.Status != "passed" {
		t.Fatalf("a step with network = False saw more than loopback: %+v", got)
	}
}

func TestAPassWithTheNetworkIsNotReusedWithout(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	s := plan.Step{Name: "read", Run: "cat a.txt", Cache: true}
	run(t, cfg, r, sha, s)
	s.Offline = true
	if got := run(t, cfg, r, sha, s); got.Cached {
		t.Error("a pass with the network stood in for the step without it")
	}
	if got := run(t, cfg, r, sha, s); !got.Cached {
		t.Error("a pass without the network was not reused")
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
		done <- Run(ctx, cfg, r.dir, sha, plan.Step{Name: "slow", Image: testImage, Inputs: []string{"."}, Run: "echo up; sleep 120"}, func(line string) {
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

func TestPackagesAreInstalledOnce(t *testing.T) {
	needDocker(t)
	const base = "debian:12-slim"
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	s := plan.Step{Name: "use", Image: base, Packages: []string{"jq"}, Run: `echo '{"a": 7}' | jq .a`}

	first := run(t, cfg, r, sha, s)
	if first.Status != "passed" || strings.TrimSpace(first.Output) != "7" {
		t.Fatalf("first run = %+v, want jq to print 7", first)
	}
	env := func() string {
		out, _ := exec.Command("docker", "images", "--format", "{{.ID}} {{.CreatedAt}}", "starci-env").Output()
		return string(out)
	}
	built := env()
	if built == "" {
		t.Fatal("no environment image was built")
	}
	// The same packages on the same base are not installed again, even by a
	// step that otherwise differs.
	other := s
	other.Run = `echo '{"b": 8}' | jq .b`
	if got := run(t, cfg, r, sha, other); strings.TrimSpace(got.Output) != "8" {
		t.Fatalf("second step = %+v", got)
	}
	if env() != built {
		t.Error("the environment image was rebuilt for a second step with the same packages")
	}
	// Without the package, the same step cannot find the program.
	bare := s
	bare.Packages = nil
	if got := run(t, cfg, r, sha, bare); got.Status != "failed" {
		t.Error("jq ran without being installed; the test proves nothing")
	}
}

func TestPackagesNeedApt(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	got := run(t, cfg, r, sha, plan.Step{Name: "use", Image: testImage, Packages: []string{"jq"}, Run: "jq --version"})
	if got.Status != "failed" || !strings.Contains(got.Detail, "has no apt-get") {
		t.Fatalf("result = %+v, want a clear refusal on an image without apt", got)
	}
}

// memStore is a shared store that lives in the test, standing in for one on
// the network. It can be made to fail.
type memStore struct {
	mu      sync.Mutex
	results map[string]plan.Result
	gets    int
	down    bool
}

func (m *memStore) Get(_ context.Context, key string) (plan.Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	if m.down {
		return plan.Result{}, false
	}
	r, ok := m.results[key]
	return r, ok
}

func (m *memStore) Put(_ context.Context, key string, r plan.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errors.New("the shared store is unreachable")
	}
	if m.results == nil {
		m.results = map[string]plan.Result{}
	}
	m.results[key] = r
	return nil
}

func TestAPassOnOneMachineIsReusedOnAnother(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "hello\n"})
	shared := &memStore{}
	one := Config{Worker: "one", StateDir: t.TempDir(), Shared: shared}
	two := Config{Worker: "two", StateDir: t.TempDir(), Shared: shared}
	s := plan.Step{Name: "read", Run: "cat a.txt", Cache: true}

	if got := run(t, one, r, sha, s); got.Cached || got.Worker != "one" {
		t.Fatalf("on the first machine = %+v, want a fresh run there", got)
	}
	got := run(t, two, r, sha, s)
	if !got.Cached || got.Worker != "one" || got.Output != "hello\n" {
		t.Errorf("on the second machine = %+v, want the first machine's pass, output included", got)
	}

	// Having fetched it once, the second machine does not ask again.
	asked := shared.gets
	run(t, two, r, sha, s)
	if shared.gets != asked {
		t.Errorf("the shared store was asked again for a result this machine already holds")
	}

	// A machine with no shared store knows nothing about it.
	alone := Config{Worker: "three", StateDir: t.TempDir()}
	if got := run(t, alone, r, sha, s); got.Cached {
		t.Error("a machine without the shared store reused a result it could not have seen")
	}
}

func TestAnUnreachableSharedStoreCostsOnlyARerun(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	shared := &memStore{down: true}
	cfg := Config{Worker: "one", StateDir: t.TempDir(), Shared: shared}
	s := plan.Step{Name: "read", Run: "cat a.txt", Cache: true}

	var logged []string
	got := Run(context.Background(), cfg, r.dir, sha, plan.Step{Name: s.Name, Image: testImage, Inputs: []string{"."}, Run: s.Run, Cache: true},
		func(line string) { logged = append(logged, line) })
	if got.Status != "passed" {
		t.Fatalf("with the shared store down = %+v, want the step to run and pass", got)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "could not be stored") {
		t.Error("nothing said that the result could not be stored")
	}
	// It is still remembered on this machine.
	if got := run(t, cfg, r, sha, s); !got.Cached {
		t.Error("the result was not kept locally when the shared store was down")
	}
}

// The question a worker asks of a commit, asked of a checkout: needs git and
// nothing else.
func TestMissingInputsNamesWhatSelectsNothing(t *testing.T) {
	r := newRepo(t)
	r.commit(map[string]string{"src/a.go": "a", "docs/guide/intro.md": "i", "Makefile": "m"})
	// On disk but never added: not in any commit a worker would build.
	if err := os.WriteFile(filepath.Join(r.dir, "untracked.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		inputs []string
		want   string
	}{
		{[]string{"."}, ""},
		{[]string{"src", "Makefile", "docs/**/*.md"}, ""},
		{[]string{"src", "lib"}, "lib"},
		{[]string{"*.md", "**/*.md"}, "*.md"}, // * stays within a directory
		{[]string{"untracked.txt", "makefile"}, "untracked.txt makefile"},
	}
	for _, c := range cases {
		// Asked from a subdirectory: inputs are always from the top.
		got, err := MissingInputs(filepath.Join(r.dir, "src"), c.inputs)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("inputs %q: missing = %q, want %q", c.inputs, got, c.want)
		}
	}

	if _, err := MissingInputs(t.TempDir(), []string{"."}); err == nil {
		t.Error("a directory that is not in a repository was accepted")
	}
}

// web is a service that, once it has been up for delay, answers on port 8080
// with its host name, which is the name it was given.
func web(name, delay string) plan.Service {
	return plan.Service{
		Name: name, Image: testImage, Env: map[string]string{},
		Args:  []string{"sh", "-c", "sleep " + delay + "; exec nc -lk -p 8080 -e hostname"},
		Ready: "nc -w 2 127.0.0.1 8080 </dev/null | grep -q .",
	}
}

// leftovers lists what steps with services have left on this machine for the
// worker called name.
func leftovers(t *testing.T, name string) string {
	t.Helper()
	owned := "label=" + ownerLabel + "=" + name
	containers, _ := exec.Command("docker", "ps", "-aq", "--filter", owned).Output()
	networks, _ := exec.Command("docker", "network", "ls", "-q", "--filter", owned).Output()
	return strings.TrimSpace(string(containers) + string(networks))
}

func TestAStepReachesItsServicesByName(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test-" + t.Name(), StateDir: t.TempDir()}
	s := plan.Step{
		Name: "ask", Run: "nc -w 2 one 8080 </dev/null && nc -w 2 two 8080 </dev/null",
		// One of them is slow to start: the step waits for it.
		Services: []plan.Service{web("one", "0"), web("two", "2")},
	}
	got := run(t, cfg, r, sha, s)
	if got.Status != "passed" || got.Output != "one\ntwo\n" {
		t.Fatalf("result = %+v, want a pass that printed each service's name", got)
	}
	if left := leftovers(t, cfg.Worker); left != "" {
		t.Errorf("services or their network outlived the step: %s", left)
	}
}

func TestAStepWithoutTheNetworkStillReachesItsServices(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test-" + t.Name(), StateDir: t.TempDir()}
	s := plan.Step{
		Name: "sealed", Offline: true, Services: []plan.Service{web("one", "0")},
		// The service answers, and there is no way out: no default route.
		Run: "nc -w 2 one 8080 </dev/null && ! ip route | grep -q default",
	}
	if got := run(t, cfg, r, sha, s); got.Status != "passed" {
		t.Fatalf("result = %+v", got)
	}
	s.Offline = false
	if got := run(t, cfg, r, sha, s); got.Status != "failed" {
		t.Fatalf("a step with the network and services has no way out: %+v", got)
	}
}

func TestAServiceIsPartOfWhatAPassMeans(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test-" + t.Name(), StateDir: t.TempDir()}
	s := plan.Step{Name: "read", Run: "cat a.txt", Cache: true}
	run(t, cfg, r, sha, s)
	s.Services = []plan.Service{web("one", "0")}
	if got := run(t, cfg, r, sha, s); got.Cached {
		t.Error("a pass without the service stood in for the step with it")
	}
	if got := run(t, cfg, r, sha, s); !got.Cached {
		t.Error("a pass with the service was not reused")
	}
	s.Services[0].Env = map[string]string{"MODE": "other"}
	if got := run(t, cfg, r, sha, s); got.Cached {
		t.Error("a pass stood in for the step after its service's environment changed")
	}
}

func TestAServiceThatFailsFailsTheStepAndSaysWhy(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test-" + t.Name(), StateDir: t.TempDir()}

	dead := plan.Service{Name: "dead", Image: testImage, Args: []string{"sh", "-c", "echo no licence; exit 1"}, Ready: "true"}
	got := run(t, cfg, r, sha, plan.Step{Name: "a", Run: "true", Services: []plan.Service{dead}})
	if got.Status != "failed" || !strings.Contains(got.Detail, `"dead" stopped`) || !strings.Contains(got.Detail, "no licence") {
		t.Errorf("a service that exited: %+v", got)
	}

	old := readyWithin
	readyWithin = 2 * time.Second
	defer func() { readyWithin = old }()
	never := plan.Service{Name: "never", Image: testImage, Args: []string{"sh", "-c", "echo starting; sleep 600"}, Ready: "false"}
	got = run(t, cfg, r, sha, plan.Step{Name: "b", Run: "true", Services: []plan.Service{never}})
	if got.Status != "failed" || !strings.Contains(got.Detail, `"never" was not ready`) || !strings.Contains(got.Detail, "starting") {
		t.Errorf("a service that was never ready: %+v", got)
	}

	// Nor is one whose readiness command never comes back.
	never.Ready = "sleep 600"
	began := time.Now()
	got = run(t, cfg, r, sha, plan.Step{Name: "hung", Run: "true", Services: []plan.Service{never}})
	if got.Status != "failed" || !strings.Contains(got.Detail, `"never" was not ready`) || time.Since(began) > time.Minute {
		t.Errorf("a service whose readiness command hangs, after %s: %+v", time.Since(began).Round(time.Second), got)
	}

	got = run(t, cfg, r, sha, plan.Step{Name: "c", Run: "true", Services: []plan.Service{{Name: "gone", Image: "starci.invalid/no-such-image:1"}}})
	if got.Status != "failed" || !strings.Contains(got.Detail, `service "gone"`) {
		t.Errorf("a service whose image does not exist: %+v", got)
	}
	if left := leftovers(t, cfg.Worker); left != "" {
		t.Errorf("failed services were left behind: %s", left)
	}
}

func TestCancelStopsTheServices(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test-" + t.Name(), StateDir: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	up := make(chan struct{})
	done := make(chan plan.Result)
	go func() {
		s := plan.Step{Name: "slow", Image: testImage, Inputs: []string{"."}, Run: "echo up; sleep 120", Services: []plan.Service{web("one", "0")}}
		done <- Run(ctx, cfg, r.dir, sha, s, func(line string) {
			if line == "up" {
				close(up)
			}
		})
	}()
	<-up
	if leftovers(t, cfg.Worker) == "" {
		t.Fatal("nothing carries this worker's label while its step runs")
	}
	cancel()
	if got := <-done; got.Status != "failed" || got.Detail != "cancelled" {
		t.Errorf("result = %+v, want a cancelled failure", got)
	}
	if left := leftovers(t, cfg.Worker); left != "" {
		t.Errorf("services or their network are still there after cancel: %s", left)
	}
}

func TestSweepRemovesWhatAKilledWorkerLeft(t *testing.T) {
	needDocker(t)
	// What a worker killed partway through a step leaves: nothing ran stop.
	mine, other := "test-"+t.Name(), "test-"+t.Name()+"-other"
	var stops []func()
	for _, worker := range []string{mine, other} {
		s := plan.Step{Services: []plan.Service{web("one", "0")}}
		_, stop, err := startServices(context.Background(), "starci-sweep-"+worker, worker, s)
		stops = append(stops, stop)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()

	Sweep(mine)
	if left := leftovers(t, mine); left != "" {
		t.Errorf("after a sweep this worker still has: %s", left)
	}
	if leftovers(t, other) == "" {
		t.Error("a sweep for one worker removed another's services")
	}
}
