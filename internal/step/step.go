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
	"io"
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
	// Machine is recorded in results, so a reused pass says where it ran.
	Machine string
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
	// Bare lets this machine run bare steps: steps that name no image and
	// run as its own processes, as its own user, with no container around
	// them. A machine that has not said so refuses them.
	Bare bool
	// BareImage, if set, is an image bare steps run in instead: each in a
	// container of its own, thrown away after, as if it had named the image.
	// Bare need not be set for them then.
	BareImage string
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
		return plan.Result{Name: s.Name, Status: "failed", Machine: cfg.Machine, Detail: fmt.Sprintf(format, args...)}
	}

	if s.Bare() && cfg.BareImage != "" {
		s = inImage(s, cfg.BareImage)
	}
	// Said before anything is fetched: no amount of work here changes what
	// this machine is.
	if unmet := Unmet(s); unmet != "" {
		return failed("this machine cannot take the step: %s", unmet)
	}
	if s.Bare() && !cfg.Bare {
		return failed("this machine does not run bare steps here; run with --bare or STARCI_BARE=true to let it")
	}

	root, err := os.MkdirTemp("", "starci-")
	if err != nil {
		return failed("%v", err)
	}
	defer os.RemoveAll(root)
	// The commit is kept apart from the workspace, not beside it, so that a
	// step with no container around it does not find every file one
	// directory up.
	gitRoot, err := os.MkdirTemp("", "starci-git-")
	if err != nil {
		return failed("%v", err)
	}
	defer os.RemoveAll(gitRoot)

	done := phase(ctx, "fetch the commit")
	co, err := fetch(cloneURL, sha, gitRoot, root, cfg.GitEnv)
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
	if s.Bare() {
		return runBare(ctx, cfg, s, co, root, content, started, log)
	}
	done = phase(ctx, "find the image")
	img, err := inspectImage(s.Image)
	done()
	if err != nil {
		return failed("%v", err)
	}
	// A service's image is part of what the step ran against, so it is
	// identified by content like the step's own: a tag can move.
	var serviceIDs [][]any
	for _, sv := range s.Services {
		done := phase(ctx, "find the image of "+sv.Name)
		svImg, err := inspectImage(sv.Image)
		done()
		if err != nil {
			return failed("service %q: %v", sv.Name, err)
		}
		env := [][2]string{}
		for _, k := range sortedKeys(sv.Env) {
			env = append(env, [2]string{k, sv.Env[k]})
		}
		id := []any{sv.Name, svImg.id, env, sv.Args, sv.Ready}
		// Only said when it has some, so that every other service keeps the
		// identity it had.
		if len(sv.Files) > 0 {
			given, err := co.contentID(sv.FilePaths())
			if err != nil {
				return failed("service %q: %v", sv.Name, strings.Replace(err.Error(), "input ", "file ", 1))
			}
			places := [][2]string{}
			for _, from := range sv.FilePaths() {
				places = append(places, [2]string{from, sv.Files[from]})
			}
			id = append(id, places, given)
		}
		serviceIDs = append(serviceIDs, id)
	}
	var tools []installed
	for _, t := range s.Tools {
		done := phase(ctx, "install "+t.Name)
		in, err := install(cfg.StateDir, t, "linux/"+img.arch)
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
	resultKey := key(s, img, content, toolIDs, serviceIDs)
	if earlier, ok := reuse(ctx, cfg, s, resultKey, log); ok {
		return earlier
	}

	done = phase(ctx, "check out the inputs")
	err = co.materialise(s.Inputs)
	done()
	if err != nil {
		return failed("checkout failed: %v", err)
	}
	if err := giveArtifacts(ctx, cfg, s, co.workspace); err != nil {
		return failed("%v", err)
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
	var services sidecars
	switch {
	case len(s.Services) > 0:
		log("starting " + strings.Join(serviceNames(s), ", "))
		done := phase(ctx, "start the services")
		// A service's files are checked out beside the workspace, never in
		// it: the step is given its own inputs and nothing of theirs.
		given := map[string]string{}
		for _, sv := range s.Services {
			if len(sv.Files) == 0 {
				continue
			}
			given[sv.Name] = filepath.Join(root, "services", sv.Name)
			if err := co.materialiseInto(given[sv.Name], sv.FilePaths()); err != nil {
				done()
				return failed("checkout of the files of service %q failed: %v", sv.Name, err)
			}
		}
		up, stop, err := startServices(ctx, container, cfg.Machine, s, given)
		done()
		// Deferred before the error is looked at: some may have started.
		defer stop()
		if ctx.Err() != nil {
			return failed("cancelled")
		}
		if err != nil {
			return failed("%v", err)
		}
		services = up
		// The network is internal when the step gave up the network, so it
		// reaches its services and nothing else.
		// Labelled like them, so that Sweep clears the network's last user too.
		args = append(args, "--network", up.network, "--label", ownerLabel+"="+cfg.Machine)
	case s.Offline:
		args = append(args, "--network", "none")
	}
	for _, name := range sortedKeys(s.Caches) {
		args = append(args, "-v", "starci-cache-"+name+":"+s.Caches[name])
	}
	for _, name := range sortedKeys(s.Env) {
		args = append(args, "-e", name+"="+s.Env[name])
	}
	args = append(args, toolMounts...)
	args = append(args, runImage)
	if len(s.Argv) > 0 {
		args = append(args, s.Argv...)
	} else {
		args = append(args, "sh", "-c", s.Run)
	}

	done = phase(ctx, "run the command")
	defer done()
	cmd := exec.CommandContext(ctx, "docker", args...)
	ran, err := stream(cmd, log)
	if err != nil {
		return failed("cannot start docker: %v", err)
	}
	if ctx.Err() != nil {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		return failed("cancelled")
	}
	if ran.err != nil {
		// A step that fails against a service often fails because of it.
		for _, name := range serviceNames(s) {
			if said := services.tail(name); said != "" {
				log("the last of what " + name + " said:\n" + said)
			}
		}
	}
	return finish(ctx, cfg, s, resultKey, started, ran, co.workspace, log)
}

// giveArtifacts writes the artifacts s is handed into its workspace.
func giveArtifacts(ctx context.Context, cfg Config, s plan.Step, workspace string) error {
	if len(s.Artifacts) == 0 {
		return nil
	}
	done := phase(ctx, "fetch the artifacts")
	defer done()
	return give(ctx, cfg.blobs(), workspace, s.Artifacts)
}

// reuse returns an earlier pass of the step, if it may have one.
func reuse(ctx context.Context, cfg Config, s plan.Step, resultKey string, log func(string)) (plan.Result, bool) {
	if !s.Cache {
		return plan.Result{}, false
	}
	done := phase(ctx, "look for an earlier pass")
	earlier, ok := cfg.store().Get(ctx, resultKey)
	done()
	if !ok {
		return plan.Result{}, false
	}
	log(fmt.Sprintf("same environment, command and inputs as a passing run on %s; not run again", earlier.Machine))
	earlier.Name, earlier.Cached, earlier.Seconds = s.Name, true, 0
	return earlier, true
}

// ran is what became of a command once it had started.
type ran struct {
	tail   []string
	output string
	// err is how it exited, nil for success.
	err error
}

// stream starts cmd, passes each line it prints to log, and waits for it.
// The error is for a command that could not be started at all.
func stream(cmd *exec.Cmd, log func(string)) (ran, error) {
	return streamUntil(cmd, log, nil)
}

// guard is what watches over a command beyond starting and waiting for it:
// started is called as soon as it has started, and exited as soon as it has
// exited, before its output is read to the end.
type guard struct {
	started func(log func(string))
	exited  func()
}

// streamUntil is stream, with g watching over the command. Whatever the
// command started that still holds the output open once it has exited is
// what g.exited is for.
func streamUntil(cmd *exec.Cmd, log func(string), g *guard) (ran, error) {
	out, in, err := os.Pipe()
	if err != nil {
		return ran{}, err
	}
	defer out.Close()
	cmd.Stdout, cmd.Stderr = in, in
	err = cmd.Start()
	// The command has its own copy; with this one closed, the output ends
	// when the last of the command's processes lets go of it.
	in.Close()
	if err != nil {
		return ran{}, err
	}
	if g != nil {
		g.started(log)
	}
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if g != nil {
			g.exited()
		}
		exited <- err
	}()
	var r ran
	var output strings.Builder
	lines := bufio.NewScanner(out)
	lines.Buffer(make([]byte, 64*1024), 1024*1024)
	for lines.Scan() {
		line := lines.Text()
		if r.tail = append(r.tail, line); len(r.tail) > tailLines {
			r.tail = r.tail[1:]
		}
		if output.Len()+len(line) < outputLimit {
			output.WriteString(line)
			output.WriteByte('\n')
		}
		log(line)
	}
	// A line too long to scan ends the loop early; the rest is still read,
	// so that the command is never left blocked writing to nobody.
	_, _ = io.Copy(io.Discard, out)
	r.err = <-exited
	r.output = output.String()
	return r, nil
}

// finish turns how a command ran into the step's result, and remembers a
// pass.
func finish(ctx context.Context, cfg Config, s plan.Step, resultKey string, started time.Time, r ran, workspace string, log func(string)) plan.Result {
	seconds := float64(time.Since(started).Round(100*time.Millisecond)) / float64(time.Second)
	if r.err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(r.err, &exit) {
			code = exit.ExitCode()
		}
		return plan.Result{Name: s.Name, Status: "failed", Machine: cfg.Machine, Seconds: seconds, Output: r.output,
			Detail: fmt.Sprintf("exit code %d after %.1fs\n%s", code, seconds, strings.Join(r.tail, "\n"))}
	}

	result := plan.Result{Name: s.Name, Status: "passed", Seconds: seconds, Machine: cfg.Machine, Output: r.output}
	if len(s.Keep) > 0 {
		done := phase(ctx, "keep the artifacts")
		kept, err := keep(ctx, cfg.blobs(), workspace, s.Keep)
		done()
		if err != nil {
			result.Status, result.Detail = "failed", err.Error()
			return result
		}
		result.Artifacts = kept
	}
	if s.Cache {
		// Only passes are remembered. A failure is always worth running again.
		// Not being able to remember one is worth saying, but the step passed.
		if err := cfg.store().Put(ctx, resultKey, result); err != nil {
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

func fetch(cloneURL, sha, gitRoot, root string, env []string) (*checkout, error) {
	co := &checkout{gitDir: filepath.Join(gitRoot, "git"), workspace: filepath.Join(root, "workspace"), env: env}
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
	return co.gitIn(co.workspace, args...)
}

// gitIn is git with dir as the place files are written to.
func (co *checkout) gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--git-dir=" + co.gitDir, "--work-tree=" + dir}, args...)...)
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
// machine uses. A machine refuses such a step, but only once it has been given
// it; this is the same question, asked of a checkout before anything runs.
//
// It is the tracked files that are asked about, not the directory's contents:
// a file that was never added is not in the commit a machine builds.
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
	return co.materialiseInto(co.workspace, inputs)
}

// materialiseInto writes the files the inputs select under dir, at the paths
// they have in the repository.
func (co *checkout) materialiseInto(dir string, inputs []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	args := []string{"checkout", "-q", "FETCH_HEAD", "--"}
	for _, in := range inputs {
		args = append(args, pathspec(in))
	}
	_, err := co.gitIn(dir, args...)
	return err
}

// key covers everything that decides a step's outcome, as far as starci can
// know. The step's name is deliberately absent: renaming a step is not a
// reason to run it again.
func key(s plan.Step, img image, content string, tools [][]string, services [][]any) string {
	env := [][2]string{}
	for _, k := range sortedKeys(s.Env) {
		env = append(env, [2]string{k, s.Env[k]})
	}
	// The architecture is named separately from the image id. For an image
	// built for several architectures, the id can be the same on all of them,
	// and a pass on arm64 says nothing about amd64.
	parts := []any{keyVersion, img.id, img.arch, s.Run, env, content, tools, s.Packages}
	// A command given as a list is only said when there is one, so that
	// every other step keeps its key.
	if len(s.Argv) > 0 {
		parts = append(parts, "argv", s.Argv)
	}
	// A pass with the network says nothing about the same step without it.
	// Only said when it is off, so that every other step keeps its key.
	if s.Offline {
		parts = append(parts, "offline")
	}
	// Likewise only said when there are some.
	if len(services) > 0 {
		parts = append(parts, "services", services)
	}
	parts = append(parts, artifactKey(s)...)
	material, _ := json.Marshal(parts)
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

func serviceNames(s plan.Step) []string {
	names := make([]string, len(s.Services))
	for i, sv := range s.Services {
		names[i] = sv.Name
	}
	return names
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
