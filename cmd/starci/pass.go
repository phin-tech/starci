package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

// passCmd is how a step run by something other than `starci step` (a pod made
// by the Argo target) asks for, and leaves, a record of a pass in the store a
// machine uses (STARCI_STORE):
//
//	starci pass key [DIR] < step      the key the step's pass is stored under
//	starci pass get KEY               exit 0 if there is a pass, 1 if not
//	starci pass put KEY [NAME]        record a pass
//
// The step is a plan.Step as JSON, and DIR the checkout its inputs are in.
func passCmd(args []string) error {
	if len(args) == 0 {
		die(usage)
	}
	cfg, err := machineConfig(hostName())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch args[0] {
	case "key":
		dir := "."
		if len(args) > 1 {
			dir = args[1]
		}
		var s plan.Step
		if err := readJSON(os.Stdin, &s, false); err != nil {
			return fmt.Errorf("reading the step: %w", err)
		}
		content, err := step.InputsID(dir, s.Inputs)
		if err != nil {
			return err
		}
		arch := os.Getenv("STARCI_ARCH")
		if arch == "" {
			return errors.New("STARCI_ARCH is not set: the nodes' architecture, amd64 or arm64")
		}
		fmt.Println(step.PodKey(s, arch, content))
		return nil
	case "get":
		if len(args) != 2 {
			die(usage)
		}
		if _, ok := cfg.Results().Get(ctx, args[1]); !ok {
			os.Exit(1)
		}
		return nil
	case "put":
		if len(args) < 2 || len(args) > 3 {
			die(usage)
		}
		name := ""
		if len(args) == 3 {
			name = args[2]
		}
		return cfg.Results().Put(ctx, args[1], plan.Result{Name: name, Status: "passed", Machine: hostName()})
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
