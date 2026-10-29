package step

import (
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

// What one step keeps reaches the step it is handed to, executable bits and
// directories included, and nothing else of the first step's workspace does.
func TestAStepIsHandedWhatAnotherKept(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	cfg := bareConfig(t)

	build := runBareStep(t, cfg, r, sha, plan.Step{Name: "build", Keep: []string{"out", "notes.txt"},
		Run: `mkdir -p out/lib && printf '#!/bin/sh\necho hello\n' > out/app && chmod +x out/app && echo x > out/lib/x && echo n > notes.txt && echo scratch > scratch`})
	if build.Status != "passed" {
		t.Fatalf("build: %s: %s", build.Status, build.Detail)
	}
	out, notes := build.Artifacts["out"], build.Artifacts["notes.txt"]
	if out.Hash == "" || notes.Hash == "" || out.Size == 0 {
		t.Fatalf("build kept %+v, want out and notes.txt", build.Artifacts)
	}

	test := runBareStep(t, cfg, r, sha, plan.Step{Name: "test",
		Artifacts: map[string]plan.Pointer{"bin": out, "docs/notes.txt": notes},
		Run:       `test "$(./bin/app)" = hello && test -f bin/lib/x && test "$(cat docs/notes.txt)" = n && test ! -e scratch`})
	if test.Status != "passed" {
		t.Fatalf("test: %s: %s", test.Status, test.Detail)
	}
}

// The same files kept twice are the same pointer, which is what lets a step
// handed them be reused after the step before it ran again.
func TestKeepingTheSameFilesIsTheSamePointer(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	cfg := bareConfig(t)
	s := plan.Step{Name: "build", Keep: []string{"out"}, Run: `mkdir out && echo same > out/f`}
	first := runBareStep(t, cfg, r, sha, s)
	again := runBareStep(t, cfg, r, sha, s)
	if first.Artifacts["out"].Hash == "" || first.Artifacts["out"] != again.Artifacts["out"] {
		t.Errorf("kept %+v then %+v, want the same pointer", first.Artifacts["out"], again.Artifacts["out"])
	}
	other := s
	other.Artifacts = map[string]plan.Pointer{"in": first.Artifacts["out"]}
	if bareKey(s, "p", "c", nil) == bareKey(other, "p", "c", nil) {
		t.Error("a step handed an artifact has the key of one that is not")
	}
}

func TestKeepingWhatAStepDidNotMakeFailsIt(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	res := runBareStep(t, bareConfig(t), r, sha, plan.Step{Name: "build", Keep: []string{"out/app"}, Run: "true"})
	if res.Status != "failed" || res.Detail == "" {
		t.Errorf("result = %+v, want a failure saying out/app was never made", res)
	}
}

func TestAnArtifactDoesNotLandOnAnInput(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"bin/app": "from the repository"})
	cfg := bareConfig(t)
	build := runBareStep(t, cfg, r, sha, plan.Step{Name: "build", Keep: []string{"app"}, Inputs: []string{"bin"}, Run: "echo built > app"})
	if build.Status != "passed" {
		t.Fatalf("build: %s", build.Detail)
	}
	res := runBareStep(t, cfg, r, sha, plan.Step{Name: "test", Artifacts: map[string]plan.Pointer{"bin/app": build.Artifacts["app"]}, Run: "true"})
	if res.Status != "failed" {
		t.Errorf("result = %+v, want a refusal to overwrite bin/app", res)
	}
}
