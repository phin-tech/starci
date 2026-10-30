package step

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// A step compiled to an Argo task is run in a pod, not by Run: an init
// container, in an image that holds starci, runs PreparePod, the step's own
// image runs its command, and then FinishPod if the step keeps something or
// remembers its passes. What a machine does around a step is done here with
// the same code (the checkout, the inputs' identity, tools, artifacts), so a
// pod is given exactly what a machine would give the step.

// Pod is a step to be run in a pod.
type Pod struct {
	// Step is the step, its artifacts resolved (plan.Resolve).
	Step plan.Step
	// CloneURL and SHA are the commit.
	CloneURL, SHA string
	// Arch is the nodes' architecture, for the step's tools and its key.
	Arch string
	// Workspace is where the step's inputs and artifacts go: the directory
	// the step's own container works in.
	Workspace string
	// Shared is what the init container leaves the main one: tools, the
	// key of a pass, whether it passed before, and starci itself.
	Shared string
}

// Files in Pod.Shared.
const (
	// PodHit is there when the step passed before with the same inputs.
	PodHit = "hit"
	// podKeyFile holds the key its pass is stored under.
	podKeyFile = "key"
	// PodKept holds the pointers a step that passed before kept then.
	PodKept = "kept.json"
	// PodPath holds the directories of its tools, for PATH.
	PodPath = "path"
	// PodStarci is starci, for FinishPod to be run from the step's image.
	PodStarci = "starci"
)

// PreparePod puts a step's inputs, artifacts and tools in place, unless it
// passed before, which it records in p.Shared. cfg.StateDir must be under
// p.Shared: the tools are installed there.
func PreparePod(ctx context.Context, cfg Config, p Pod) error {
	s := p.Step
	if err := os.MkdirAll(p.Shared, 0o755); err != nil {
		return err
	}
	moving := len(s.Keep) > 0 || len(s.Artifacts) > 0
	if moving && cfg.Shared == nil {
		return errors.New("a step that keeps or is handed artifacts is run in a pod only with a shared store (the `store` parameter): nothing else carries files from one pod to another")
	}
	gitRoot, err := os.MkdirTemp("", "starci-git-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(gitRoot)
	root, err := os.MkdirTemp("", "starci-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	co, err := fetch(p.CloneURL, p.SHA, gitRoot, root, cfg.GitEnv)
	if err != nil {
		return fmt.Errorf("fetch of %s failed: %v", p.SHA, err)
	}
	content, err := co.contentID(s.Inputs)
	if err != nil {
		return err
	}

	if s.Cache && cfg.Shared != nil {
		key := PodKey(s, p.Arch, content)
		if err := os.WriteFile(filepath.Join(p.Shared, podKeyFile), []byte(key), 0o644); err != nil {
			return err
		}
		if earlier, ok := cfg.Results().Get(ctx, key); ok {
			// Nothing more to set up: the main container sees the mark,
			// hands on what was kept then, and ends.
			if err := writePointers(filepath.Join(p.Shared, PodKept), earlier.Artifacts); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(p.Shared, PodHit), nil, 0o644); err != nil {
				return err
			}
			return readable(p.Shared)
		}
	}

	if err := co.materialiseInto(p.Workspace, s.Inputs); err != nil {
		return fmt.Errorf("checkout failed: %v", err)
	}
	if len(s.Artifacts) > 0 {
		if err := give(ctx, cfg.blobs(), p.Workspace, s.Artifacts); err != nil {
			return err
		}
	}
	var dirs []string
	for _, t := range s.Tools {
		in, err := install(cfg.StateDir, t, "linux/"+p.Arch)
		if err != nil {
			return err
		}
		for _, b := range in.bins {
			dirs = append(dirs, filepath.Join(in.dir, b))
		}
	}
	if err := os.WriteFile(filepath.Join(p.Shared, PodPath), []byte(strings.Join(dirs, ":")), 0o644); err != nil {
		return err
	}
	if moving || s.Cache {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		if err := copyExecutable(self, filepath.Join(p.Shared, PodStarci)); err != nil {
			return err
		}
	}
	// The step's image may run as any user.
	if out, err := exec.Command("chmod", "-R", "a+rwX", p.Workspace).CombinedOutput(); err != nil {
		return fmt.Errorf("chmod: %s", out)
	}
	return readable(p.Shared)
}

// FinishPod is what follows a step that passed in a pod: what it keeps is
// stored, and its pointers written to out, for the tasks after it; and the
// pass is recorded, if PreparePod keyed it. Not being able to record a pass
// is said, but the step passed.
func FinishPod(ctx context.Context, cfg Config, p Pod, out string, log func(string)) error {
	s := p.Step
	var kept map[string]plan.Pointer
	if len(s.Keep) > 0 {
		if cfg.Shared == nil {
			return errors.New("a step that keeps something is run in a pod only with a shared store (the `store` parameter)")
		}
		var err error
		if kept, err = keep(ctx, cfg.blobs(), p.Workspace, s.Keep); err != nil {
			return err
		}
	}
	if err := writePointers(out, kept); err != nil {
		return err
	}
	if key, err := os.ReadFile(filepath.Join(p.Shared, podKeyFile)); err == nil && len(key) > 0 {
		r := plan.Result{Name: s.Name, Status: "passed", Machine: cfg.Machine, Artifacts: kept}
		if err := cfg.Results().Put(ctx, string(key), r); err != nil {
			log("could not record the pass, so the step will run again next time: " + err.Error())
		}
	}
	return nil
}

func writePointers(file string, pointers map[string]plan.Pointer) error {
	if pointers == nil {
		pointers = map[string]plan.Pointer{}
	}
	data, err := json.Marshal(pointers)
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o644)
}

func readable(dir string) error {
	if out, err := exec.Command("chmod", "-R", "a+rX", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("chmod: %s", out)
	}
	return nil
}

func copyExecutable(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
