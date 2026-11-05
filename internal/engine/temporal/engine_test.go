//go:build temporal

package temporal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/engine/enginetest"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

// These tests need a Temporal server. They run when
// STARCI_TEST_TEMPORAL_SERVER=1, connecting as the SDK's environment says
// (TEMPORAL_ADDRESS and the rest), each on task queues of its own so that no
// other machine takes its steps. Machines run in this process, or as this
// test binary run again (TestMain), so steps fetch from paths on this
// computer.

func dial(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("STARCI_TEST_TEMPORAL_SERVER") == "" {
		t.Skip("set STARCI_TEST_TEMPORAL_SERVER=1 and TEMPORAL_ADDRESS to run against a Temporal server")
	}
	if testing.Short() {
		t.Skip("runs steps; skipped with -short")
	}
	opts, err := envconfig.LoadDefaultClientOptions()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Dial(opts)
	if err != nil {
		t.Fatalf("connecting to Temporal: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func newEngine(t *testing.T, c client.Client) *Engine {
	return &Engine{Client: c, Queue: "starci-test-" + uuid.NewString()}
}

// sharedDir is a store every machine of a test reaches, as STARCI_STORE
// would be.
type sharedDir struct {
	step.FileStore
	step.FileBlobs
}

func newSharedDir(t *testing.T) sharedDir {
	dir := t.TempDir()
	return sharedDir{step.FileStore{Dir: filepath.Join(dir, "results")}, step.FileBlobs{Dir: filepath.Join(dir, "blobs")}}
}

// machine joins eng's pool in this process until the test ends.
func machine(t *testing.T, eng *Engine, name string, labels map[string]string, shared step.Store) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- eng.Serve(ctx, engine.Machine{
			Config: step.Config{Machine: name, StateDir: t.TempDir(), Bare: true, Shared: shared},
			Labels: labels,
			Slots:  4,
		})
	}()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(time.Minute):
			t.Error("the machine did not stop")
		}
	})
}

func newRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "start")
	return dir, git("rev-parse", "HEAD")
}

func runOn(t *testing.T, ctx context.Context, eng engine.Engine, src string) (engine.Result, error) {
	t.Helper()
	dir, sha := newRepo(t)
	c := plan.Context{Repo: "test/repo", SHA: sha, Ref: "main", Event: plan.EventManual}
	r := engine.Run{Context: c, CloneURL: dir}
	mode, err := plan.Mode("ci.star", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	r.Mode = mode
	if mode == plan.Basic {
		if r.Plan, err = plan.Eval("ci.star", []byte(src), c); err != nil {
			t.Fatal(err)
		}
	} else {
		r.Filename, r.Source = "ci.star", []byte(src)
	}
	return eng.Execute(ctx, r)
}

func mustRun(t *testing.T, eng engine.Engine, src string) engine.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := runOn(t, ctx, eng, src)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

func resultFor(r engine.Result, name string) plan.Result {
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	return plan.Result{}
}

// TestEngine puts the engine through the shared tests, with one machine that
// has a shared store.
func TestEngine(t *testing.T) {
	eng := newEngine(t, dial(t))
	machine(t, eng, "test-"+uuid.NewString()[:8], nil, newSharedDir(t))
	enginetest.Run(t, eng)
}

// A step goes only to a machine that has what its on asks for.
func TestPlacement(t *testing.T) {
	eng := newEngine(t, dial(t))
	shared := newSharedDir(t)
	plain, gpu := "plain-"+uuid.NewString()[:8], "gpu-"+uuid.NewString()[:8]
	machine(t, eng, plain, nil, shared)
	machine(t, eng, gpu, map[string]string{"gpu": "true"}, shared)

	for i := 0; i < 3; i++ { // either machine could take an unplaced step; only one may take these
		result := mustRun(t, eng, `
def ci(ctx):
    return [
        step("anywhere", run = "true", cache = False),
        step("needs gpu", run = "true", cache = False, on = {"gpu": "true"}),
        step("needs gpu here", run = "true", cache = False, on = {"gpu": "true", "os": "`+runtime.GOOS+`"}),
    ]
`)
		if !result.OK {
			t.Fatalf("run failed: %+v", result.Steps)
		}
		for _, name := range []string{"needs gpu", "needs gpu here"} {
			if got := resultFor(result, name).Machine; got != gpu {
				t.Errorf("%q ran on %s, not the machine with a gpu", name, got)
			}
		}
	}
}

// A machine with no shared store refuses a step that keeps or is handed
// something, and says why.
func TestArtifactsNeedASharedStore(t *testing.T) {
	eng := newEngine(t, dial(t))
	machine(t, eng, "alone-"+uuid.NewString()[:8], nil, nil)
	result := mustRun(t, eng, `
def ci(ctx):
    return [step("build", run = "mkdir out && echo made > out/app", keep = ["out"], cache = False)]
`)
	r := resultFor(result, "build")
	if result.OK || r.Status != "failed" || !strings.Contains(r.Detail, "STARCI_STORE") {
		t.Fatalf("a step that keeps something on a machine with no shared store: %s %q", r.Status, r.Detail)
	}
}

// Cancelling a run stops its steps, and their containers and services are
// gone when Execute returns.
func TestCancel(t *testing.T) {
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
	eng := newEngine(t, dial(t))
	name := "cancel-" + uuid.NewString()[:8]
	machine(t, eng, name, nil, newSharedDir(t))
	owned := "label=starci.machine=" + name
	containers := func() string {
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", owned).Output()
		return strings.TrimSpace(string(out))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		r   engine.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := runOn(t, ctx, eng, `
def ci(ctx):
    db = service("db", image = "alpine:3", args = ["sleep", "600"])
    return [step("forever", image = "alpine:3", run = "sleep 600", cache = False, services = [db])]
`)
		done <- outcome{r, err}
	}()

	deadline := time.Now().Add(2 * time.Minute)
	for strings.Count(containers(), "\n") < 1 { // the service and the step: two lines, one newline
		if time.Now().After(deadline) {
			t.Fatal("the step's containers never started")
		}
		time.Sleep(time.Second)
	}
	cancelled := time.Now()
	cancel()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("Execute: %v", o.err)
		}
		if o.r.OK || o.r.Error != "cancelled" {
			t.Errorf("a cancelled run says ok=%v error=%q", o.r.OK, o.r.Error)
		}
		t.Logf("stopped in %s", time.Since(cancelled).Round(time.Second))
	case <-time.After(2 * time.Minute):
		t.Fatal("Execute did not return after the run was cancelled")
	}
	if left := containers(); left != "" {
		t.Errorf("containers outlived the cancelled run:\n%s", left)
	}
	if out, _ := exec.Command("docker", "network", "ls", "-q", "--filter", owned).Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("networks outlived the cancelled run:\n%s", out)
	}
}

// A machine killed partway through a step of an advanced script: the run
// resumes on another, steps that had finished are not run again, and the run
// passes.
func TestKilledMachine(t *testing.T) {
	eng := newEngine(t, dial(t))
	marks := t.TempDir()
	mark := func(name string) string { return filepath.Join(marks, name) }
	count := func(name string) int {
		b, _ := os.ReadFile(mark(name))
		return strings.Count(string(b), "\n")
	}
	pause := "23.4" + uuid.NewString()[:4] // unique, to find its process
	pause = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == '.' {
			return r
		}
		return '1'
	}, pause)

	// The first machine is this test binary run again, so that it can be
	// killed outright.
	doomed := exec.Command(os.Args[0], "-test.run=^$")
	doomed.Env = append(os.Environ(),
		"STARCI_TEST_MACHINE=doomed-"+uuid.NewString()[:8],
		"STARCI_TEST_MACHINE_QUEUE="+eng.Queue,
		"STARCI_TEST_MACHINE_STATE="+t.TempDir())
	doomed.Stdout, doomed.Stderr = os.Stderr, os.Stderr
	if err := doomed.Start(); err != nil {
		t.Fatal(err)
	}
	defer doomed.Process.Kill()

	id := "starci-test-killed-" + uuid.NewString()
	eng.ID = func(engine.Run) string { return id }
	type outcome struct {
		r   engine.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		r, err := runOn(t, ctx, eng, `
def pipeline(ctx):
    run(step("first", run = "echo x >> `+mark("first")+`", cache = False))
    run(step("slow", run = "echo x >> `+mark("slow")+`; sleep `+pause+`", cache = False))
    run(step("last", run = "echo x >> `+mark("last")+`", cache = False))
`)
		done <- outcome{r, err}
	}()

	deadline := time.Now().Add(2 * time.Minute)
	for count("slow") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slow step never started")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := doomed.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	doomed.Wait()
	time.Sleep(2 * time.Second)
	// The watchdog ends a killed machine's bare steps.
	if out, _ := exec.Command("pgrep", "-f", "sleep "+pause).Output(); len(out) > 0 {
		t.Errorf("the killed machine's step is still running: %s", out)
	}

	survivor := "survivor-" + uuid.NewString()[:8]
	machine(t, eng, survivor, nil, newSharedDir(t))
	var o outcome
	select {
	case o = <-done:
	case <-time.After(4 * time.Minute):
		t.Fatal("the run did not finish on the machine that was left")
	}
	if o.err != nil {
		t.Fatalf("Execute: %v", o.err)
	}
	if !o.r.OK {
		t.Fatalf("the run did not pass: %q %+v", o.r.Error, o.r.Steps)
	}
	if got := count("first"); got != 1 {
		t.Errorf("a step that had finished was run %d times", got)
	}
	if got := count("slow"); got != 2 {
		t.Errorf("the step the machine was killed in was run %d times, want 2 (killed, then again)", got)
	}
	if got := count("last"); got != 1 {
		t.Errorf("the step after was run %d times", got)
	}
	if got := resultFor(o.r, "slow").Machine; got != survivor {
		t.Errorf("the slow step's result is from %s, not the machine that was left", got)
	}
	replay(t, eng.Client, id, "killed-machine")
}

// TestReplay replays an advanced run's history against this code. With
// STARCI_RECORD_HISTORY=1 it keeps the history in testdata, where
// TestRecordedHistories replays it without a server.
func TestReplay(t *testing.T) {
	eng := newEngine(t, dial(t))
	machine(t, eng, "replay-"+uuid.NewString()[:8], nil, newSharedDir(t))
	id := "starci-test-replay-" + uuid.NewString()
	eng.ID = func(engine.Run) string { return id }
	result := mustRun(t, eng, `
def pipeline(ctx):
    probe = run(step("probe", run = "exit 1", cache = False), allow_failure = True)
    if not probe.ok:
        results = parallel([step("x", run = "true", cache = False), step("y", run = "true", cache = False)])
        run(step("after " + str(len(results)), run = "true", cache = False))
`)
	if !result.OK {
		t.Fatalf("run failed: %q %+v", result.Error, result.Steps)
	}
	replay(t, eng.Client, id, "advanced")
}

func replay(t *testing.T, c client.Client, id, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	h := &historypb.History{}
	it := c.GetWorkflowHistory(ctx, id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for it.HasNext() {
		e, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		h.Events = append(h.Events, e)
	}
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(runWorkflow, workflow.RegisterOptions{Name: workflowName})
	if err := r.ReplayWorkflowHistory(nil, h); err != nil {
		t.Errorf("replaying %s: %v", id, err)
	}
	if os.Getenv("STARCI_RECORD_HISTORY") != "" {
		b, err := protojson.MarshalOptions{Indent: "  "}.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", name+".history.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
