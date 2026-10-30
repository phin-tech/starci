package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

// podCmd runs the parts of a step that a pod made by the Argo target cannot
// run in the step's own image (step.PreparePod, step.FinishPod):
//
//	starci pod prepare    in the init container: inputs, artifacts, tools,
//	                      and whether it passed before
//	starci pod finish     in the main container, after the step passed:
//	                      store what it keeps, record the pass
//
// Both read the step from the environment, as the template sets it:
//
//	STARCI_STEP       the step, as JSON, with its artifact() placeholders
//	STARCI_GIVEN      what the steps it needs kept, by name, as JSON
//	STARCI_CLONE_URL  the commit (prepare)
//	STARCI_SHA
//	STARCI_ARCH       the nodes' architecture (prepare)
//	STARCI_STORE      the shared store, as for a machine; STARCI_STATE where
//	                  this container keeps its own copies
func podCmd(args []string) error {
	if len(args) == 0 {
		die(usage)
	}
	fs := flag.NewFlagSet("pod", flag.ExitOnError)
	workspace := fs.String("workspace", "/workspace", "where the step's inputs go")
	shared := fs.String("shared", "/opt/starci", "what the init container leaves the main one")
	out := fs.String("out", "/tmp/starci-kept.json", "where finish writes the pointers of what the step kept")
	fs.Parse(args[1:])

	var s plan.Step
	if err := json.Unmarshal([]byte(os.Getenv("STARCI_STEP")), &s); err != nil {
		return fmt.Errorf("STARCI_STEP is not a step: %w", err)
	}
	// What the step is handed, from what the steps it needs kept.
	given := map[string]map[string]plan.Pointer{}
	if g := os.Getenv("STARCI_GIVEN"); g != "" {
		if err := json.Unmarshal([]byte(g), &given); err != nil {
			return fmt.Errorf("STARCI_GIVEN is not what the steps before kept: %w", err)
		}
	}
	byName := map[string]plan.Result{}
	for name, kept := range given {
		byName[name] = plan.Result{Name: name, Status: "passed", Artifacts: kept}
	}
	resolved, err := plan.Resolve(s, byName)
	if err != nil {
		return fmt.Errorf("step %q cannot be handed its artifacts: %w", s.Name, err)
	}

	cfg, err := machineConfig(hostName())
	if err != nil {
		return err
	}
	p := step.Pod{Step: resolved, CloneURL: os.Getenv("STARCI_CLONE_URL"), SHA: os.Getenv("STARCI_SHA"),
		Arch: os.Getenv("STARCI_ARCH"), Workspace: *workspace, Shared: *shared}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "prepare":
		if p.CloneURL == "" || p.SHA == "" || p.Arch == "" {
			return errors.New("STARCI_CLONE_URL, STARCI_SHA and STARCI_ARCH must be set")
		}
		return step.PreparePod(ctx, cfg, p)
	case "finish":
		return step.FinishPod(ctx, cfg, p, *out, func(line string) { fmt.Fprintln(os.Stderr, "starci: "+line) })
	}
	die(usage)
	return nil
}

func hostName() string {
	if host, _ := os.Hostname(); host != "" {
		return host
	}
	return "this machine"
}
