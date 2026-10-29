// Command starci plans and runs a repository's Starlark CI script.
//
//	starci plan     evaluate a script and print the plan; runs nothing
//	starci run      execute a commit of a checkout on this machine, with Docker
//	                for steps in an image
//	starci compile  write a basic script out for GitHub Actions or CircleCI, or
//	                as an executable
//	starci validate check that a script's inputs select files and that the
//	                tools it pins download as pinned; runs nothing
//	starci next     say what a run does next, given its results so far
//	starci step     run one step that next asked for, on this machine
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.phin.tech/sam/starci/internal/bundle"
	"git.phin.tech/sam/starci/internal/ciinfo"
	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/engine/local"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	s3store "git.phin.tech/sam/starci/internal/store/s3"
	"git.phin.tech/sam/starci/internal/tracing"
)

const usage = `usage:
  starci plan   [--file .ci.star] [--repo owner/name] [--sha SHA] [--ref REF] [--changed a,b,c]
  starci run    CHECKOUT [-v] [--slots N] [--bare[=false]] [--bare-image IMAGE] [--sha SHA] [--file FILE] [--clone-url URL]
  starci compile [--target github|circleci|temporal|binary] [-o FILE|DIR --freeze --base STARCI] [--file .ci.star] [--runs-on NAME] [--arch amd64] [--offline] [--check FILE|DIR]
  starci validate [--file .ci.star] [--arch amd64,arm64]
  starci next   CHECKOUT [--sha SHA] [--file FILE] [--clone-url URL] < what is done so far
  starci step   [--bare[=false]] [--bare-image IMAGE] < a step next asked for
  starci sweep  [MACHINE]`

func main() {
	// Before anything else, even a script this executable carries: a
	// watchdog over a bare step is this executable run again.
	step.Watchdog()
	// An executable written by `starci compile --target binary` carries a
	// script and does nothing but run it, in the checkout it is pointed at.
	if b, err := bundle.Self(); err != nil {
		die("starci: " + err.Error())
	} else if b != nil {
		embedded = b
		dir, rest := ".", os.Args[1:]
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			dir, rest = rest[0], rest[1:]
		}
		if err := runCmd(append([]string{dir}, rest...)); err != nil {
			die("starci: " + err.Error())
		}
		return
	}
	if len(os.Args) < 2 {
		die(usage)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = planCmd(os.Args[2:])
	case "run":
		err = runCmd(os.Args[2:])
	case "compile":
		err = compileCmd(os.Args[2:])
	case "validate":
		err = validateCmd(os.Args[2:])
	case "next":
		err = nextCmd(os.Args[2:])
	case "step":
		err = stepCmd(os.Args[2:])
	case "sweep":
		err = sweepCmd(os.Args[2:])
	default:
		die(usage)
	}
	if err != nil {
		die("starci: " + err.Error())
	}
}

// embedded is the script this executable carries, if it was written by
// `starci compile --target binary`.
var embedded *bundle.Payload

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func planCmd(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	file := fs.String("file", ".ci.star", "CI script to evaluate")
	repo := fs.String("repo", "", "repository, as owner/name")
	sha := fs.String("sha", "", "commit being built")
	ref := fs.String("ref", "", "git ref being built")
	changed := fs.String("changed", "", "comma-separated files changed by the commit")
	event := fs.String("event", "", "why the run starts: push (the default), tag, pull_request, schedule or manual")
	tag := fs.String("tag", "", "the tag, for --event tag")
	scheduleName := fs.String("schedule", "", "the schedule, for --event schedule")
	fs.Parse(args)

	c := plan.Context{Repo: *repo, SHA: *sha, Ref: *ref}
	if err := applyEvent(&c, *event, *tag, *scheduleName); err != nil {
		return err
	}
	for _, f := range strings.Split(*changed, ",") {
		if f = strings.TrimSpace(f); f != "" {
			c.ChangedFiles = append(c.ChangedFiles, f)
		}
	}
	p, err := evalFile(*file, c)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // commands contain && and <, which are not HTML here
	return enc.Encode(p)
}

func evalFile(file string, c plan.Context) (*plan.Plan, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return plan.Eval(file, src, c)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	// Every run is on this machine; --local is what that was asked for with
	// when there was another kind, and is still accepted.
	fs.Bool("local", true, "accepted, and means nothing: every run is on this machine")
	var bare optionalBool
	fs.Var(&bare, "bare", "run bare steps, or with --bare=false refuse them (default: STARCI_BARE, else yes but for a pull request from a fork)")
	bareImage := fs.String("bare-image", os.Getenv("STARCI_BARE_IMAGE"), "run bare steps in this image instead of on the machine, each in a container of its own (default: STARCI_BARE_IMAGE)")
	rf := addRunFlags(fs)
	slots := fs.Int("slots", 4, "how many steps to run at once")
	verbose := fs.Bool("v", false, "print each step's output as it runs")
	// The checkout comes first on the command line, before the flags.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		die(usage)
	}
	dir := args[0]
	fs.Parse(args[1:])

	co, err := rf.resolve(dir)
	if err != nil {
		return err
	}
	src, mode, c := co.src, co.mode, co.context
	fmt.Printf("event: %s (%s)\n", describeEvent(c), co.origin)

	// In basic mode the plan is known now, whichever engine runs it.
	var p *plan.Plan
	if mode == plan.Basic {
		if embedded.Frozen() {
			// Fixed when the executable was written: not evaluated again.
			frozen := *embedded.Plan
			p = &frozen
			p.Repo, p.SHA, p.Ref = c.Repo, c.SHA, c.Ref
			err = p.Validate()
		} else {
			p, err = plan.Eval(co.file, src, c)
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s @ %.8s: %d step(s)\n", p.Repo, p.SHA, len(p.Steps))
		for _, layer := range p.Layers() {
			var names []string
			for _, s := range layer {
				names = append(names, s.Name)
			}
			fmt.Println("  " + strings.Join(names, "  |  "))
		}
		if len(p.Steps) == 0 {
			fmt.Println("nothing to run")
			return nil
		}
	} else {
		fmt.Printf("%s @ %.8s: advanced script, steps are decided as it runs\n", c.Repo, c.SHA)
	}

	run := engine.Run{Mode: mode, Plan: p, Context: c, CloneURL: co.cloneURL}
	if mode == plan.Advanced {
		run.Filename, run.Source = filepath.Base(co.file), src
	}

	// Ctrl-C stops the run; where the engine can, its steps too.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Spans go wherever the standard OTEL_EXPORTER_OTLP_* variables say,
	// and nowhere if they say nothing.
	_, flush, err := tracing.Export(ctx)
	if err != nil {
		return fmt.Errorf("setting up traces: %w", err)
	}
	defer flush()

	eng, err := localEngine(*slots, *verbose, c, bare, *bareImage)
	if err != nil {
		return err
	}
	// Whatever the run asks for that this engine does not do is said now,
	// not left to be discovered.
	for _, lost := range eng.Can().Missing(run) {
		fmt.Println("note: " + lost)
	}

	started := time.Now()
	result, err := eng.Execute(ctx, run)
	if err != nil {
		return err
	}

	fmt.Println()
	for _, r := range result.Steps {
		where := ""
		if r.Machine != "" {
			where = " on " + r.Machine
		}
		note := fmt.Sprintf("%.1fs%s", r.Seconds, where)
		if r.Cached {
			note = "cached, first passed" + where
		}
		fmt.Printf("  %-8s #%d %s  %s\n", r.Status, r.Number, r.Name, note)
		if r.Detail != "" {
			lines := strings.Split(r.Detail, "\n")
			if len(lines) > 15 {
				lines = lines[len(lines)-15:]
			}
			for _, l := range lines {
				fmt.Println("           " + l)
			}
		}
	}
	if result.Error != "" {
		fmt.Println("\n" + result.Error)
	}
	verdict := "PASSED"
	if !result.OK {
		verdict = "FAILED"
	}
	fmt.Printf("\n%s in %.1fs\n", verdict, time.Since(started).Seconds())
	if !result.OK {
		os.Exit(1)
	}
	return nil
}

// runFlags are what starci is told about a run of a checkout, by run and
// next alike.
type runFlags struct {
	cloneURL, sha, ref, file, repo, event, tag, scheduleName *string
}

func addRunFlags(fs *flag.FlagSet) runFlags {
	var f runFlags
	f.cloneURL = fs.String("clone-url", "", "URL the commit is fetched from (default: the checkout)")
	f.sha = fs.String("sha", "HEAD", "commit to build")
	f.ref = fs.String("ref", "", "branch this run is for (default: the checked-out branch when building HEAD)")
	f.file = fs.String("file", "", "CI script (default: .ci.star in the checkout)")
	f.repo = fs.String("repo", "", "owner/name, for display (default: from the clone URL)")
	f.event = fs.String("event", "", "why the run starts, to try what a script does for it: push, tag, pull_request, schedule or manual (default: what the CI tool this is running in says, or manual)")
	f.tag = fs.String("tag", "", "the tag, for --event tag")
	f.scheduleName = fs.String("schedule", "", "the schedule, for --event schedule")
	return f
}

// checkout is a run of a commit of a checkout, as worked out before any step
// runs: the script, and what it is told about the run.
type checkout struct {
	file     string
	src      []byte
	mode     string
	context  plan.Context
	cloneURL string
	// origin says where the event came from, for telling a person.
	origin string
}

func (f runFlags) resolve(dir string) (checkout, error) {
	if *f.cloneURL == "" {
		// The checkout is a perfectly good place to fetch a commit from. Only
		// what is committed is built, the same as on a machine.
		abs, err := filepath.Abs(dir)
		if err != nil {
			return checkout{}, err
		}
		*f.cloneURL = abs
	}

	git := func(a ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	commit, err := git("rev-parse", *f.sha)
	if err != nil {
		return checkout{}, fmt.Errorf("%s is not a commit in %s", *f.sha, dir)
	}
	changed, err := git("diff-tree", "--no-commit-id", "--name-only", "-r", commit)
	if err != nil {
		return checkout{}, err
	}
	if *f.file == "" {
		*f.file = filepath.Join(dir, ".ci.star")
	}
	if *f.ref == "" && *f.sha == "HEAD" {
		*f.ref, _ = git("symbolic-ref", "-q", "--short", "HEAD")
	}
	if *f.repo == "" {
		parts := strings.Split(strings.TrimSuffix(*f.cloneURL, ".git"), "/")
		if len(parts) >= 2 {
			*f.repo = strings.Join(parts[len(parts)-2:], "/")
		}
	}

	var src []byte
	if embedded != nil {
		*f.file, src = embedded.Name, []byte(embedded.Script)
	} else if src, err = os.ReadFile(*f.file); err != nil {
		return checkout{}, err
	}
	mode := plan.Basic
	if !embedded.Frozen() {
		if mode, err = plan.Mode(*f.file, src); err != nil {
			return checkout{}, err
		}
	}

	// Started by hand, unless something says otherwise.
	c := plan.Context{Repo: *f.repo, SHA: commit, Ref: *f.ref, ChangedFiles: strings.Fields(changed), Event: plan.EventManual}
	var schedules []plan.Schedule
	if !embedded.Frozen() {
		if schedules, err = plan.Schedules(*f.file, src); err != nil {
			return checkout{}, err
		}
	}
	// Inside a CI tool, the tool knows why this run exists. The script is
	// told what it would have been told had starci received the event.
	origin := "started by hand"
	if found, ok := ciinfo.Detect(os.Getenv, os.ReadFile, schedules); ok {
		origin = "from " + found.Tool
		d := found.Context
		c.Event, c.Tag, c.PR, c.Schedule = d.Event, d.Tag, d.PR, d.Schedule
		if d.Event == plan.EventTag || d.Ref != "" {
			c.Ref = d.Ref
		}
		if *f.repo == "" && d.Repo != "" {
			c.Repo = d.Repo
		}
		if found.ChangesKnown {
			c.ChangedFiles = d.ChangedFiles
		}
	}
	if *f.event != "" {
		origin = "as asked"
		if err := applyEvent(&c, *f.event, *f.tag, *f.scheduleName); err != nil {
			return checkout{}, err
		}
	}
	return checkout{file: *f.file, src: src, mode: mode, context: c, cloneURL: *f.cloneURL, origin: origin}, nil
}

// localEngine is this machine, with Docker for steps in an image: no queue,
// no other machines, and nothing to resume from if this process is stopped.
//
// Bare steps run here unless bareHere says otherwise.
func localEngine(slots int, verbose bool, c plan.Context, bare optionalBool, bareImage string) (engine.Engine, error) {
	// Named, because with a shared store this machine's results are seen
	// from others.
	host, _ := os.Hostname()
	if host == "" {
		host = "this machine"
	}
	cfg, err := machineConfig(host)
	if err != nil {
		return nil, err
	}
	if cfg.Bare, err = bareHere(os.Getenv, c, bare); err != nil {
		return nil, err
	}
	cfg.BareImage = bareImage
	eng := &local.Engine{Config: cfg, Slots: slots}
	if verbose {
		var mu sync.Mutex
		eng.Log = func(name, line string) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Printf("  [%s] %s\n", name, line)
		}
	}
	return eng, nil
}

// machineConfig is what this machine brings to running steps, from the
// environment: where it keeps its state, and the store it shares with others.
//
//	STARCI_STATE  where results and tools are kept (default: ~/.cache/starci)
//	STARCI_STORE  a store shared with other machines, as
//	              s3://bucket/prefix?endpoint=host, with credentials in the
//	              usual AWS_* variables. Optional.
func machineConfig(name string) (step.Config, error) {
	cfg := step.Config{Machine: name, StateDir: os.Getenv("STARCI_STATE")}
	if cfg.StateDir == "" {
		var err error
		if cfg.StateDir, err = step.DefaultStateDir(); err != nil {
			return cfg, err
		}
	}
	if location := os.Getenv("STARCI_STORE"); location != "" {
		store, err := s3store.Open(location)
		if err != nil {
			return cfg, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := store.Check(ctx); err != nil {
			return cfg, fmt.Errorf("STARCI_STORE is set but unusable: %w", err)
		}
		cfg.Shared = store
	}
	return cfg, nil
}

// bareHere says whether `starci run` runs bare steps: steps that name no
// image and run as this machine's own processes, as its user. --bare decides
// when it is given, and STARCI_BARE when it is set. Otherwise they run, this
// being the person's own machine and checkout, except for a pull request
// from a fork: that is someone else's code, and runs bare only on a machine
// thrown away after.
func bareHere(getenv func(string) string, c plan.Context, flag optionalBool) (bool, error) {
	if flag.set {
		return flag.value, nil
	}
	switch v := getenv("STARCI_BARE"); v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "":
	default:
		return false, fmt.Errorf("STARCI_BARE must be true or false, got %q", v)
	}
	if c.PR == nil || !c.PR.Fork {
		return true, nil
	}
	return ciinfo.Disposable(getenv), nil
}

// optionalBool is a flag that may be given as --name, --name=false, or not at
// all, and says which.
type optionalBool struct{ set, value bool }

func (b *optionalBool) String() string {
	if !b.set {
		return ""
	}
	return strconv.FormatBool(b.value)
}

func (b *optionalBool) Set(v string) error {
	parsed, err := strconv.ParseBool(v)
	b.set, b.value = true, parsed
	return err
}

func (b *optionalBool) IsBoolFlag() bool { return true }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// compileBinary writes an executable that carries the script and runs it with
// Docker, in the checkout it is pointed at:
//
//	starci compile --target binary -o ci && ./ci [checkout]
//
// By default the script travels and is evaluated when the executable runs, so
// advanced scripts and scripts that read ctx behave as under `starci run`.
// With --freeze the plan travels instead, as the other targets' files do.
func compileBinary(file, output, base string, freeze, offline bool, arch, check string) error {
	if check != "" {
		return errors.New("--check is for the workflow targets: an executable is not text to compare")
	}
	if output == "" {
		return errors.New("--target binary needs -o, where to write the executable")
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	mode, err := plan.Mode(file, src)
	if err != nil {
		return err
	}
	payload := bundle.Payload{Name: filepath.Base(file), Script: string(src)}
	if freeze {
		if mode == plan.Advanced {
			return fmt.Errorf("%s is an advanced script: its steps are only known as it runs, so it cannot be frozen (leave out --freeze to carry the script)", file)
		}
		p, read, err := plan.EvalTracked(file, src, plan.Context{})
		if err != nil {
			return err
		}
		if len(p.Steps) == 0 {
			return fmt.Errorf("%s plans no steps for a push about which nothing is known; there is nothing to freeze", file)
		}
		if len(read) > 0 {
			fmt.Fprintf(os.Stderr, "note: the script reads ctx.%s, so this plan is for a commit about which nothing is known, and is the same for every run\n", strings.Join(read, ", ctx."))
		}
		// The plan is what runs; the source is not needed again.
		payload.Script, payload.Plan = "", p
	}
	if !offline {
		if err := checkDeclaredTools(file, src, arch); err != nil {
			return err
		}
	}
	if base == "" {
		if base, err = os.Executable(); err != nil {
			return err
		}
	}
	if err := bundle.Write(output, base, payload); err != nil {
		return err
	}
	kind := "carries " + payload.Name
	if freeze {
		kind = "carries the plan of " + payload.Name + ", frozen"
	}
	fmt.Fprintf(os.Stderr, "wrote %s, which %s; run it in a checkout: %s [dir]\n", output, kind, output)
	return nil
}

// applyEvent sets why a run starts, from what was asked for on the command
// line.
func applyEvent(c *plan.Context, event, tag, schedule string) error {
	switch event {
	case "":
		if tag != "" || schedule != "" {
			return errors.New("--tag and --schedule go with --event tag and --event schedule")
		}
		return nil
	case plan.EventPush, plan.EventManual:
	case plan.EventTag:
		if tag == "" {
			return errors.New("--event tag needs --tag NAME")
		}
		c.Tag, c.Ref = tag, ""
	case plan.EventSchedule:
		if schedule == "" {
			return errors.New("--event schedule needs --schedule NAME")
		}
		c.Schedule = schedule
	case plan.EventPullRequest:
		// A stand-in: enough to see what a script does for a pull request
		// from this repository.
		c.PR = &plan.PullRequest{Number: 1, Base: "main", Head: c.Ref}
	default:
		return fmt.Errorf("unknown event %q: push, tag, pull_request, schedule or manual", event)
	}
	c.Event = event
	return nil
}

func describeEvent(c plan.Context) string {
	switch c.Event {
	case plan.EventTag:
		return "tag " + c.Tag
	case plan.EventPullRequest:
		if c.PR == nil {
			return "pull request"
		}
		from := ""
		if c.PR.Fork {
			from = ", from a fork"
		}
		return fmt.Sprintf("pull request %d%s", c.PR.Number, from)
	case plan.EventSchedule:
		if c.Schedule == "" {
			return "a schedule the script does not declare"
		}
		return "schedule " + c.Schedule
	case plan.EventManual:
		if c.Ref != "" {
			return "manual run of " + c.Ref
		}
		return "manual run"
	}
	if c.Ref != "" {
		return "push to " + c.Ref
	}
	return "push"
}

// firstDifference says where two texts part ways, in terms of the first.
func firstDifference(have, want string) string {
	h, w := strings.Split(have, "\n"), strings.Split(want, "\n")
	for i := range h {
		if i >= len(w) {
			return fmt.Sprintf("it has more after line %d, where it should end", i)
		}
		if h[i] != w[i] {
			return fmt.Sprintf("they first differ at line %d:\n  has:    %s\n  should: %s", i+1, h[i], w[i])
		}
	}
	return fmt.Sprintf("it ends at line %d, where there should be more", len(h))
}

// validateCmd checks what a script depends on that evaluating it cannot
// see. In the repository: that each input of each step selects a file git
// tracks. Outside it: that each tool it pins can still be downloaded, has the
// checksum the script gives, and unpacks to programs where the script says
// they are, for every architecture the tool lists, whatever this machine is.
func validateCmd(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	file := fs.String("file", ".ci.star", "CI script to validate")
	only := fs.String("arch", "", "platforms to check, comma-separated: amd64, arm64 (Linux), or os/arch such as darwin/arm64 (default: every one a tool lists)")
	fs.Parse(args)

	var archs []string
	for _, a := range strings.Split(*only, ",") {
		switch a = strings.TrimSpace(a); a {
		case "":
		case "amd64", "arm64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
			archs = append(archs, a)
		default:
			return fmt.Errorf("unknown platform %q: amd64, arm64, or one of linux/amd64, linux/arm64, darwin/amd64, darwin/arm64", a)
		}
	}
	src, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	// A script that could not be run is not valid, whatever its tools are
	// like. In basic mode that includes the plan it returns.
	mode, err := plan.Mode(*file, src)
	if err != nil {
		return err
	}
	var problems []string
	fmt.Println("inputs")
	if mode == plan.Basic {
		p, err := plan.Eval(*file, src, plan.Context{})
		if err != nil {
			return err
		}
		if failed := checkInputs(filepath.Dir(*file), p.Steps); failed > 0 {
			problems = append(problems, fmt.Sprintf("%d step(s) have an input that selects nothing", failed))
		}
	} else {
		fmt.Println("  not checked: an advanced script's steps are only known as it runs")
	}

	tools, err := plan.Tools(*file, src)
	if err != nil {
		return err
	}
	fmt.Println("tools")
	if len(tools) == 0 {
		fmt.Println("  none declared")
	}
	if failed := checkTools(os.Stdout, tools, archs); failed > 0 {
		problems = append(problems, fmt.Sprintf("%d tool download(s) failed the check", failed))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// checkInputs asks, of the repository dir is in, whether each input of each
// step selects a file that git tracks there. The steps are those planned for
// a commit about which nothing is known. It prints a line per step and
// returns how many have an input that selects nothing.
func checkInputs(dir string, steps []plan.Step) (failed int) {
	if err := exec.Command("git", "-C", dir, "rev-parse", "--git-dir").Run(); err != nil {
		fmt.Println("  not checked: the script is not in a git repository")
		return 0
	}
	if len(steps) == 0 {
		fmt.Println("  no steps are planned for a commit about which nothing is known")
	}
	for _, s := range steps {
		missing, err := step.MissingInputs(dir, s.Inputs)
		// A service's files are looked for the same way.
		var absent []string
		for _, sv := range s.Services {
			if err != nil {
				break
			}
			var gone []string
			gone, err = step.MissingInputs(dir, sv.FilePaths())
			for _, f := range gone {
				absent = append(absent, fmt.Sprintf("service %q: file %q is not a file that git tracks here", sv.Name, f))
			}
		}
		switch {
		case err != nil:
			failed++
			fmt.Printf("  %-8s %s\n           %v\n", "failed", s.Name, err)
		case len(missing)+len(absent) > 0:
			failed++
			fmt.Printf("  %-8s %s\n", "failed", s.Name)
			for _, in := range missing {
				fmt.Printf("           input %q selects no file that git tracks here\n", in)
			}
			for _, line := range absent {
				fmt.Printf("           %s\n", line)
			}
		default:
			fmt.Printf("  %-8s %s\n", "ok", s.Name)
		}
	}
	return failed
}

// checkTools downloads and checks each tool for each of the given
// architectures (all of its builds, when none are given), a few at a time. It writes a line per download to w, in order, and returns
// how many failed.
func checkTools(w *os.File, tools []plan.Tool, archs []string) (failed int) {
	type check struct {
		tool plan.Tool
		arch string
		done chan error
	}
	var checks []check
	for _, t := range tools {
		want := archs
		if len(want) == 0 {
			want = sortedKeys(t.SHA256)
		}
		// An architecture that was asked for and has no build is reported,
		// not passed over.
		for _, arch := range want {
			checks = append(checks, check{t, arch, make(chan error, 1)})
		}
	}
	slots := make(chan struct{}, 4)
	for _, c := range checks {
		go func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			c.done <- step.CheckTool(c.tool, c.arch)
		}()
	}
	for _, c := range checks {
		if err := <-c.done; err != nil {
			failed++
			fmt.Fprintf(w, "  %-8s %s %s\n           %v\n", "failed", c.tool.Name, c.arch, err)
			continue
		}
		fmt.Fprintf(w, "  %-8s %s %s\n", "ok", c.tool.Name, c.arch)
	}
	return failed
}
