package step

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// DefaultTimeout is how long a step's command may run on a machine that
// does not say otherwise.
const DefaultTimeout = 30 * time.Minute

// Limits is what a step may use on a machine. Each is both the most a step
// may ask for and what a step that does not ask is given: a machine that
// lets steps have 4 GB gives 4 GB to one that says nothing, and refuses one
// that asks for 8.
//
// A zero Timeout means DefaultTimeout. Zero CPUs or Memory means no limit:
// a step may ask for any amount, and one that asks for none is given none.
type Limits struct {
	Timeout time.Duration
	CPUs    float64
	Memory  int64
}

// For returns what s is to run under here: what it asked for, or failing
// that what the machine gives. A step that asks for more than the machine
// allows is refused, with a sentence that says which limit and what it is.
// It is not quietly given less: a step that says it needs 8 GB and is run
// in 4 fails later and less clearly.
func (l Limits) For(s plan.Step) (Limits, error) {
	most := l
	if most.Timeout == 0 {
		most.Timeout = DefaultTimeout
	}
	got := most
	if s.Timeout > 0 {
		got.Timeout = time.Duration(s.Timeout) * time.Second
		if got.Timeout > most.Timeout {
			return Limits{}, fmt.Errorf("it asks for a timeout of %s, and a step on this machine may have at most %s",
				plan.FormatTimeout(s.Timeout), plan.FormatTimeout(int(most.Timeout/time.Second)))
		}
	}
	if s.CPUs > 0 {
		got.CPUs = s.CPUs
		if most.CPUs > 0 && s.CPUs > most.CPUs {
			return Limits{}, fmt.Errorf("it asks for %s cpus, and a step on this machine may have at most %s", formatCPUs(s.CPUs), formatCPUs(most.CPUs))
		}
	}
	if s.Memory > 0 {
		got.Memory = s.Memory
		if most.Memory > 0 && s.Memory > most.Memory {
			return Limits{}, fmt.Errorf("it asks for %s of memory, and a step on this machine may have at most %s", plan.FormatMemory(s.Memory), plan.FormatMemory(most.Memory))
		}
	}
	return got, nil
}

// args are the docker arguments that hold a container to these limits.
func (l Limits) args() []string {
	var args []string
	if l.CPUs > 0 {
		args = append(args, "--cpus", formatCPUs(l.CPUs))
	}
	if l.Memory > 0 {
		// Swap is set to the same amount, which means none: a step held to
		// 4 GB that could use 4 GB of swap besides is not held to 4 GB.
		bytes := strconv.FormatInt(l.Memory, 10)
		args = append(args, "--memory", bytes, "--memory-swap", bytes)
	}
	return args
}

func formatCPUs(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

// outOfMemory reports whether the container was stopped for using more
// memory than it was allowed. It can only be asked of a container that
// still exists, which is why one with a memory limit is not started with
// --rm.
func outOfMemory(container string) bool {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.OOMKilled}}", container).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
