package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/tracing"
)

// pickEngine is the engine called name. The local engine is made by local,
// since how depends on the run; every other is made from the environment.
func pickEngine(name string, local func() (engine.Engine, error)) (engine.Engine, error) {
	switch name {
	case "", "local":
		return local()
	case "temporal":
		return temporalEngine()
	}
	return nil, fmt.Errorf("there is no engine called %q: there are local and temporal", name)
}

// fetchable reports why a pool's machines could not fetch from cloneURL, or
// "" if they can: a path is on this computer, and they are not.
func fetchable(cloneURL string) string {
	if filepath.IsAbs(cloneURL) || strings.HasPrefix(cloneURL, "file://") || strings.HasPrefix(cloneURL, ".") {
		return fmt.Sprintf("the pool's machines cannot fetch the commit from %s, a path on this computer; say where they can with --clone-url", cloneURL)
	}
	return ""
}

// labelFlag is key=value labels, given one at a time or comma-separated.
type labelFlag map[string]string

func (l labelFlag) String() string {
	var parts []string
	for _, k := range sortedKeys(l) {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, ",")
}

func (l labelFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			return fmt.Errorf("a label is key=value, got %q", part)
		}
		l[k] = val
	}
	return nil
}

// machineCmd makes this computer one of an engine's machines, taking steps
// from it until it is stopped.
func machineCmd(args []string) error {
	fs := flag.NewFlagSet("machine", flag.ExitOnError)
	engineName := fs.String("engine", os.Getenv("STARCI_ENGINE"), "the engine whose pool to join (default: STARCI_ENGINE)")
	host, _ := os.Hostname()
	name := fs.String("name", host, "this machine's name, in results and for clearing away what an earlier life of it left; one machine to a name")
	slots := fs.Int("slots", 4, "how many steps to run at once")
	var bare optionalBool
	fs.Var(&bare, "bare", "run bare steps, or with --bare=false refuse them (default: STARCI_BARE, else yes but for a pull request from a fork)")
	bareImage := fs.String("bare-image", os.Getenv("STARCI_BARE_IMAGE"), "run bare steps in this image instead of on the machine (default: STARCI_BARE_IMAGE)")
	verbose := fs.Bool("v", false, "print each step's output as it runs")
	labels := labelFlag{}
	if err := labels.Set(os.Getenv("STARCI_LABELS")); err != nil {
		return fmt.Errorf("STARCI_LABELS: %w", err)
	}
	fs.Var(labels, "label", "a label this machine offers a step's on, as key=value; may be given again (default: STARCI_LABELS, comma-separated). Its os and arch are known")
	fs.Parse(args)

	if *engineName == "" {
		return errors.New("say which engine's pool to join, with --engine or STARCI_ENGINE")
	}
	if *name == "" {
		return errors.New("this computer has no host name; give the machine one with --name")
	}
	eng, err := pickEngine(*engineName, func() (engine.Engine, error) {
		return nil, errors.New("the local engine has no pool to join: it runs a run's steps in the process that started it")
	})
	if err != nil {
		return err
	}
	pool, ok := eng.(engine.Pool)
	if !ok {
		return fmt.Errorf("the %s engine has no pool of machines to join", eng.Name())
	}

	cfg, err := machineConfig(*name)
	if err != nil {
		return err
	}
	cfg.BareImage = *bareImage
	// Whether bare steps run here is decided for each run, from what it is
	// for. A mistake in STARCI_BARE is found now, not at the first step.
	if _, err := bareHere(os.Getenv, plan.Context{}, bare); err != nil {
		return err
	}
	if cfg.Shared == nil {
		fmt.Println("note: no store shared with the rest of the pool (STARCI_STORE): steps that keep or are handed artifacts are refused here")
	}
	m := engine.Machine{
		Config: cfg,
		Labels: labels,
		Slots:  *slots,
		Bare: func(c plan.Context) bool {
			ok, _ := bareHere(os.Getenv, c, bare)
			return ok
		},
	}
	if *verbose {
		var mu sync.Mutex
		m.Log = func(step, line string) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Printf("  [%s] %s\n", step, line)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, flush, err := tracing.Export(ctx)
	if err != nil {
		return fmt.Errorf("setting up traces: %w", err)
	}
	defer flush()

	fmt.Printf("machine %s: taking steps from %s, %d at a time\n", *name, eng.Name(), *slots)
	return pool.Serve(ctx, m)
}
