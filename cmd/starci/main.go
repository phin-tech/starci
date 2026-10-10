// Command starci plans and runs a repository's Starlark CI script.
//
//	starci plan     evaluate a script and print the plan; runs nothing
//	starci run      execute a commit of a local checkout: on Hatchet's workers,
//	                or with --local on this machine with nothing but Docker
//	starci worker   execute steps on this machine
//	starci serve    be the address a forge delivers to, passing deliveries on
//	starci hooks    have Hatchet itself be that address
//	starci compile  write a basic script out for GitHub Actions or CircleCI
//	starci validate check that a script's inputs select files and that the
//	                tools it pins download as pinned; runs nothing
//	starci workers  list the workers Hatchet knows, and what they offer
//	starci shun     stop giving a worker new steps; unshun reverses it
//
// Everything but plan, validate, compile and run --local reaches Hatchet
// through the HATCHET_CLIENT_* variables.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
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

	"git.phin.tech/sam/starci/internal/ciinfo"
	"git.phin.tech/sam/starci/internal/compile"
	"git.phin.tech/sam/starci/internal/engine"
	onhatchet "git.phin.tech/sam/starci/internal/engine/hatchet"
	"git.phin.tech/sam/starci/internal/engine/local"
	"git.phin.tech/sam/starci/internal/forge"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	s3store "git.phin.tech/sam/starci/internal/store/s3"
	"git.phin.tech/sam/starci/internal/tracing"
	"git.phin.tech/sam/starci/internal/trigger"
)

const usage = `usage:
  starci plan   [--file .ci.star] [--repo owner/name] [--sha SHA] [--ref REF] [--changed a,b,c]
  starci run    CHECKOUT --clone-url URL [--sha SHA] [--ref BRANCH] [--file FILE] [--repo owner/name]
  starci run    CHECKOUT --local [-v] [--slots N] [--sha SHA] [--file FILE]
  starci worker
  starci serve
  starci hooks add forgejo|github
  starci compile [--target github|circleci] [--file .ci.star] [--runs-on NAME] [--arch amd64] [--offline] [--check FILE]
  starci validate [--file .ci.star] [--arch amd64,arm64]
  starci workers
  starci shun WORKER
  starci unshun WORKER`

func main() {
	if len(os.Args) < 2 {
		die(usage)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = planCmd(os.Args[2:])
	case "run":
		err = runCmd(os.Args[2:])
	case "worker":
		err = workerCmd()
	case "compile":
		err = compileCmd(os.Args[2:])
	case "validate":
		err = validateCmd(os.Args[2:])
	case "serve":
		err = serveCmd()
	case "hooks":
		err = hooksCmd(os.Args[2:])
	case "workers":
		err = workersCmd()
	case "shun", "unshun":
		if len(os.Args) != 3 {
			die(usage)
		}
		err = shunCmd(os.Args[2], os.Args[1] == "shun")
	default:
		die(usage)
	}
	if err != nil {
		die("starci: " + err.Error())
	}
}

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
	localRun := fs.Bool("local", false, "run the steps on this machine with Docker, without Hatchet")
	cloneURL := fs.String("clone-url", "", "URL workers fetch the commit from (with --local, defaults to the checkout)")
	sha := fs.String("sha", "HEAD", "commit to build")
	ref := fs.String("ref", "", "branch this run is for; a newer run for the same branch cancels it (default: the checked-out branch when building HEAD)")
	file := fs.String("file", "", "CI script (default: .ci.star in the checkout)")
	repo := fs.String("repo", "", "owner/name, for display (default: from the clone URL)")
	event := fs.String("event", "", "why the run starts, to try what a script does for it: push, tag, pull_request, schedule or manual (default: what the CI tool this is running in says, or manual)")
	tag := fs.String("tag", "", "the tag, for --event tag")
	scheduleName := fs.String("schedule", "", "the schedule, for --event schedule")
	slots := fs.Int("slots", 4, "with --local, how many steps to run at once")
	verbose := fs.Bool("v", false, "with --local, print each step's output as it runs")
	// The checkout comes first on the command line, before the flags.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		die(usage)
	}
	dir := args[0]
	fs.Parse(args[1:])

	if *cloneURL == "" {
		if !*localRun {
			die(usage)
		}
		// The checkout is a perfectly good place to fetch a commit from. Only
		// what is committed is built, the same as on a worker.
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		*cloneURL = abs
	}

	git := func(a ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	commit, err := git("rev-parse", *sha)
	if err != nil {
		return fmt.Errorf("%s is not a commit in %s", *sha, dir)
	}
	changed, err := git("diff-tree", "--no-commit-id", "--name-only", "-r", commit)
	if err != nil {
		return err
	}
	if *file == "" {
		*file = filepath.Join(dir, ".ci.star")
	}
	if *ref == "" && *sha == "HEAD" {
		*ref, _ = git("symbolic-ref", "-q", "--short", "HEAD")
	}
	if *repo == "" {
		parts := strings.Split(strings.TrimSuffix(*cloneURL, ".git"), "/")
		if len(parts) >= 2 {
			*repo = strings.Join(parts[len(parts)-2:], "/")
		}
	}

	src, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	mode, err := plan.Mode(*file, src)
	if err != nil {
		return err
	}

	// Started by hand, unless something says otherwise.
	c := plan.Context{Repo: *repo, SHA: commit, Ref: *ref, ChangedFiles: strings.Fields(changed), Event: plan.EventManual}
	schedules, err := plan.Schedules(*file, src)
	if err != nil {
		return err
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
		if *repo == "" && d.Repo != "" {
			c.Repo = d.Repo
		}
		if found.ChangesKnown {
			c.ChangedFiles = d.ChangedFiles
		}
	}
	if *event != "" {
		origin = "as asked"
		if err := applyEvent(&c, *event, *tag, *scheduleName); err != nil {
			return err
		}
	}
	fmt.Printf("event: %s (%s)\n", describeEvent(c), origin)

	// In basic mode the plan is known now, whichever engine runs it.
	var p *plan.Plan
	if mode == plan.Basic {
		if p, err = plan.Eval(*file, src, c); err != nil {
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

	run := engine.Run{Mode: mode, Plan: p, Context: c, CloneURL: *cloneURL, Supersedes: trigger.Supersedes(c)}
	if mode == plan.Advanced {
		run.Filename, run.Source = filepath.Base(*file), src
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

	var eng engine.Engine
	if *localRun {
		eng, err = localEngine(*slots, *verbose)
	} else {
		eng, err = onhatchet.New(onhatchet.Options{})
	}
	if err != nil {
		return err
	}
	// Whatever the run asks for that this engine does not do is said now,
	// not left to be discovered.
	for _, lost := range eng.Can().Missing(run) {
		fmt.Println("note: " + lost)
	}
	// A step nobody can take waits in silence until the engine gives up on
	// it. Say so now, while the cause is still obvious.
	if fleet, ok := eng.(engine.Fleet); ok && p != nil {
		if workers, werr := fleet.Workers(ctx); werr == nil {
			for _, s := range p.Steps {
				if !engine.CanTake(workers, s) {
					fmt.Printf("warning: no connected worker can take step %q (needs %s); it will wait for one\n", s.Name, engine.Placement(s))
				}
			}
		}
	}

	started := time.Now()
	result, err := eng.Execute(ctx, run)
	if err != nil {
		return err
	}

	fmt.Println()
	for _, r := range result.Steps {
		where := ""
		if r.Worker != "" {
			where = " on " + r.Worker
		}
		note := fmt.Sprintf("%.1fs%s", r.Seconds, where)
		if r.Cached {
			note = "cached, first passed" + where
		}
		fmt.Printf("  %-8s %s  %s\n", r.Status, r.Name, note)
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

// localEngine is this machine, with nothing but Docker: the same script, the
// same sandbox and the same result cache as a worker, and no queue, no other
// machines, and nothing to resume from if this process is stopped.
func localEngine(slots int, verbose bool) (engine.Engine, error) {
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

// workerCmd is configured by environment, since it normally runs as a service:
//
//	STARCI_WORKER_NAME  name recorded in results (default: hostname)
//	STARCI_STATE        where results and tools are kept (default: ~/.cache/starci)
//	STARCI_STORE        a result store shared with other machines (see machineConfig)
//	STARCI_STEP_TIMEOUT, STARCI_STEP_CPUS, STARCI_STEP_MEMORY
//	                    the most a step may have here, and what one that does not say gets
//	STARCI_SLOTS        steps to run at once (default: 4)
//	STARCI_LABELS       what this machine offers, as k=v,k=v (e.g. gpu=true)
//	STARCI_DASHBOARD    Hatchet's dashboard, as a person reaches it; statuses then link to the run
//	STARCI_TRACES       true to send Hatchet a trace of where each step's time went
//	OTEL_EXPORTER_OTLP_ENDPOINT (and the other standard OTEL_EXPORTER_OTLP_*
//	                    variables) send the same traces to any OpenTelemetry endpoint
func workerCmd() error {
	name := os.Getenv("STARCI_WORKER_NAME")
	if name == "" {
		name, _ = os.Hostname()
	}
	cfg, err := machineConfig(name)
	if err != nil {
		return err
	}
	slots := 4
	if v := os.Getenv("STARCI_SLOTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("STARCI_SLOTS must be a positive number, got %q", v)
		}
		slots = n
	}
	labels := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("STARCI_LABELS"), ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return fmt.Errorf("STARCI_LABELS entries must look like key=value, got %q", pair)
		}
		labels[k] = v
	}

	forges, secrets, err := forgesFromEnv()
	if err != nil {
		return err
	}
	// A worker given a forge's webhook secret also acts on its deliveries:
	// it can check them, and it has the token to fetch the script.
	reception := &onhatchet.Reception{Hooks: map[string]trigger.Hook{}, AllowForks: os.Getenv("STARCI_ALLOW_FORK_PULL_REQUESTS") == "true"}
	for forgeName, f := range forges {
		if len(secrets[forgeName]) > 0 {
			reception.Hooks[forgeName] = trigger.Hook{Forge: f, Secret: secrets[forgeName]}
		}
	}

	eng, err := onhatchet.New(onhatchet.Options{Machine: cfg, Post: reporter(forges), Reception: reception, Slots: slots, Labels: labels, Dashboard: os.Getenv("STARCI_DASHBOARD"), Traces: os.Getenv("STARCI_TRACES") == "true"})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, flush, err := tracing.Export(ctx)
	if err != nil {
		return fmt.Errorf("setting up traces: %w", err)
	}
	defer flush()
	return engine.Host(eng).Work(ctx)
}

// machineConfig is what this machine brings to running steps, from the
// environment: where it keeps its state, and the store it shares with others.
//
//	STARCI_STATE  where results and tools are kept (default: ~/.cache/starci)
//	STARCI_STORE  a store shared with other machines, as
//	              s3://bucket/prefix?endpoint=host, with credentials in the
//	              usual AWS_* variables. Optional.
func machineConfig(name string) (step.Config, error) {
	cfg := step.Config{Worker: name, StateDir: os.Getenv("STARCI_STATE")}
	if cfg.StateDir == "" {
		var err error
		if cfg.StateDir, err = step.DefaultStateDir(); err != nil {
			return cfg, err
		}
	}
	forges, _, err := forgesFromEnv()
	if err != nil {
		return cfg, err
	}
	var all []forge.Forge
	for _, f := range forges {
		all = append(all, f)
	}
	cfg.GitEnv = forge.GitEnv(all...)
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
	if v := os.Getenv("STARCI_STEP_TIMEOUT"); v != "" {
		seconds, err := plan.ParseTimeout(v)
		if err != nil {
			return cfg, fmt.Errorf("STARCI_STEP_TIMEOUT: %w", err)
		}
		cfg.Limits.Timeout = time.Duration(seconds) * time.Second
	}
	if v := os.Getenv("STARCI_STEP_CPUS"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("STARCI_STEP_CPUS must be a number above zero, got %q", v)
		}
		cfg.Limits.CPUs = n
	}
	if v := os.Getenv("STARCI_STEP_MEMORY"); v != "" {
		if cfg.Limits.Memory, err = plan.ParseMemory(v); err != nil {
			return cfg, fmt.Errorf("STARCI_STEP_MEMORY: %w", err)
		}
	}
	return cfg, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func workersCmd() error {
	eng, err := onhatchet.New(onhatchet.Options{})
	if err != nil {
		return err
	}
	workers, err := engine.Fleet(eng).Workers(context.Background())
	if err != nil {
		return err
	}
	for _, w := range workers {
		var labels []string
		for _, k := range sortedKeys(w.Labels) {
			labels = append(labels, k+"="+w.Labels[k])
		}
		state := "taking work"
		if w.Shunned {
			state = "shunned"
		}
		fmt.Printf("%-24s %-12s %s\n", w.Name, state, strings.Join(labels, " "))
	}
	if len(workers) == 0 {
		fmt.Println("no workers connected")
	}
	return nil
}

// shunCmd stops (or resumes) new work being given to a worker. Steps it is
// already running finish; nothing new arrives. For a machine that is
// misbehaving, or about to be taken down.
func shunCmd(name string, shun bool) error {
	eng, err := onhatchet.New(onhatchet.Options{})
	if err != nil {
		return err
	}
	if err := engine.Fleet(eng).Shun(context.Background(), name, shun); err != nil {
		return err
	}
	if shun {
		fmt.Printf("%s is shunned: it will finish what it has and get nothing new\n", name)
	} else {
		fmt.Printf("%s is taking work again\n", name)
	}
	return nil
}

// forgesFromEnv reads the forges this process can talk to, and the secret
// each signs its deliveries with.
//
//	STARCI_FORGEJO_URL          https://git.example.com
//	STARCI_FORGEJO_TOKEN_FILE   file holding an access token for it
//	STARCI_FORGEJO_SECRET_FILE  file holding its webhook secret (serve only)
//	STARCI_GITHUB_TOKEN_FILE    file holding a GitHub token
//	STARCI_GITHUB_API           the API's address, for GitHub Enterprise
//	STARCI_GITHUB_SECRET_FILE   file holding its webhook secret (serve only)
//
// Secrets are named by file, not given by value, so that they are never in a
// process listing or a compose file.
func forgesFromEnv() (forges map[string]forge.Forge, secrets map[string][]byte, err error) {
	forges, secrets = map[string]forge.Forge{}, map[string][]byte{}
	read := func(variable string) (string, error) {
		path := os.Getenv(variable)
		if path == "" {
			return "", nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", variable, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if base := os.Getenv("STARCI_FORGEJO_URL"); base != "" {
		token, err := read("STARCI_FORGEJO_TOKEN_FILE")
		if err != nil {
			return nil, nil, err
		}
		forges["forgejo"] = forge.Forgejo{BaseURL: base, Token: token}
		secret, err := read("STARCI_FORGEJO_SECRET_FILE")
		if err != nil {
			return nil, nil, err
		}
		secrets["forgejo"] = []byte(secret)
	}
	token, err := read("STARCI_GITHUB_TOKEN_FILE")
	if err != nil {
		return nil, nil, err
	}
	if token != "" {
		// The API's address, for GitHub Enterprise; github.com when unset.
		forges["github"] = forge.GitHub{Token: token, API: os.Getenv("STARCI_GITHUB_API")}
		secret, err := read("STARCI_GITHUB_SECRET_FILE")
		if err != nil {
			return nil, nil, err
		}
		secrets["github"] = []byte(secret)
	}
	return forges, secrets, nil
}

// reporter posts outcomes through whichever of the forges a run names.
func reporter(forges map[string]forge.Forge) onhatchet.Reporter {
	if len(forges) == 0 {
		return nil
	}
	return func(ctx context.Context, to trigger.Report, s forge.Status) error {
		f, ok := forges[to.Forge]
		if !ok {
			return fmt.Errorf("this worker has no credentials for %s", to.Forge)
		}
		s.Context = to.Context
		return f.SetStatus(ctx, to.Repo, to.SHA, s)
	}
}

// serveCmd is a door: the address a forge delivers webhooks to. It passes
// each delivery to Hatchet as an event and does nothing else. A worker acts
// on it. So this holds no access to the forge and can start nothing itself,
// and anything else that raises the same event can stand in for it; see
// doors/.
//
//	STARCI_LISTEN  address to listen on (default :8080)
//
// plus the forges to accept deliveries for, from forgesFromEnv, and Hatchet's
// HATCHET_CLIENT_* variables. A forge delivers to /hooks/forgejo or
// /hooks/github. Given a forge's webhook secret, unsigned deliveries are
// turned away here; without it they are passed on and turned away by the
// worker.
func serveCmd() error {
	forges, secrets, err := forgesFromEnv()
	if err != nil {
		return err
	}
	if len(forges) == 0 {
		return errors.New("no forge is configured; set STARCI_FORGEJO_URL or STARCI_GITHUB_TOKEN_FILE")
	}
	eng, err := onhatchet.New(onhatchet.Options{})
	if err != nil {
		return err
	}

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
	}
	listen := os.Getenv("STARCI_LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	mux := http.NewServeMux()
	mux.Handle("/hooks/", &trigger.Door{Forges: forges, Secrets: secrets, Send: engine.Relay(eng).Relay, Log: logf})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	var names []string
	for name := range forges {
		names = append(names, "/hooks/"+name)
	}
	sort.Strings(names)
	fmt.Fprintf(os.Stderr, "listening on %s for %s\n", listen, strings.Join(names, ", "))
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}

// hooksCmd makes Hatchet itself the door for a forge: it registers a webhook
// with Hatchet, which then receives the forge's deliveries, checks their
// signatures, and raises the event a worker acts on. Nothing of starci's is
// in the path.
//
//	starci hooks add forgejo|github
//
// The forge's webhook secret is read as for a worker, from
// STARCI_FORGEJO_SECRET_FILE or STARCI_GITHUB_SECRET_FILE.
func hooksCmd(args []string) error {
	if len(args) != 2 || args[0] != "add" {
		return errors.New("usage: starci hooks add forgejo|github")
	}
	name := args[1]
	_, secrets, err := forgesFromEnv()
	if err != nil {
		return err
	}
	if len(secrets[name]) == 0 {
		return fmt.Errorf("no webhook secret is configured for %s; the engine needs it to check deliveries", name)
	}
	eng, err := onhatchet.New(onhatchet.Options{})
	if err != nil {
		return err
	}
	path, err := engine.Relay(eng).AcceptWebhooks(context.Background(), name, secrets[name])
	if err != nil {
		return err
	}
	fmt.Printf("%s now accepts %s deliveries. Point the forge's webhook at:\n\n  <%s address>%s\n\nwith content type application/json and the same secret.\n", eng.Name(), name, eng.Name(), path)
	return nil
}

// compileCmd writes a basic script out as another CI system's configuration,
// for a repository hosted where starci is not running:
//
//	starci compile > .github/workflows/ci.yml
//	starci compile --target circleci > .circleci/config.yml
//
// Forgejo and Gitea Actions read the GitHub file.
func compileCmd(args []string) error {
	fs := flag.NewFlagSet("compile", flag.ExitOnError)
	file := fs.String("file", ".ci.star", "CI script to compile")
	target := fs.String("target", "github", "what to write: github (also read by Forgejo and Gitea) or circleci")
	runsOn := fs.String("runs-on", "", "the machines to run on: a runner label for github (default ubuntu-latest), a resource class for circleci (default medium)")
	arch := fs.String("arch", "amd64", "architecture of those machines: amd64 or arm64")
	defaultBranch := fs.String("default-branch", "main", "the repository's default branch")
	offline := fs.Bool("offline", false, "do not download the script's tools to check them first")
	check := fs.String("check", "", "write nothing: fail unless this file is what would be written")
	fs.Parse(args)

	src, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	opts := compile.Options{Source: filepath.Base(*file), RunsOn: *runsOn, Arch: *arch, DefaultBranch: *defaultBranch}
	var out string
	switch *target {
	case "github":
		// Asked once for each kind of event, so that what the script does
		// for a tag or a pull request or a schedule is in the workflow too.
		out, err = compile.GitHubEvents(*file, src, opts)
	case "circleci":
		// Compiled for a push about which nothing else is known.
		p, read, perr := plan.EvalTracked(*file, src, plan.Context{})
		if perr != nil {
			return perr
		}
		if len(p.Steps) == 0 {
			return fmt.Errorf("%s plans no steps for a push about which nothing is known; there is nothing to compile", *file)
		}
		opts.Read = read
		out, err = compile.CircleCI(p, opts)
	default:
		return fmt.Errorf("unknown target %q: github or circleci", *target)
	}
	if err != nil {
		return err
	}
	// A compiled file is a copy, and a copy goes stale the moment the script
	// is edited without it. This is the question to ask on every push; it
	// needs no network, so it downloads nothing.
	if *check != "" {
		have, err := os.ReadFile(*check)
		if err != nil {
			return err
		}
		if string(have) == out {
			fmt.Printf("%s is what %s compiles to\n", *check, *file)
			return nil
		}
		again := "starci compile"
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "check" && f.Name != "offline" {
				again += " --" + f.Name + " " + f.Value.String()
			}
		})
		return fmt.Errorf("%s is not what %s compiles to; %s\nwrite it again with: %s > %s", *check, *file, firstDifference(string(have), out), again, *check)
	}
	// What is written is run later, somewhere else. A tool that cannot be
	// fetched is better found here than in every job that wants it.
	if !*offline {
		// Every tool the script declares, not one plan's: a workflow holds
		// the steps of each kind of event. One with no build for these
		// machines is left to the compiler, which refuses a step that uses it.
		declared, err := plan.Tools(*file, src)
		if err != nil {
			return err
		}
		var tools []plan.Tool
		for _, t := range declared {
			if _, ok := t.SHA256[*arch]; ok {
				tools = append(tools, t)
			}
		}
		if failed := checkTools(os.Stderr, tools, []string{*arch}); failed > 0 {
			return fmt.Errorf("%d tool download(s) failed the check; nothing was written (--offline skips the check)", failed)
		}
	}
	fmt.Print(out)
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
	only := fs.String("arch", "", "architectures to check, comma-separated (default: every one a tool lists)")
	fs.Parse(args)

	var archs []string
	for _, a := range strings.Split(*only, ",") {
		if a = strings.TrimSpace(a); a == "amd64" || a == "arm64" {
			archs = append(archs, a)
		} else if a != "" {
			return fmt.Errorf("unknown architecture %q: amd64 or arm64", a)
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
		switch {
		case err != nil:
			failed++
			fmt.Printf("  %-8s %s\n           %v\n", "failed", s.Name, err)
		case len(missing) > 0:
			failed++
			fmt.Printf("  %-8s %s\n", "failed", s.Name)
			for _, in := range missing {
				fmt.Printf("           input %q selects no file that git tracks here\n", in)
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
