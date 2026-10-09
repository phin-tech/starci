// Package step runs one step on the machine it is called on: fetch the
// commit, give a container the step's inputs and nothing else, and reuse an
// earlier pass when nothing that decides the outcome has changed.
//
// It knows nothing about how steps are distributed or ordered. Every engine
// that executes a run ends up calling Run for each step.
package step

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
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

const (
	// How much of a failed step's output is carried in its detail. The whole
	// output is passed to the log function as it is produced.
	tailLines = 40
	// How much of a step's output is kept in its result, for a script to read.
	outputLimit = 64 * 1024
)

// Config is what Run needs to know about the machine it is on.
type Config struct {
	// Worker is recorded in results, so a reused pass says where it ran.
	Worker string
	// StateDir holds passing results, one file per key. It is per machine: a
	// result recorded on one is not seen by another.
	StateDir string
}

// DefaultStateDir is where results are kept when nothing else is configured.
func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "starci"), nil
}

// Run executes s against the commit sha fetched from cloneURL. Each line the
// step prints is passed to log.
//
// It always returns a result. A step that could not be run at all (the commit
// cannot be fetched, the image cannot be pulled, an input is missing) is a
// failed step like any other, with the reason in Detail.
func Run(ctx context.Context, cfg Config, cloneURL, sha string, s plan.Step, log func(string)) plan.Result {
	started := time.Now()
	failed := func(format string, args ...any) plan.Result {
		return plan.Result{Name: s.Name, Status: "failed", Worker: cfg.Worker, Detail: fmt.Sprintf(format, args...)}
	}

	root, err := os.MkdirTemp("", "starci-")
	if err != nil {
		return failed("%v", err)
	}
	defer os.RemoveAll(root)

	co, err := fetch(cloneURL, sha, root)
	if err != nil {
		return failed("fetch of %s failed: %v", sha, err)
	}
	content, err := co.contentIDs(s.Inputs)
	if err != nil {
		return failed("%v", err)
	}
	image, err := imageID(s.Image)
	if err != nil {
		return failed("%v", err)
	}

	stored := filepath.Join(cfg.StateDir, "results", key(s, image, content)+".json")
	if s.Cache {
		if data, err := os.ReadFile(stored); err == nil {
			var earlier plan.Result
			if json.Unmarshal(data, &earlier) == nil && earlier.Status == "passed" {
				log(fmt.Sprintf("same image, command and inputs as a passing run on %s; not run again", earlier.Worker))
				earlier.Name, earlier.Cached, earlier.Seconds = s.Name, true, 0
				return earlier
			}
		}
	}

	if err := co.materialise(s.Inputs); err != nil {
		return failed("checkout failed: %v", err)
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
		return failed("%v", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return failed("cannot start docker: %v", err)
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
		log(line)
	}
	runErr := cmd.Wait()
	if ctx.Err() != nil {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		return failed("cancelled")
	}

	seconds := float64(time.Since(started).Round(100*time.Millisecond)) / float64(time.Second)
	if runErr != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			code = exit.ExitCode()
		}
		r := failed("exit code %d after %.1fs\n%s", code, seconds, strings.Join(tail, "\n"))
		r.Seconds, r.Output = seconds, output.String()
		return r
	}

	result := plan.Result{Name: s.Name, Status: "passed", Seconds: seconds, Worker: cfg.Worker, Output: output.String()}
	if s.Cache {
		// Only passes are remembered. A failure is always worth running again.
		if data, err := json.Marshal(result); err == nil {
			if os.MkdirAll(filepath.Dir(stored), 0o755) == nil {
				_ = os.WriteFile(stored, data, 0o644)
			}
		}
	}
	return result
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
