package step

import (
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

func TestPodKeyChangesWithWhatDecidesTheOutcome(t *testing.T) {
	base := plan.Step{Name: "a", Image: "alpine:3", Run: "make", Env: map[string]string{"A": "1"}}
	k := PodKey(base, "amd64", "content")
	if PodKey(base, "amd64", "content") != k {
		t.Fatal("the key is not stable")
	}
	renamed := base
	renamed.Name = "b"
	if PodKey(renamed, "amd64", "content") != k {
		t.Error("renaming a step changed its key")
	}
	for name, changed := range map[string]string{
		"arch":    PodKey(base, "arm64", "content"),
		"content": PodKey(base, "amd64", "other"),
		"image":   PodKey(plan.Step{Image: "alpine:4", Run: "make", Env: base.Env}, "amd64", "content"),
		"command": PodKey(plan.Step{Image: "alpine:3", Run: "make test", Env: base.Env}, "amd64", "content"),
		"env":     PodKey(plan.Step{Image: "alpine:3", Run: "make", Env: map[string]string{"A": "2"}}, "amd64", "content"),
	} {
		if changed == k {
			t.Errorf("a different %s gave the same key", name)
		}
	}
	if k == key(base, image{}, "content", nil, nil) {
		t.Error("a pod's key is a machine's")
	}
}
