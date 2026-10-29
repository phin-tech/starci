package step

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestInputsIDFollowsTheFilesTheInputsSelect(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=a@b", "-c", "user.name=a"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, text string) {
		t.Helper()
		at := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("src/a.go", "a")
	write("docs/b.md", "b")
	git("add", "-A")
	first, err := InputsID(dir, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	write("docs/b.md", "changed")
	git("add", "-A")
	if again, _ := InputsID(dir, []string{"src"}); again != first {
		t.Error("a file outside the inputs changed the id")
	}
	write("src/a.go", "changed")
	git("add", "-A")
	if again, _ := InputsID(dir, []string{"src"}); again == first {
		t.Error("a file inside the inputs did not change the id")
	}
	if _, err := InputsID(dir, []string{"nope"}); err == nil {
		t.Error("an input that selects nothing was accepted")
	}
}
