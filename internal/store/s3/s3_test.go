package s3

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

func TestMain(m *testing.M) {
	// The steps here are bare, and a watchdog over one is this binary.
	step.Watchdog()
	os.Exit(m.Run())
}

// open returns the store STARCI_TEST_S3 names, with its bucket made, or
// skips: these need an S3 to talk to (docs/development.md).
func open(t *testing.T) *Store {
	t.Helper()
	location := os.Getenv("STARCI_TEST_S3")
	if location == "" {
		t.Skip("STARCI_TEST_S3 is not set")
	}
	s, err := Open(location)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if ok, err := s.client.BucketExists(ctx, s.bucket); err != nil {
		t.Fatalf("reaching %s: %v", location, err)
	} else if !ok {
		if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Each test in a prefix of its own, so that nothing an earlier one
	// stored is found.
	s.prefix = strings.Trim(s.prefix+"/"+strings.ReplaceAll(t.Name(), "/", "-")+"-"+time.Now().Format("150405.000000000"), "/")
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAResultIsStoredAndFound(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	key := strings.Repeat("ab", 32)
	if _, ok := s.Get(ctx, key); ok {
		t.Fatal("found a result before one was stored")
	}
	want := plan.Result{Name: "build", Status: "passed", Output: "made\n", Machine: "a",
		Artifacts: map[string]plan.Pointer{"out": {Hash: strings.Repeat("cd", 32), Size: 5}}}
	if err := s.Put(ctx, key, want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(ctx, key)
	if !ok || got.Output != want.Output || got.Artifacts["out"] != want.Artifacts["out"] {
		t.Errorf("got %+v, %v; want %+v", got, ok, want)
	}
}

func TestABlobIsStoredOnceAndFetched(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "in")
	if err := os.WriteFile(file, []byte("contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ef", 32)
	for i := 0; i < 2; i++ {
		if err := s.PutBlob(ctx, hash, file); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	out := filepath.Join(dir, "out")
	if err := s.GetBlob(ctx, hash, out); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); string(data) != "contents\n" {
		t.Errorf("fetched %q", data)
	}
	if err := s.GetBlob(ctx, strings.Repeat("00", 32), filepath.Join(dir, "missing")); err == nil {
		t.Error("fetched a blob that was never stored")
	}
}

// Two machines that share nothing but the store: what a step kept on one is
// handed to a step on the other, and a pass on one is reused on the other.
func TestMachinesShareArtifactsAndPassesThroughTheStore(t *testing.T) {
	s := open(t)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "a"}, {"commit", "-q", "-m", "a"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sha, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	machine := func(name string) step.Config {
		return step.Config{Machine: name, StateDir: t.TempDir(), Shared: s, Bare: true}
	}
	run := func(cfg step.Config, st plan.Step) plan.Result {
		t.Helper()
		st.Inputs = []string{"."}
		return step.Run(context.Background(), cfg, repo, strings.TrimSpace(string(sha)), st, func(string) {})
	}
	a, b := machine("a"), machine("b")

	build := plan.Step{Name: "build", Keep: []string{"out"}, Cache: true,
		Run: `mkdir out && printf '#!/bin/sh\necho made\n' > out/app && chmod +x out/app`}
	built := run(a, build)
	if built.Status != "passed" || built.Artifacts["out"].Hash == "" {
		t.Fatalf("build on a: %+v", built)
	}
	used := run(b, plan.Step{Name: "use", Artifacts: map[string]plan.Pointer{"bin": built.Artifacts["out"]},
		Run: `test "$(./bin/app)" = made`})
	if used.Status != "passed" {
		t.Fatalf("use on b, handed what a kept: %s: %s", used.Status, used.Detail)
	}
	again := run(b, build)
	if !again.Cached || again.Machine != "a" || again.Artifacts["out"] != built.Artifacts["out"] {
		t.Errorf("build on b = %+v; want a's pass, reused, with the same pointer", again)
	}
}
