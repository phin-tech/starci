// Package executor runs a plan as Hatchet tasks.
//
// Two generic tasks, rather than one registered workflow per repository,
// because the shape of a run is only known once its script has been evaluated:
//
//	ci-step      runs one container against the inputs of one step
//	ci-pipeline  walks a plan and starts a ci-step child per step
package executor

import (
	"bufio"
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

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"git.phin.tech/sam/starci/internal/plan"
)

const (
	StepTask     = "ci-step"
	PipelineTask = "ci-pipeline"

	// How much of a failed step's output is carried in its error, for the
	// caller. The full output is in the task's logs.
	tailLines = 40
)

type PipelineInput struct {
	Plan     plan.Plan `json:"plan"`
	CloneURL string    `json:"clone_url"`
}

type StepInput struct {
	CloneURL string    `json:"clone_url"`
	SHA      string    `json:"sha"`
	Step     plan.Step `json:"step"`
}

type StepResult struct {
	Name    string  `json:"name"`
	Status  string  `json:"status"` // passed, failed or skipped
	Seconds float64 `json:"seconds"`
	Worker  string  `json:"worker"`
	// Cached is true when an earlier passing run with the same key stood in
	// for this one.
	Cached bool   `json:"cached"`
	Detail string `json:"detail"`
}

type PipelineResult struct {
	OK    bool         `json:"ok"`
	Steps []StepResult `json:"steps"`
}

// Config is what a worker needs to know about the machine it is on.
type Config struct {
	// WorkerName is recorded in results, so a cached pass says where it ran.
	WorkerName string
	// StateDir holds passing results, one file per key. Per worker for now: a
	// result recorded on one machine is not seen by another.
	StateDir string
}

// Tasks defines both tasks on the client. Register them on a worker to
// execute plans, or use them from any process to trigger and await runs.
func Tasks(client *hatchet.Client, cfg Config) (step, pipeline *hatchet.StandaloneTask) {
	step = client.NewStandaloneTask(StepTask,
		func(ctx hatchet.Context, in StepInput) (StepResult, error) { return runStep(ctx, cfg, in) },
		hatchet.WithExecutionTimeout(30*time.Minute),
	)
	pipeline = client.NewStandaloneDurableTask(PipelineTask,
		func(ctx hatchet.DurableContext, in PipelineInput) (PipelineResult, error) {
			return runPipeline(ctx, step, in)
		},
		hatchet.WithExecutionTimeout(time.Hour),
	)
	return step, pipeline
}

func runPipeline(ctx hatchet.DurableContext, step *hatchet.StandaloneTask, in PipelineInput) (PipelineResult, error) {
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
			if len(s.Worker) > 0 {
				opt.Opts = append(opt.Opts, hatchet.WithDesiredWorkerLabels(requiredLabels(s.Worker)))
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
		for i, s := range runnable {
			results[s.Name] = await(&refs[i], s.Name)
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

func requiredLabels(want map[string]string) map[string]*hatchet.DesiredWorkerLabel {
	labels := map[string]*hatchet.DesiredWorkerLabel{}
	for k, v := range want {
		labels[k] = &hatchet.DesiredWorkerLabel{Value: v, Required: true}
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

	args := []string{"run", "--rm", "-v", co.workspace + ":/workspace", "-w", "/workspace"}
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
	lines := bufio.NewScanner(out)
	lines.Buffer(make([]byte, 64*1024), 1024*1024)
	for lines.Scan() {
		line := lines.Text()
		if tail = append(tail, line); len(tail) > tailLines {
			tail = tail[1:]
		}
		// Hatchet rejects an empty log message; a blank line still matters
		// for reading test output, so send it as a space.
		if line == "" {
			line = " "
		}
		ctx.Log(line)
	}
	runErr := cmd.Wait()

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

	result := StepResult{Name: s.Name, Status: "passed", Seconds: seconds, Worker: cfg.WorkerName}
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
