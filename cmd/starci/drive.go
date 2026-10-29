package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	"git.phin.tech/sam/starci/internal/tracing"
)

// next and step are a run taken apart, for something other than starci to
// carry out: next says what to run, given the results so far, and step runs
// one step on this machine. Whatever calls them keeps the results, and is the
// record a run resumes from (docs/driving.md).

// nextState is what `starci next` reads: nothing the first time, then the
// context and clone URL the first call worked out, with the results so far.
// The context travels so that every call tells the script the same thing,
// whichever machine makes it.
type nextState struct {
	Context  *plan.Context `json:"context,omitempty"`
	CloneURL string        `json:"clone_url,omitempty"`
	Results  []plan.Result `json:"results,omitempty"`
}

// nextAnswer is what it prints: the decision, and what to hand back.
type nextAnswer struct {
	plan.Decision
	Context  plan.Context `json:"context"`
	CloneURL string       `json:"clone_url"`
}

func nextCmd(args []string) error {
	fs := flag.NewFlagSet("next", flag.ExitOnError)
	rf := addRunFlags(fs)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		die(usage)
	}
	dir := args[0]
	fs.Parse(args[1:])

	var in nextState
	if err := readJSON(os.Stdin, &in, true); err != nil {
		return fmt.Errorf("reading what is done so far: %w", err)
	}
	co, err := rf.resolve(dir)
	if err != nil {
		return err
	}
	out := nextAnswer{Context: co.context, CloneURL: co.cloneURL}
	if in.Context != nil {
		out.Context, out.CloneURL = *in.Context, in.CloneURL
	}

	if co.mode == plan.Advanced {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		out.Decision, err = plan.NextInPipeline(ctx, filepath.Base(co.file), co.src, out.Context, in.Results)
	} else {
		var p *plan.Plan
		if p, err = plan.Eval(co.file, co.src, out.Context); err == nil {
			out.Decision, err = p.Next(in.Results)
		}
	}
	if err != nil {
		return err
	}
	return writeJSON(out)
}

// stepJob is what `starci step` reads: one step as next asked for it, and
// the run it belongs to.
type stepJob struct {
	Step     plan.Asked   `json:"step"`
	Context  plan.Context `json:"context"`
	CloneURL string       `json:"clone_url"`
	// Trace places the step's spans in a run's trace. Optional.
	Trace tracing.Carrier `json:"trace,omitzero"`
}

func stepCmd(args []string) error {
	fs := flag.NewFlagSet("step", flag.ExitOnError)
	var bare optionalBool
	fs.Var(&bare, "bare", "run a bare step, or with --bare=false refuse it (default: STARCI_BARE, else yes but for a pull request from a fork)")
	bareImage := fs.String("bare-image", os.Getenv("STARCI_BARE_IMAGE"), "run a bare step in this image instead of on the machine (default: STARCI_BARE_IMAGE)")
	fs.Parse(args)

	var job stepJob
	if err := readJSON(os.Stdin, &job, false); err != nil {
		return fmt.Errorf("reading the step: %w", err)
	}
	switch {
	case job.Step.Number < 1:
		return errors.New("the step has no number; hand step what next asked for")
	case job.CloneURL == "" || job.Context.SHA == "":
		return errors.New("the step needs the run's clone_url and context, as next printed them")
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "this machine"
	}
	cfg, err := machineConfig(host)
	if err != nil {
		return err
	}
	if cfg.Bare, err = bareHere(os.Getenv, job.Context, bare); err != nil {
		return err
	}
	cfg.BareImage = *bareImage

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, flush, err := tracing.Export(ctx)
	if err != nil {
		return fmt.Errorf("setting up traces: %w", err)
	}
	defer flush()

	s := job.Step.Step
	stepCtx, end := ctx, func(plan.Result) {}
	if job.Trace.Parent != "" {
		stepCtx, end = job.Trace.Step(ctx, s.Name)
	}
	// Lines go to stderr as they come, for whatever runs this to keep;
	// stdout is the result alone.
	result := step.Run(stepCtx, cfg, job.CloneURL, job.Context.SHA, s, func(line string) {
		fmt.Fprintln(os.Stderr, line)
	})
	end(result)
	return writeJSON(job.Step.Answer(result))
}

// sweepCmd removes what an earlier life of this machine left running: the
// services of a step whose machine was killed (step.Sweep). A program that
// keeps machines runs it as each one starts, before it takes work, and only
// where the name belongs to this machine alone.
func sweepCmd(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() > 1 {
		die(usage)
	}
	machine := fs.Arg(0)
	if machine == "" {
		// The name `starci step` gives its machine.
		machine, _ = os.Hostname()
	}
	if machine == "" {
		return errors.New("this machine has no name; give one")
	}
	step.Sweep(machine)
	return nil
}

func readJSON(r io.Reader, v any, emptyOK bool) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		if emptyOK {
			return nil
		}
		return errors.New("nothing on stdin")
	}
	return json.Unmarshal(data, v)
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false) // commands contain && and <, which are not HTML here
	return enc.Encode(v)
}
