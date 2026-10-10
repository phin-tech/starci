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
	"go.opentelemetry.io/otel/trace"
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
	// GitEnv is extra environment for git when it fetches a commit, which is
	// how credentials for private repositories are supplied: as git
	// configuration in the environment, never in a clone URL, where they
	// would be stored with the run and shown in its logs.
	GitEnv []string
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

// phase marks one part of running a step, for whoever is tracing the run:
// where a step's time went is otherwise one number. It costs nothing when
// nobody is. The returned function ends the phase, and may be called twice.
func phase(ctx context.Context, name string) (done func()) {
	_, span := trace.SpanFromContext(ctx).TracerProvider().Tracer("starci").Start(ctx, name)
	return func() { span.End() }
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

	done := phase(ctx, "fetch the commit")
	co, err := fetch(cloneURL, sha, root, cfg.GitEnv)
	done()
	if err != nil {
		return failed("fetch of %s failed: %v", sha, err)
	}
	done = phase(ctx, "identify the inputs")
	content, err := co.contentID(s.Inputs)
	done()
	if err != nil {
		return failed("%v", err)
	}
	done = phase(ctx, "find the image")
	img, err := inspectImage(s.Image)
	done()
	if err != nil {
		return failed("%v", err)
	}
	var tools []installed
	for _, t := range s.Tools {
		done := phase(ctx, "install "+t.Name)
		in, err := install(cfg.StateDir, t, img.arch)
		done()
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
		// An earlier pass whose outputs nobody holds any more is not one to
		// reuse: the steps given them would have nothing to be given.
		done := phase(ctx, "look for an earlier pass")
		earlier, ok := results.Get(ctx, resultKey)
		ok = ok && held(ctx, results, earlier, s.Outputs)
		done()
		if ok {
			log(fmt.Sprintf("same image, command and inputs as a passing run on %s; not run again", earlier.Worker))
			earlier.Name, earlier.Cached, earlier.Seconds = s.Name, true, 0
			return earlier
		}
	}

	done = phase(ctx, "check out the inputs")
	err = co.materialise(s.Inputs)
	done()
	if err != nil {
		return failed("checkout failed: %v", err)
	}
	// After the checkout, so that what another step produced stands in for
	// whatever the repository has at the same path.
	if len(s.Given) > 0 {
		done := phase(ctx, "fetch what other steps produced")
		err := receive(ctx, results, co.workspace, root, s.Given)
		done()
		if err != nil {
			return failed("%v", err)
		}
	}

	runImage := s.Image
	if len(s.Packages) > 0 {
		log("preparing an image with " + strings.Join(s.Packages, ", "))
		done := phase(ctx, "prepare an image with packages")
		runImage, err = withPackages(s.Image, img.id, s.Packages)
		done()
		if err != nil {
			return failed("%v", err)
		}
	}

	// Named, so that it can be removed if the run is cancelled: stopping the
	// docker client alone leaves the container running.
	container := "starci-" + filepath.Base(root)
	args := []string{"run", "--rm", "--name", container, "-v", co.workspace + ":/workspace", "-w", "/workspace"}
	if s.Offline {
		args = append(args, "--network", "none")
	}
	for _, name := range sortedKeys(s.Caches) {
		args = append(args, "-v", "starci-cache-"+name+":"+s.Caches[name])
	}
	for _, name := range sortedKeys(s.Env) {
		args = append(args, "-e", name+"="+s.Env[name])
	}
	args = append(args, toolMounts...)
	args = append(args, runImage, "sh", "-c", s.Run)

	done = phase(ctx, "run the command")
	defer done()
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
	// The command is over; what follows is the machine's doing, not its.
	done()
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
	if len(s.Outputs) > 0 {
		// A step that passed without leaving what it said it would has not
		// done what the steps after it are counting on.
		done := phase(ctx, "keep the outputs")
		kept, err := keep(ctx, results, co.workspace, root, s.Outputs, log)
		done()
		if err != nil {
			r := failed("the command passed, but %v", err)
			r.Seconds, r.Output = seconds, output.String()
			return r
		}
		result.Outputs = kept
	}
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
	env       []string
}

func fetch(cloneURL, sha, root string, env []string) (*checkout, error) {
	co := &checkout{gitDir: filepath.Join(root, "git"), workspace: filepath.Join(root, "workspace"), env: env}
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
	// Never wait at a prompt for a password nobody will type.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), co.env...)
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
	// A step given only what other steps produced takes nothing from the
	// commit. Asked with no pathspec at all, git would answer with all of it.
	if len(inputs) == 0 {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:]), nil
	}
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

// MissingInputs returns those of a step's inputs that select none of the
// files git tracks in the repository dir is in, asked with the pathspecs a
// worker uses. A worker refuses such a step, but only once it has been given
// it; this is the same question, asked of a checkout before anything runs.
//
// It is the tracked files that are asked about, not the directory's contents:
// a file that was never added is not in the commit a worker builds.
func MissingInputs(dir string, inputs []string) ([]string, error) {
	var missing []string
	for _, in := range inputs {
		out, err := exec.Command("git", "-C", dir, "ls-files", "--", pathspec(in)).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("input %q: %s", in, strings.TrimSpace(string(out)))
		}
		if len(out) == 0 {
			missing = append(missing, in)
		}
	}
	return missing, nil
}

func (co *checkout) materialise(inputs []string) error {
	if len(inputs) == 0 {
		return nil
	}
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
	parts := []any{keyVersion, img.id, img.arch, s.Run, env, content, tools, s.Packages}
	// A pass with the network says nothing about the same step without it.
	// Only said when it is off, so that every other step keeps its key.
	if s.Offline {
		parts = append(parts, "offline")
	}
	// What a step is given by other steps is an input like any other: its
	// path and its content, and not which step made it. And what it is to
	// leave behind is part of what a pass means, so a pass that left nothing
	// is not taken for one that would have left something.
	if len(s.Given) > 0 {
		given := [][2]string{}
		for _, g := range s.Given {
			given = append(given, [2]string{g.Path, g.Digest})
		}
		sort.Slice(given, func(i, j int) bool { return given[i][0] < given[j][0] })
		parts = append(parts, "given", given)
	}
	if len(s.Outputs) > 0 {
		parts = append(parts, "outputs", s.Outputs)
	}
	material, _ := json.Marshal(parts)
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
