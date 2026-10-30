package step

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

// sharedDir is a store other machines would share, kept in a directory.
type sharedDir struct {
	FileStore
	FileBlobs
}

func newShared(t *testing.T) sharedDir {
	dir := t.TempDir()
	return sharedDir{FileStore{Dir: filepath.Join(dir, "results")}, FileBlobs{Dir: filepath.Join(dir, "blobs")}}
}

// pod plays a pod: a workspace and a shared directory of its own, nothing in
// common with another pod but the store.
func pod(t *testing.T, store Store, r *repo, sha string, s plan.Step) (Pod, Config) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "opt-starci")
	p := Pod{Step: s, CloneURL: r.dir, SHA: sha, Arch: "amd64", Workspace: filepath.Join(root, "workspace"), Shared: shared}
	return p, Config{Machine: "pod", StateDir: filepath.Join(shared, "state"), Shared: store}
}

// What one pod keeps reaches the pod handed it, through the store alone; and a
// pod that passed before hands on what it kept then without running.
func TestPodsHandArtifactsOnThroughTheStore(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"src/a.txt": "a", "other.txt": "o"})
	store := newShared(t)
	ctx := context.Background()

	build := plan.Step{Name: "build", Inputs: []string{"src"}, Keep: []string{"out"}, Cache: true}
	p, cfg := pod(t, store, r, sha, build)
	if err := PreparePod(ctx, cfg, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Workspace, "src", "a.txt")); err != nil {
		t.Fatalf("the input is not in the workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Workspace, "other.txt")); err == nil {
		t.Fatal("a file that is not an input is in the workspace")
	}
	// The step's own image runs its command...
	os.MkdirAll(filepath.Join(p.Workspace, "out"), 0o755)
	os.WriteFile(filepath.Join(p.Workspace, "out", "app"), []byte("#!/bin/sh\necho made\n"), 0o755)
	// ...and the main container finishes it.
	kept := filepath.Join(t.TempDir(), "kept.json")
	if err := FinishPod(ctx, cfg, p, kept, func(l string) { t.Log(l) }); err != nil {
		t.Fatal(err)
	}
	var pointers map[string]plan.Pointer
	data, _ := os.ReadFile(kept)
	if err := json.Unmarshal(data, &pointers); err != nil || pointers["out"].Hash == "" {
		t.Fatalf("kept %s (%v)", data, err)
	}

	use := plan.Step{Name: "use", Inputs: []string{"src"}, Artifacts: map[string]plan.Pointer{"bin": pointers["out"]}}
	p2, cfg2 := pod(t, store, r, sha, use)
	if err := PreparePod(ctx, cfg2, p2); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(p2.Workspace, "bin", "app"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("the artifact is not in the next pod's workspace, executable: %v %v", info, err)
	}

	again, cfg3 := pod(t, store, r, sha, build)
	if err := PreparePod(ctx, cfg3, again); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(again.Shared, PodHit)); err != nil {
		t.Fatal("a pod whose step passed before with the same inputs is not told so")
	}
	data, _ = os.ReadFile(filepath.Join(again.Shared, PodKept))
	var before map[string]plan.Pointer
	if json.Unmarshal(data, &before); before["out"] != pointers["out"] {
		t.Errorf("a pod that passed before hands on %s, want what it kept then", data)
	}
}

func TestAPodCannotMoveFilesWithoutAStore(t *testing.T) {
	r := newRepo(t)
	sha := r.commit(map[string]string{"a": "a"})
	p, cfg := pod(t, nil, r, sha, plan.Step{Name: "build", Inputs: []string{"."}, Keep: []string{"out"}})
	cfg.Shared = nil
	if err := PreparePod(context.Background(), cfg, p); err == nil || !strings.Contains(err.Error(), "shared store") {
		t.Errorf("error = %v", err)
	}
}
