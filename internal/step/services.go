package step

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// How long a service is given to become ready before the step is failed.
var readyWithin = 2 * time.Minute

const (
	// How often its readiness command is tried.
	readyEvery = 500 * time.Millisecond
	// How much of a service's output is shown when it is to blame.
	serviceTail = "20"
	// ownerLabel is put on everything made for a step with services, naming
	// the worker that made it, so that what a worker left behind when it was
	// killed can be found. See Sweep.
	ownerLabel = "starci.worker"
)

// Sweep removes what an earlier life of this worker left running: a worker
// that is killed partway through a step cannot stop that step's services, and
// unlike the step's own container they never end by themselves. Each also
// holds a network, of which a machine has only a few dozen to give.
//
// It is for a worker to call as it starts, before it takes any work, and only
// where the name is this worker's alone: it cannot tell a dead worker's
// services from those of a live one with the same name.
func Sweep(worker string) {
	owned := "label=" + ownerLabel + "=" + worker
	if out, err := exec.Command("docker", "ps", "-aq", "--filter", owned).Output(); err == nil {
		for _, id := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
		}
	}
	if out, err := exec.Command("docker", "network", "ls", "-q", "--filter", owned).Output(); err == nil {
		for _, id := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "network", "rm", id).Run()
		}
	}
}

// sidecars is a step's services, running on a network of their own.
type sidecars struct {
	// network is what the step's container joins to reach them.
	network    string
	containers map[string]string // service name -> container
}

// startServices puts a step's services on a network made for this step alone
// and returns once each one says it is ready. Everything is named after the
// step's container, so steps running side by side on one machine each get
// their own services and cannot reach each other's.
//
// stop must be called whether or not an error is returned: a service that
// started before another failed is still running.
func startServices(ctx context.Context, base, worker string, s plan.Step) (up sidecars, stop func(), err error) {
	up = sidecars{network: base, containers: map[string]string{}}
	stop = func() {
		// Not tied to ctx: a cancelled run is exactly when this has to work.
		for _, c := range up.containers {
			_ = exec.Command("docker", "rm", "-f", "-v", c).Run()
		}
		_ = exec.Command("docker", "network", "rm", up.network).Run()
	}

	owner := ownerLabel + "=" + worker
	create := []string{"network", "create", "--label", owner}
	if s.Offline {
		// A step without the network still reaches its services, and they
		// reach nothing else either.
		create = append(create, "--internal")
	}
	if out, err := exec.Command("docker", append(create, up.network)...).CombinedOutput(); err != nil {
		// Most often the machine has run out: Docker has about thirty
		// networks to give unless it is configured with more.
		return up, stop, fmt.Errorf("cannot make a network for the step's services: %s", strings.TrimSpace(string(out)))
	}

	for _, sv := range s.Services {
		container := base + "-" + sv.Name
		args := []string{"run", "-d", "--name", container, "--network", up.network, "--network-alias", sv.Name, "--hostname", sv.Name, "--label", owner}
		for _, name := range sortedKeys(sv.Env) {
			args = append(args, "-e", name+"="+sv.Env[name])
		}
		args = append(append(args, sv.Image), sv.Args...)
		// Recorded before it is known to have started: a container that was
		// created and then failed to start is still there to remove.
		up.containers[sv.Name] = container
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			return up, stop, fmt.Errorf("service %q could not be started: %s", sv.Name, strings.TrimSpace(string(out)))
		}
	}
	for _, sv := range s.Services {
		if err := up.waitFor(ctx, sv); err != nil {
			return up, stop, err
		}
	}
	return up, stop, nil
}

// waitFor returns when the service is ready. One with a readiness command is
// ready when that succeeds; one without, when its image's own health check
// says so; and one whose image has none, as soon as it is running.
func (up sidecars) waitFor(ctx context.Context, sv plan.Service) error {
	container := up.containers[sv.Name]
	deadline := time.Now().Add(readyWithin)
	for {
		state, err := exec.Command("docker", "inspect", "--format",
			"{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", container).Output()
		running, health, _ := strings.Cut(strings.TrimSpace(string(state)), " ")
		if err != nil || running != "true" {
			return fmt.Errorf("service %q stopped before the step could use it\n%s", sv.Name, up.tail(sv.Name))
		}
		asked := "its image's health check never passed"
		if sv.Ready != "" {
			asked = "`" + sv.Ready + "` never succeeded in its container"
			// One try may not outlast what is left of the wait: a command
			// that hangs, asking a server that accepts and never answers, is
			// not ready.
			attempt, cancel := context.WithDeadline(ctx, deadline)
			err = exec.CommandContext(attempt, "docker", "exec", container, "sh", "-c", sv.Ready).Run()
			cancel()
			if err == nil {
				return nil
			}
		} else if health == "" || health == "healthy" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %q was not ready after %s: %s\n%s", sv.Name, readyWithin, asked, up.tail(sv.Name))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyEvery):
		}
	}
}

// tail is the last of what a service printed.
func (up sidecars) tail(name string) string {
	out, _ := exec.Command("docker", "logs", "--tail", serviceTail, up.containers[name]).CombinedOutput()
	return strings.TrimRight(string(out), "\n")
}
