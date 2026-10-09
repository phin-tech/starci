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
	// keyVersion is part of every result key. Raise it whenever what goes
	// into a key changes meaning, so that results stored under the old
	// meaning are never mistaken for results under the new one.
	keyVersion = 5
)

// Config is what Run needs to know about the machine it is on.
type Config struct {
	// Worker is recorded in results, so a reused pass says where it ran.
	Worker string
	// StateDir holds what this machine keeps between steps: its own copy of
	// passing results, and installed tools.
	StateDir string
	// Shared, if set, is a result store other machines use too. A step that
	// passed on any of them is not run again here. Without it, a machine
	// knows only its own results.
	Shared Store
}

func (c Config) store() Store {
	local := FileStore{Dir: filepath.Join(c.StateDir, "results")}
	if c.Shared == nil {
		return local
	}
	return Layered{Near: local, Far: c.Shared}
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
	content, err := co.contentID(s.Inputs)
	if err != nil {
		return failed("%v", err)
	}
	img, err := inspectImage(s.Image)
	if err != nil {
		return failed("%v", err)
	}
	var tools []installed
	for _, t := range s.Tools {
		in, err := install(cfg.StateDir, t, img.arch)
		if err != nil {
			return failed("%v", err)
		}
		tools = append(tools, in)
	}
	// A step that sets its own PATH means it; tools still go in front.
	basePath := img.path
	if p, ok := s.Env["PATH"]; ok {
		basePath = p
	}
	toolMounts, toolIDs := toolArgs(tools, basePath)

	// Keyed on the base image and the package list, not on the image built
	// from them: that image differs from machine to machine and from day to
	// day, and a key made from it could never be shared or even looked up
	// without building first.
	resultKey := key(s, img, content, toolIDs)
	results := cfg.store()
	if s.Cache {
		if earlier, ok := results.Get(ctx, resultKey); ok {
			log(fmt.Sprintf("same image, command and inputs as a passing run on %s; not run again", earlier.Worker))
			earlier.Name, earlier.Cached, earlier.Seconds = s.Name, true, 0
			return earlier
		}
	}

	if err := co.materialise(s.Inputs); err != nil {
		return failed("checkout failed: %v", err)
	}

	runImage := s.Image
	if len(s.Packages) > 0 {
		log("preparing an image with " + strings.Join(s.Packages, ", "))
		if runImage, err = withPackages(s.Image, img.id, s.Packages); err != nil {
			return failed("%v", err)
		}
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
	args = append(args, toolMounts...)
	args = append(args, runImage, "sh", "-c", s.Run)

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
		// Not being able to remember one is worth saying, but the step passed.
		if err := results.Put(ctx, resultKey, result); err != nil {
			log("warning: this result could not be stored, so the step will run again next time: " + err.Error())
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
	// Load the commit into the index, which is what lets git's full pathspec
	// matching be asked about it before any file is written out.
	if _, err := co.git("read-tree", "FETCH_HEAD"); err != nil {
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

// pathspec turns an input into the git pathspec that selects it. An input
// with wildcards is a pattern in git's glob syntax, where * stays within a
// directory and ** crosses them; anything else is a literal path, which as a
// directory means everything beneath it.
func pathspec(input string) string {
	if input == "." {
		return ":(top)"
	}
	if strings.ContainsAny(input, "*?[") {
		return ":(top,glob)" + input
	}
	return ":(top,literal)" + input
}

// contentID identifies exactly the files the inputs select in this commit: a
// hash over each one's path, mode and blob id.
//
// Git does the matching here and in materialise, from the same pathspecs, so
// the set a result is keyed on and the set a step is given cannot disagree.
// A new file that matches a pattern changes the id; one that does not, does
// not.
func (co *checkout) contentID(inputs []string) (string, error) {
	// Each input is checked on its own first: one that selects nothing is a
	// mistake in the script, and would otherwise vanish into the union.
	for _, in := range inputs {
		out, err := co.git("ls-files", "--", pathspec(in))
		if err != nil {
			return "", fmt.Errorf("input %q: %v", in, err)
		}
		if out == "" {
			return "", fmt.Errorf("input %q matches nothing in this commit", in)
		}
	}
	specs := make([]string, len(inputs))
	for i, in := range inputs {
		specs[i] = pathspec(in)
	}
	// One line per selected file, with its mode and blob id, in a stable
	// order, each file once however many inputs select it.
	listing, err := co.git(append([]string{"ls-files", "-s", "--"}, specs...)...)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(listing))
	return hex.EncodeToString(sum[:]), nil
}

func (co *checkout) materialise(inputs []string) error {
	args := []string{"checkout", "-q", "FETCH_HEAD", "--"}
	for _, in := range inputs {
		args = append(args, pathspec(in))
	}
	_, err := co.git(args...)
	return err
}

// key covers everything that decides a step's outcome, as far as starci can
// know. The step's name is deliberately absent: renaming a step is not a
// reason to run it again.
func key(s plan.Step, img image, content string, tools [][]string) string {
	env := [][2]string{}
	for _, k := range sortedKeys(s.Env) {
		env = append(env, [2]string{k, s.Env[k]})
	}
	// The architecture is named separately from the image id. For an image
	// built for several architectures, the id can be the same on all of them,
	// and a pass on arm64 says nothing about amd64.
	material, _ := json.Marshal([]any{keyVersion, img.id, img.arch, s.Run, env, content, tools, s.Packages})
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
