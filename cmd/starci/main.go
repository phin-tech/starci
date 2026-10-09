// Command starci plans and runs a repository's Starlark CI script.
//
//	starci plan    evaluate a script and print the plan; runs nothing
//	starci run     plan a commit of a local checkout and execute it on Hatchet
//	starci worker  execute steps on this machine
//
// run and worker reach Hatchet through the usual HATCHET_CLIENT_* variables.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"git.phin.tech/sam/starci/internal/executor"
	"git.phin.tech/sam/starci/internal/plan"
)

const usage = `usage:
  starci plan   [--file .ci.star] [--repo owner/name] [--sha SHA] [--ref REF] [--changed a,b,c]
  starci run    CHECKOUT --clone-url URL [--sha SHA] [--file FILE] [--repo owner/name]
  starci worker`

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
	cloneURL := fs.String("clone-url", "", "URL workers fetch the commit from")
	sha := fs.String("sha", "HEAD", "commit to build")
	file := fs.String("file", "", "CI script (default: .ci.star in the checkout)")
	repo := fs.String("repo", "", "owner/name, for display (default: from the clone URL)")
	// The checkout comes first on the command line, before the flags.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		die(usage)
	}
	dir := args[0]
	fs.Parse(args[1:])
	if *cloneURL == "" {
		die(usage)
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
	if *repo == "" {
		parts := strings.Split(strings.TrimSuffix(*cloneURL, ".git"), "/")
		if len(parts) >= 2 {
			*repo = strings.Join(parts[len(parts)-2:], "/")
		}
	}

	p, err := evalFile(*file, plan.Context{Repo: *repo, SHA: commit, ChangedFiles: strings.Fields(changed)})
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

	client, err := hatchet.NewClient()
	if err != nil {
		return err
	}
	_, pipeline := executor.Tasks(client, executor.Config{})

	started := time.Now()
	res, err := pipeline.Run(context.Background(), executor.PipelineInput{Plan: *p, CloneURL: *cloneURL})
	if err != nil {
		return err
	}
	var result executor.PipelineResult
	if err := res.Into(&result); err != nil {
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

// workerCmd is configured by environment, since it normally runs as a service:
//
//	STARCI_WORKER_NAME  name recorded in results (default: hostname)
//	STARCI_STATE        where passing results are kept (default: ~/.cache/starci)
//	STARCI_SLOTS        steps to run at once (default: 4)
//	STARCI_LABELS       what this machine offers, as k=v,k=v (e.g. gpu=true)
func workerCmd() error {
	name := os.Getenv("STARCI_WORKER_NAME")
	if name == "" {
		name, _ = os.Hostname()
	}
	state := os.Getenv("STARCI_STATE")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		state = filepath.Join(home, ".cache", "starci")
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
	step, pipeline := executor.Tasks(client, executor.Config{WorkerName: name, StateDir: state})
	worker, err := client.NewWorker("starci-"+name,
		hatchet.WithWorkflows(step, pipeline),
		hatchet.WithSlots(slots),
		// Pipelines only wait on their steps, so they get their own pool and
		// can never occupy the slots those steps need.
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
