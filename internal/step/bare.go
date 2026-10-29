package step

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// Platform is what this machine is, as os/arch.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Unmet says which of a step's well-known requirements this machine does not
// meet, or nothing if it meets them all. Labels beyond those are an engine's
// business; these are the same for every engine, so the machine can check
// them itself.
func Unmet(s plan.Step) string {
	have := map[string]string{plan.PlaceOS: runtime.GOOS, plan.PlaceArch: runtime.GOARCH}
	var unmet []string
	for _, k := range []string{plan.PlaceOS, plan.PlaceArch} {
		if v, ok := s.On[k]; ok && v != have[k] {
			unmet = append(unmet, fmt.Sprintf("it needs %s=%s and this machine is %s", k, v, have[k]))
		}
		if v, ok := s.Avoid[k]; ok && v == have[k] {
			unmet = append(unmet, fmt.Sprintf("it avoids %s=%s, which this machine is", k, v))
		}
	}
	return strings.Join(unmet, "; ")
}

// runBare runs a step that names no image as a process of this machine, in
// its workspace, with the machine's PATH (its tools in front) and nothing
// else of the machine's environment, and a home and temporary directory of
// its own.
//
// What a container gives for nothing has to be done by hand here, and some of
// it cannot be: the workspace holds only the step's inputs, but nothing stops
// the step reading elsewhere on the machine. Only a machine that has agreed to
// run bare steps runs them.
func runBare(ctx context.Context, cfg Config, s plan.Step, co *checkout, root, content string, started time.Time, log func(string)) plan.Result {
	failed := func(format string, args ...any) plan.Result {
		return plan.Result{Name: s.Name, Status: "failed", Machine: cfg.Machine, Detail: fmt.Sprintf(format, args...)}
	}
	platform := Platform()

	var dirs []string
	var toolIDs [][]string
	for _, t := range s.Tools {
		done := phase(ctx, "install "+t.Name)
		in, err := install(cfg.StateDir, t, platform)
		done()
		if err != nil {
			return failed("%v", err)
		}
		for _, b := range in.bins {
			dirs = append(dirs, filepath.Join(in.dir, b))
		}
		toolIDs = append(toolIDs, append([]string{in.name, in.sum}, in.bins...))
	}

	resultKey := bareKey(s, platform, content, toolIDs)
	if earlier, ok := reuse(ctx, cfg, s, resultKey, log); ok {
		return earlier
	}

	done := phase(ctx, "check out the inputs")
	err := co.materialise(s.Inputs)
	done()
	if err != nil {
		return failed("checkout failed: %v", err)
	}
	if err := giveArtifacts(ctx, cfg, s, co.workspace); err != nil {
		return failed("%v", err)
	}

	home, tmp := filepath.Join(root, "home"), filepath.Join(root, "tmp")
	for _, d := range []string{home, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return failed("%v", err)
		}
	}
	// A step that sets its own PATH means it; tools still go in front.
	base := os.Getenv("PATH")
	if p, ok := s.Env["PATH"]; ok {
		base = p
	}
	path := strings.Join(append(dirs, base), string(os.PathListSeparator))
	// The workspace as the step will see it from inside: a shell reports
	// where it is with links resolved, and the temporary directory is often
	// reached through one.
	workspace, err := filepath.EvalSymlinks(co.workspace)
	if err != nil {
		return failed("%v", err)
	}
	// Nothing else of the machine's own environment: it is where a machine
	// keeps its tokens.
	env := []string{"HOME=" + home, "TMPDIR=" + tmp, "LANG=C.UTF-8", "STARCI_WORKSPACE=" + workspace}
	for _, k := range sortedKeys(s.Env) {
		if k != "PATH" {
			env = append(env, k+"="+s.Env[k])
		}
	}
	for _, name := range sortedKeys(s.Caches) {
		dir := filepath.Join(cfg.StateDir, "caches", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return failed("cache %q: %v", name, err)
		}
		env = append(env, s.Caches[name]+"="+dir)
	}
	env = append(env, "PATH="+path)

	argv := s.Argv
	if len(argv) == 0 {
		argv = []string{"/bin/sh", "-c", s.Run}
	}
	// Found on the step's PATH, not on starci's.
	program, err := lookIn(argv[0], path)
	if err != nil {
		return failed("%v", err)
	}
	cmd := exec.CommandContext(ctx, program, argv[1:]...)
	cmd.Dir, cmd.Env = workspace, env
	g, err := confine(cmd, s.Offline)
	if err != nil {
		return failed("%v", err)
	}

	done = phase(ctx, "run the command")
	defer done()
	ran, err := streamUntil(cmd, log, g)
	if err != nil {
		return failed("cannot start %s: %v", argv[0], err)
	}
	if ctx.Err() != nil {
		return failed("cancelled")
	}
	return finish(ctx, cfg, s, resultKey, started, ran, co.workspace, log)
}

// bareKey is key for a bare step, which a script asks to be reused with
// cache = True. There is no image to name, so what stands for one is this
// machine's platform and the step's tools. Nothing else about the machine is
// in it: what the step ran against is the script's to answer for.
func bareKey(s plan.Step, platform, content string, tools [][]string) string {
	env := [][2]string{}
	for _, k := range sortedKeys(s.Env) {
		env = append(env, [2]string{k, s.Env[k]})
	}
	parts := []any{keyVersion, "bare", platform, s.Run, env, content, tools}
	if len(s.Argv) > 0 {
		parts = append(parts, "argv", s.Argv)
	}
	if s.Offline {
		parts = append(parts, "offline")
	}
	parts = append(parts, artifactKey(s)...)
	material, _ := json.Marshal(parts)
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

// lookIn finds a program the way a shell given path as its PATH would.
func lookIn(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s is not on the step's PATH", name)
}

// cacheRoot is where a bare step run in an image finds its caches.
const cacheRoot = "/var/cache/starci"

// inImage is a bare step made into one that names image: what it is given
// the same, but in a container. A cache, which a bare step names by the
// variable to set to it, is a directory in the container that the variable
// names instead.
func inImage(s plan.Step, image string) plan.Step {
	s.Image = image
	env := map[string]string{"STARCI_WORKSPACE": "/workspace"}
	for k, v := range s.Env {
		env[k] = v
	}
	caches := map[string]string{}
	for name, variable := range s.Caches {
		caches[name] = cacheRoot + "/" + name
		env[variable] = caches[name]
	}
	s.Env, s.Caches = env, caches
	return s
}
