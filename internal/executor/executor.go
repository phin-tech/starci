// Package executor runs a plan as Hatchet tasks.
//
// Two generic tasks, rather than one registered workflow per repository,
// because the shape of a run is only known once its script has been evaluated:
//
//	ci-step      runs one container against the inputs of one step
//	ci-pipeline  walks a plan and starts a ci-step child per step (basic mode)
//	ci-script    runs an advanced script, starting a ci-step child each time
//	             the script runs a step
package executor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"git.phin.tech/sam/starci/internal/plan"
)

const (
	StepTask     = "ci-step"
	PipelineTask = "ci-pipeline"
	ScriptTask   = "ci-script"

	// How much of a step's output is kept in its result, for a script to read.
	outputLimit = 64 * 1024

	// How much of a failed step's output is carried in its error, for the
	// caller. The full output is in the task's logs.
	tailLines = 40
)

type PipelineInput struct {
	Plan     plan.Plan `json:"plan"`
	CloneURL string    `json:"clone_url"`
	// Supersedes names what this run replaces: a new run with the same value
	// cancels any still in progress. A branch, so that a push makes the run
	// for the previous push pointless; or the commit, for a one-off run that
	// should not disturb anything.
	Supersedes string `json:"supersedes"`
}

type StepInput struct {
	CloneURL string    `json:"clone_url"`
	SHA      string    `json:"sha"`
	Step     plan.Step `json:"step"`
}

// ScriptInput is an advanced script and what it is being run for. The script
// travels as source: it is evaluated on a worker, as the run unfolds.
type ScriptInput struct {
	Filename   string       `json:"filename"`
	Source     string       `json:"source"`
	Context    plan.Context `json:"context"`
	CloneURL   string       `json:"clone_url"`
	Supersedes string       `json:"supersedes"`
}

type StepResult struct {
	Name    string  `json:"name"`
	Status  string  `json:"status"` // passed, failed or skipped
	Seconds float64 `json:"seconds"`
	Worker  string  `json:"worker"`
	// Output is what the step printed, up to outputLimit. It is part of a
	// stored result, so a step that is skipped still hands a script the
	// output it produced when it ran.
	Output string `json:"output,omitempty"`
	// Cached is true when an earlier passing run with the same key stood in
	// for this one.
	Cached bool   `json:"cached"`
	Detail string `json:"detail"`
}

type PipelineResult struct {
	OK    bool         `json:"ok"`
	Steps []StepResult `json:"steps"`
	// Error is why an advanced script ended early: a failed step it did not
	// allow for, its own fail(), or a mistake in the script.
	Error string `json:"error,omitempty"`
}

// Config is what a worker needs to know about the machine it is on.
type Config struct {
	// WorkerName is recorded in results, so a cached pass says where it ran.
	WorkerName string
	// StateDir holds passing results, one file per key. Per worker for now: a
	// result recorded on one machine is not seen by another.
	StateDir string
}

var (
	one              int32 = 1
	cancelInProgress       = types.CancelInProgress
	notEqual               = types.WorkerLabelComparator_NOT_EQUAL
)

// Tasks is the set of tasks a worker registers and a client triggers.
type Tasks struct {
	Step, Pipeline, Script *hatchet.StandaloneTask
}

func (t Tasks) All() []hatchet.WorkflowBase {
	return []hatchet.WorkflowBase{t.Step, t.Pipeline, t.Script}
}

// NewTasks defines the tasks on the client. Register them on a worker to
// execute runs, or use them from any process to trigger and await runs.
func NewTasks(client *hatchet.Client, cfg Config) Tasks {
	var step, pipeline *hatchet.StandaloneTask
	// A newer run for the same branch makes the one before it pointless.
	supersede := hatchet.WithConcurrency(&hatchet.Concurrency{
		Expression:    "input.supersedes",
		MaxRuns:       &one,
		LimitStrategy: &cancelInProgress,
	})
	step = client.NewStandaloneTask(StepTask,
		func(ctx hatchet.Context, in StepInput) (StepResult, error) { return runStep(ctx, cfg, in) },
		hatchet.WithExecutionTimeout(30*time.Minute),
	)
	pipeline = client.NewStandaloneDurableTask(PipelineTask,
		func(ctx hatchet.DurableContext, in PipelineInput) (PipelineResult, error) {
			return runPipeline(ctx, client, step, in)
		},
		hatchet.WithExecutionTimeout(time.Hour),
		supersede,
	)
	script := client.NewStandaloneDurableTask(ScriptTask,
		func(ctx hatchet.DurableContext, in ScriptInput) (PipelineResult, error) {
			return runScript(ctx, client, step, in)
		},
		hatchet.WithExecutionTimeout(time.Hour),
		supersede,
	)
	return Tasks{Step: step, Pipeline: pipeline, Script: script}
}

// runScript evaluates an advanced script, executing each run() and parallel()
// it makes as ci-step children.
//
// The script is not checkpointed; the children are. If this task is restarted
// partway, the script is evaluated again from the top, and each batch of
// children it asks for is answered from Hatchet's record of the run instead of
// being started a second time. That only works because the script asks for
// the same children in the same order when given the same answers, which a
// Starlark script cannot fail to do.
func runScript(ctx hatchet.DurableContext, client *hatchet.Client, step *hatchet.StandaloneTask, in ScriptInput) (PipelineResult, error) {
	runner := &childRunner{ctx: ctx, client: client, step: step, cloneURL: in.CloneURL, sha: in.Context.SHA}
	results, err := plan.RunPipeline(ctx, in.Filename, []byte(in.Source), in.Context, runner)
	if ctx.Err() != nil {
		return PipelineResult{}, hatchet.NewNonRetryableError(errors.New("cancelled"))
	}

	out := PipelineResult{OK: err == nil}
	if err != nil {
		out.Error = err.Error()
	}
	for _, r := range results {
		out.Steps = append(out.Steps, StepResult{
			Name: r.Name, Status: r.Status, Seconds: r.Seconds, Worker: r.Worker,
			Output: r.Output, Cached: r.Cached, Detail: r.Detail,
		})
	}
	return out, nil
}

// childRunner runs a script's steps as ci-step children of the script's task.
type childRunner struct {
	ctx      hatchet.DurableContext
	client   *hatchet.Client
	step     *hatchet.StandaloneTask
	cloneURL string
	sha      string
}

func (c *childRunner) Run(steps []plan.Step) ([]plan.Result, error) {
	single := plan.Plan{SHA: c.sha}
	inputs := make([]hatchet.RunManyOpt, len(steps))
	for i, s := range steps {
		single.Steps = []plan.Step{s}
		if err := single.Validate(); err != nil {
			return nil, err
		}
		inputs[i] = hatchet.RunManyOpt{Input: StepInput{CloneURL: c.cloneURL, SHA: c.sha, Step: s}}
		if labels := placement(s); len(labels) > 0 {
			inputs[i].Opts = append(inputs[i].Opts, hatchet.WithDesiredWorkerLabels(labels))
		}
	}
	refs, err := c.step.RunMany(c.ctx, inputs)
	if err != nil {
		return nil, fmt.Errorf("starting steps: %w", err)
	}
	settled := make(chan struct{})
	go cancelWithParent(c.ctx, c.client, refs, settled)
	defer close(settled)

	out := make([]plan.Result, len(steps))
	for i, s := range steps {
		r := await(&refs[i], s.Name)
		out[i] = plan.Result{
			Name: r.Name, Status: r.Status, Output: r.Output, Seconds: r.Seconds,
			Worker: r.Worker, Cached: r.Cached, Detail: r.Detail,
		}
	}
	return out, nil
}

func runPipeline(ctx hatchet.DurableContext, client *hatchet.Client, step *hatchet.StandaloneTask, in PipelineInput) (PipelineResult, error) {
	if err := in.Plan.Validate(); err != nil {
		return PipelineResult{}, hatchet.NewNonRetryableError(fmt.Errorf("invalid plan: %w", err))
	}
	results := map[string]StepResult{}

	// One layer at a time, children started in plan order. A step could start
	// the moment its own dependencies finish rather than when its whole layer
	// is ready; this trades that for a start order that never varies, which a
	// durable task needs in order to replay.
	for _, layer := range in.Plan.Layers() {
		var runnable []plan.Step
		var inputs []hatchet.RunManyOpt
		for _, s := range layer {
			var blocked []string
			for _, n := range s.Needs {
				if results[n].Status != "passed" {
					blocked = append(blocked, n)
				}
			}
			if len(blocked) > 0 {
				results[s.Name] = StepResult{Name: s.Name, Status: "skipped", Detail: "needs " + strings.Join(blocked, ", ")}
				continue
			}
			opt := hatchet.RunManyOpt{Input: StepInput{CloneURL: in.CloneURL, SHA: in.Plan.SHA, Step: s}}
			if labels := placement(s); len(labels) > 0 {
				opt.Opts = append(opt.Opts, hatchet.WithDesiredWorkerLabels(labels))
			}
			runnable = append(runnable, s)
			inputs = append(inputs, opt)
		}
		if len(runnable) == 0 {
			continue
		}

		refs, err := step.RunMany(ctx, inputs)
		if err != nil {
			return PipelineResult{}, fmt.Errorf("starting steps: %w", err)
		}
		settled := make(chan struct{})
		go cancelWithParent(ctx, client, refs, settled)
		for i, s := range runnable {
			results[s.Name] = await(&refs[i], s.Name)
		}
		close(settled)
		if ctx.Err() != nil {
			return PipelineResult{}, hatchet.NewNonRetryableError(errors.New("cancelled"))
		}
	}

	out := PipelineResult{OK: true}
	for _, s := range in.Plan.Steps {
		r := results[s.Name]
		out.OK = out.OK && r.Status == "passed"
		out.Steps = append(out.Steps, r)
	}
	return out, nil
}

// cancelWithParent cancels the steps a pipeline started if the pipeline itself
// is cancelled before they settle. Hatchet stops the pipeline but leaves its
// children running, and a superseded run's steps are exactly the work that
// should stop.
func cancelWithParent(ctx hatchet.DurableContext, client *hatchet.Client, refs []hatchet.WorkflowRunRef, settled <-chan struct{}) {
	select {
	case <-settled:
		return
	case <-ctx.Done():
	}
	var ids []uuid.UUID
	for _, ref := range refs {
		if id, err := uuid.Parse(ref.RunId); err == nil {
			ids = append(ids, id)
		}
	}
	// The pipeline's own context is finished; this needs one that is not.
	bg, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	_, _ = client.Runs().Cancel(bg, rest.V1CancelTaskRequest{ExternalIds: &ids})
}

// placement turns a step's worker, prefer and avoid into Hatchet's rules.
func placement(s plan.Step) map[string]*hatchet.DesiredWorkerLabel {
	labels := map[string]*hatchet.DesiredWorkerLabel{}
	for k, v := range s.Worker {
		labels[k] = &hatchet.DesiredWorkerLabel{Value: v, Required: true}
	}
	for k, v := range s.Prefer {
		labels[k] = &hatchet.DesiredWorkerLabel{Value: v, Weight: 10}
	}
	for k, v := range s.Avoid {
		labels[k] = &hatchet.DesiredWorkerLabel{Value: v, Required: true, Comparator: &notEqual}
	}
	return labels
}

func await(ref *hatchet.WorkflowRunRef, name string) StepResult {
	res, err := ref.Result()
	if err != nil {
		return StepResult{Name: name, Status: "failed", Detail: reason(err)}
	}
	var out StepResult
	if err := res.TaskOutput(StepTask).Into(&out); err != nil {
		return StepResult{Name: name, Status: "failed", Detail: "unreadable result: " + err.Error()}
	}
	return out
}

// stepFailed marks an error as the step's own failure, so that reason can
// recover what the step said from whatever the engine wrapped around it.
const stepFailed = "step failed: "

func reason(err error) string {
	text := err.Error()
	i := strings.LastIndex(text, stepFailed)
	if i < 0 {
		return strings.TrimSpace(text)
	}
	text = text[i+len(stepFailed):]
	// The engine hands a child's error back as the tail of a JSON string, so
	// newlines and quotes arrive escaped and a closing quote trails it.
	if plain, err := strconv.Unquote(`"` + strings.TrimSuffix(text, `"`) + `"`); err == nil {
		text = plain
	}
	return strings.TrimSpace(text)
}

func fail(format string, args ...any) error {
	// A step that failed will fail again on the same inputs; never retry it.
	return hatchet.NewNonRetryableError(errors.New(stepFailed + fmt.Sprintf(format, args...)))
}

func runStep(ctx hatchet.Context, cfg Config, in StepInput) (StepResult, error) {
	s := in.Step
	started := time.Now()

	root, err := os.MkdirTemp("", "starci-")
	if err != nil {
		return StepResult{}, err
	}
	defer os.RemoveAll(root)

	co, err := fetch(in.CloneURL, in.SHA, root)
	if err != nil {
		return StepResult{}, fail("fetch of %s failed: %v", in.SHA, err)
	}
	content, err := co.contentIDs(s.Inputs)
	if err != nil {
		return StepResult{}, fail("%v", err)
	}
	image, err := imageID(s.Image)
	if err != nil {
		return StepResult{}, fail("%v", err)
	}

	stored := filepath.Join(cfg.StateDir, "results", key(s, image, content)+".json")
	if s.Cache {
		if data, err := os.ReadFile(stored); err == nil {
			var earlier StepResult
			if json.Unmarshal(data, &earlier) == nil && earlier.Status == "passed" {
				ctx.Log(fmt.Sprintf("same image, command and inputs as a passing run on %s; not run again", earlier.Worker))
				earlier.Name, earlier.Cached, earlier.Seconds = s.Name, true, 0
				return earlier, nil
			}
		}
	}

	if err := co.materialise(s.Inputs); err != nil {
		return StepResult{}, fail("checkout failed: %v", err)
	}

	// Named, so that it can be removed if the run is cancelled: stopping the
	// docker client alone leaves the container running.
	container := "starci-" + filepath.Base(root)
	args := []string{"run", "--rm", "--name", container, "-v", co.workspace + ":/workspace", "-w", "/workspace"}
	for _, name := range sortedKeys(s.Caches) {
		args = append(args, "-v", "starci-cache-"+name+":"+s.Caches[name])
	}
	for _, name := range sortedKeys(s.Env) {
		args = append(args, "-e", name+"="+s.Env[name])
	}
	args = append(args, s.Image, "sh", "-c", s.Run)

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return StepResult{}, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return StepResult{}, fail("cannot start docker: %v", err)
	}
	var tail []string
	var output strings.Builder
	lines := bufio.NewScanner(out)
	lines.Buffer(make([]byte, 64*1024), 1024*1024)
	for lines.Scan() {
		line := lines.Text()
		if tail = append(tail, line); len(tail) > tailLines {
			tail = tail[1:]
		}
		if output.Len()+len(line) < outputLimit {
			output.WriteString(line)
			output.WriteByte('\n')
		}
		// Hatchet rejects an empty log message; a blank line still matters
		// for reading test output, so send it as a space.
		if line == "" {
			line = " "
		}
		ctx.Log(line)
	}
	runErr := cmd.Wait()
	if ctx.Err() != nil {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		return StepResult{}, fail("cancelled")
	}

	seconds := float64(time.Since(started).Round(100*time.Millisecond)) / float64(time.Second)
	if runErr != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			code = exit.ExitCode()
		}
		// Returning an error is what marks the task failed in Hatchet, so a
		// broken step is red in the dashboard instead of a green task
		// carrying a bad exit code.
		return StepResult{}, fail("exit code %d after %.1fs\n%s", code, seconds, strings.Join(tail, "\n"))
	}

	result := StepResult{Name: s.Name, Status: "passed", Seconds: seconds, Worker: cfg.WorkerName, Output: output.String()}
	if s.Cache {
		// Only passes are remembered. A failure is always worth running again.
		if data, err := json.Marshal(result); err == nil {
			if os.MkdirAll(filepath.Dir(stored), 0o755) == nil {
				_ = os.WriteFile(stored, data, 0o644)
			}
		}
	}
	return result, nil
}

// checkout is one commit, fetched into a git directory that is never mounted.
//
// Keeping the git directory out of the workspace is what makes inputs a real
// boundary: a shallow fetch still holds every file of the commit, so a
// workspace containing .git would let a step read what it did not declare.
type checkout struct {
	gitDir    string
	workspace string
}

func fetch(cloneURL, sha, root string) (*checkout, error) {
	co := &checkout{gitDir: filepath.Join(root, "git"), workspace: filepath.Join(root, "workspace")}
	if err := os.Mkdir(co.workspace, 0o755); err != nil {
		return nil, err
	}
	if out, err := exec.Command("git", "init", "-q", "--bare", co.gitDir).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	// Fetch exactly the commit rather than cloning a branch: the branch may
	// have moved on by the time this step runs.
	if _, err := co.git("fetch", "-q", "--depth", "1", cloneURL, sha); err != nil {
		return nil, err
	}
	return co, nil
}

func (co *checkout) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--git-dir=" + co.gitDir, "--work-tree=" + co.workspace}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// contentIDs returns git's own hash of each input: a blob id for a file, a
// tree id for a directory. Two commits with identical content under a path
// share it.
func (co *checkout) contentIDs(paths []string) ([][2]string, error) {
	var ids [][2]string
	for _, p := range paths {
		spec := "FETCH_HEAD:" + p
		if p == "." {
			spec = "FETCH_HEAD^{tree}"
		}
		id, err := co.git("rev-parse", "--verify", "-q", spec)
		if err != nil {
			return nil, fmt.Errorf("input %q does not exist in this commit", p)
		}
		ids = append(ids, [2]string{p, id})
	}
	return ids, nil
}

func (co *checkout) materialise(paths []string) error {
	_, err := co.git(append([]string{"checkout", "-q", "FETCH_HEAD", "--"}, paths...)...)
	return err
}

// imageID is the content id of the image that will actually run, pulling it if
// needed. A tag can be re-pointed; this cannot.
func imageID(image string) (string, error) {
	inspect := func() (string, error) {
		out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
		return strings.TrimSpace(string(out)), err
	}
	if id, err := inspect(); err == nil {
		return id, nil
	}
	if out, err := exec.Command("docker", "pull", "-q", image).CombinedOutput(); err != nil {
		return "", fmt.Errorf("cannot pull image %s: %s", image, strings.TrimSpace(string(out)))
	}
	return inspect()
}

// key covers everything that decides a step's outcome, as far as starci can
// know. The step's name is deliberately absent: renaming a step is not a
// reason to run it again.
func key(s plan.Step, image string, content [][2]string) string {
	env := [][2]string{}
	for _, k := range sortedKeys(s.Env) {
		env = append(env, [2]string{k, s.Env[k]})
	}
	material, _ := json.Marshal([]any{image, s.Run, env, content})
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
