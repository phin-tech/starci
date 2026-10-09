// Command starci plans and runs a repository's Starlark CI script.
//
//	starci plan     evaluate a script and print the plan; runs nothing
//	starci run      execute a commit of a local checkout: on Hatchet's workers,
//	                or with --local on this machine with nothing but Docker
//	starci worker   execute steps on this machine
//	starci serve    receive pushes from a forge and start a run for each
//	starci compile  write a basic script out as a GitHub Actions workflow
//	starci workers  list the workers Hatchet knows, and what they offer
//	starci shun     stop giving a worker new steps; unshun reverses it
//
// Everything but plan and run --local reaches Hatchet through the
// HATCHET_CLIENT_* variables.
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

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"git.phin.tech/sam/starci/internal/compile"
	onhatchet "git.phin.tech/sam/starci/internal/engine/hatchet"
	"git.phin.tech/sam/starci/internal/engine/local"
	"git.phin.tech/sam/starci/internal/forge"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	s3store "git.phin.tech/sam/starci/internal/store/s3"
	"git.phin.tech/sam/starci/internal/trigger"
)

const usage = `usage:
  starci plan   [--file .ci.star] [--repo owner/name] [--sha SHA] [--ref REF] [--changed a,b,c]
  starci run    CHECKOUT --clone-url URL [--sha SHA] [--ref BRANCH] [--file FILE] [--repo owner/name]
  starci run    CHECKOUT --local [-v] [--slots N] [--sha SHA] [--file FILE]
  starci worker
  starci serve
  starci compile [--file .ci.star] [--runs-on ubuntu-latest] [--arch amd64]
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
	case "serve":
		err = serveCmd()
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
	fs.Parse(args)

	c := plan.Context{Repo: *repo, SHA: *sha, Ref: *ref}
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
	c := plan.Context{Repo: *repo, SHA: commit, Ref: *ref, ChangedFiles: strings.Fields(changed)}
	mode, err := plan.Mode(*file, src)
	if err != nil {
		return err
	}

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

	started := time.Now()
	var result onhatchet.RunResult
	if *localRun {
		result, err = runLocally(mode, p, *file, src, c, *cloneURL, *slots, *verbose)
	} else {
		result, err = runOnHatchet(mode, p, *file, src, c, *cloneURL)
	}
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

// runLocally executes the run on this machine. The same script, the same
// sandbox and the same result cache as on a worker; no queue, no other
// machines, and nothing to resume from if this process is stopped.
func runLocally(mode string, p *plan.Plan, file string, src []byte, c plan.Context, cloneURL string, slots int, verbose bool) (onhatchet.RunResult, error) {
	// Named, because with a shared store this machine's results are seen
	// from others.
	host, _ := os.Hostname()
	if host == "" {
		host = "this machine"
	}
	cfg, err := machineConfig(host)
	if err != nil {
		return onhatchet.RunResult{}, err
	}
	// Ctrl-C stops the steps and removes their containers.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := &local.Runner{
		Ctx:      ctx,
		Config:   cfg,
		CloneURL: cloneURL,
		SHA:      c.SHA,
		Slots:    slots,
	}
	if verbose {
		var mu sync.Mutex
		runner.Log = func(name, line string) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Printf("  [%s] %s\n", name, line)
		}
	}

	var out onhatchet.RunResult
	if mode == plan.Advanced {
		results, err := plan.RunPipeline(ctx, filepath.Base(file), src, c, runner)
		out = onhatchet.RunResult{OK: err == nil, Steps: results}
		if err != nil {
			out.Error = err.Error()
		}
		return out, nil
	}
	results, err := plan.RunPlan(p, runner)
	if err != nil {
		return out, err
	}
	return onhatchet.RunResult{OK: plan.Passed(results), Steps: results}, nil
}

func runOnHatchet(mode string, p *plan.Plan, file string, src []byte, c plan.Context, cloneURL string) (onhatchet.RunResult, error) {
	var out onhatchet.RunResult
	// A run for a branch replaces the previous run for that branch. A run for
	// a bare commit replaces nothing but a repeat of itself.
	supersedes := c.Repo + "@" + c.SHA
	if c.Ref != "" {
		supersedes = c.Repo + "@" + c.Ref
	}

	client, err := hatchet.NewClient()
	if err != nil {
		return out, err
	}
	tasks := onhatchet.NewTasks(client, step.Config{}, nil)

	var res *hatchet.TaskResult
	if mode == plan.Advanced {
		res, err = tasks.Script.Run(context.Background(), onhatchet.ScriptInput{
			Filename: filepath.Base(file), Source: string(src), Context: c, CloneURL: cloneURL, Supersedes: supersedes,
		})
	} else {
		// A step nobody can take waits in silence until Hatchet gives up on
		// it. Say so now, while the cause is still obvious.
		if workers, werr := listWorkers(client); werr == nil {
			for _, s := range p.Steps {
				if !anyCanTake(workers, s) {
					fmt.Printf("warning: no connected worker can take step %q (needs %s); it will wait for one\n", s.Name, describePlacement(s))
				}
			}
		}
		res, err = tasks.Pipeline.Run(context.Background(), onhatchet.PipelineInput{Plan: *p, CloneURL: cloneURL, Supersedes: supersedes})
	}
	if err != nil {
		// A cancelled run comes back as an error with nothing in it.
		if strings.TrimSpace(err.Error()) == "" || strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return out, fmt.Errorf("the run was cancelled (a newer run for %s replaces it)", supersedes)
		}
		return out, err
	}
	err = res.Into(&out)
	return out, err
}

// workerCmd is configured by environment, since it normally runs as a service:
//
//	STARCI_WORKER_NAME  name recorded in results (default: hostname)
//	STARCI_STATE        where results and tools are kept (default: ~/.cache/starci)
//	STARCI_STORE        a result store shared with other machines (see machineConfig)
//	STARCI_SLOTS        steps to run at once (default: 4)
//	STARCI_LABELS       what this machine offers, as k=v,k=v (e.g. gpu=true)
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
	labels := map[string]any{}
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

	client, err := hatchet.NewClient()
	if err != nil {
		return err
	}
	forges, _, err := forgesFromEnv()
	if err != nil {
		return err
	}
	tasks := onhatchet.NewTasks(client, cfg, reporter(forges))
	worker, err := client.NewWorker("starci-"+name,
		hatchet.WithWorkflows(tasks.All()...),
		hatchet.WithSlots(slots),
		// Pipelines and scripts only wait on their steps, so they get their
		// own pool and can never occupy the slots those steps need.
		hatchet.WithDurableSlots(20),
		hatchet.WithLabels(labels),
	)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return worker.StartBlocking(ctx)
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
	return cfg, nil
}

// workerInfo is the part of a Hatchet worker record that placement depends on.
type workerInfo struct {
	id, name, status string
	labels           map[string]string
}

func listWorkers(client *hatchet.Client) ([]workerInfo, error) {
	list, err := client.Workers().List(context.Background())
	if err != nil {
		return nil, err
	}
	var out []workerInfo
	if list.Rows == nil {
		return out, nil
	}
	for _, w := range *list.Rows {
		info := workerInfo{id: w.Metadata.Id, name: w.Name, labels: map[string]string{}}
		if w.Status != nil {
			info.status = string(*w.Status)
		}
		if w.Labels != nil {
			for _, l := range *w.Labels {
				if l.Value != nil {
					info.labels[l.Key] = *l.Value
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// anyCanTake reports whether some active worker satisfies the step's hard
// rules. Preferences are ignored: they never stop a step from running.
func anyCanTake(workers []workerInfo, s plan.Step) bool {
	for _, w := range workers {
		if w.status != "ACTIVE" {
			continue
		}
		ok := true
		for k, v := range s.Worker {
			ok = ok && w.labels[k] == v
		}
		for k, v := range s.Avoid {
			ok = ok && w.labels[k] != v
		}
		if ok {
			return true
		}
	}
	return false
}

func describePlacement(s plan.Step) string {
	var parts []string
	for _, k := range sortedKeys(s.Worker) {
		parts = append(parts, k+"="+s.Worker[k])
	}
	for _, k := range sortedKeys(s.Avoid) {
		parts = append(parts, "not "+k+"="+s.Avoid[k])
	}
	if len(parts) == 0 {
		return "any worker"
	}
	return strings.Join(parts, ", ")
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
	client, err := hatchet.NewClient()
	if err != nil {
		return err
	}
	workers, err := listWorkers(client)
	if err != nil {
		return err
	}
	shown := 0
	for _, w := range workers {
		// Hatchet keeps a record of every worker that ever connected.
		if w.status == "INACTIVE" {
			continue
		}
		var labels []string
		for _, k := range sortedKeys(w.labels) {
			labels = append(labels, k+"="+w.labels[k])
		}
		state := "taking work"
		if w.status == "PAUSED" {
			state = "shunned"
		}
		fmt.Printf("%-24s %-12s %s\n", w.name, state, strings.Join(labels, " "))
		shown++
	}
	if shown == 0 {
		fmt.Println("no workers connected")
	}
	return nil
}

// shunCmd stops (or resumes) Hatchet assigning new work to a worker. Steps it
// is already running finish; nothing new arrives. For a machine that is
// misbehaving, or about to be taken down.
func shunCmd(name string, shun bool) error {
	client, err := hatchet.NewClient()
	if err != nil {
		return err
	}
	workers, err := listWorkers(client)
	if err != nil {
		return err
	}
	done := 0
	for _, w := range workers {
		if w.status == "INACTIVE" || (w.name != name && w.name != "starci-"+name) {
			continue
		}
		if shun {
			_, err = client.Workers().Pause(context.Background(), w.id)
		} else {
			_, err = client.Workers().Unpause(context.Background(), w.id)
		}
		if err != nil {
			return err
		}
		done++
	}
	if done == 0 {
		return fmt.Errorf("no connected worker is named %q", name)
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
		forges["github"] = forge.GitHub{Token: token}
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
		return f.SetStatus(ctx, to.Repo, to.SHA, s)
	}
}

// serveCmd receives webhook deliveries and submits a run for each push. It
// holds no state and runs nothing itself: it can be restarted at any time.
//
//	STARCI_LISTEN  address to listen on (default :8080)
//
// plus the forges from forgesFromEnv, each with its secret, and Hatchet's
// HATCHET_CLIENT_* variables. A forge delivers to /hooks/forgejo or
// /hooks/github.
func serveCmd() error {
	forges, secrets, err := forgesFromEnv()
	if err != nil {
		return err
	}
	hooks := map[string]trigger.Hook{}
	for name, f := range forges {
		if len(secrets[name]) == 0 {
			return fmt.Errorf("%s has no webhook secret configured; without one, anyone could start runs", name)
		}
		hooks[name] = trigger.Hook{Forge: f, Secret: secrets[name]}
	}
	if len(hooks) == 0 {
		return errors.New("no forge is configured; set STARCI_FORGEJO_URL or STARCI_GITHUB_TOKEN_FILE")
	}
	client, err := hatchet.NewClient()
	if err != nil {
		return err
	}
	tasks := onhatchet.NewTasks(client, step.Config{}, nil)

	listen := os.Getenv("STARCI_LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	mux := http.NewServeMux()
	mux.Handle("/hooks/", &trigger.Handler{Hooks: hooks, Submit: tasks, Log: func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
	}})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	var names []string
	for name := range hooks {
		names = append(names, "/hooks/"+name)
	}
	sort.Strings(names)
	fmt.Fprintf(os.Stderr, "listening on %s for %s\n", listen, strings.Join(names, ", "))
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}

// compileCmd writes a basic script out as a GitHub Actions workflow, for a
// repository hosted where starci is not running:
//
//	starci compile > .github/workflows/ci.yml
//
// Forgejo and Gitea Actions read the same file.
func compileCmd(args []string) error {
	fs := flag.NewFlagSet("compile", flag.ExitOnError)
	file := fs.String("file", ".ci.star", "CI script to compile")
	runsOn := fs.String("runs-on", "ubuntu-latest", "runner for steps that do not ask for particular workers")
	arch := fs.String("arch", "amd64", "architecture of those runners: amd64 or arm64")
	fs.Parse(args)

	src, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	// Compiled for a commit about which nothing is known: no branch, no list
	// of changes. That is the plan a script falls back to, and the only one
	// that can stand for every push.
	p, read, err := plan.EvalTracked(*file, src, plan.Context{})
	if err != nil {
		return err
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("%s plans no steps for a commit about which nothing is known; there is nothing to compile", *file)
	}
	out, err := compile.GitHub(p, compile.Options{Source: filepath.Base(*file), RunsOn: *runsOn, Arch: *arch, Read: read})
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
