package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
	"git.phin.tech/sam/starci/internal/tracing"
)

// A run is one trace: a span for the run, and under it, for each step, how
// long it waited and the step itself with its parts inside.
func TestARunIsOneTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("runs containers; skipped with -short")
	}
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
	seen := tracetest.NewInMemoryExporter()
	tracing.Use(sdktrace.WithSyncer(seen))
	t.Cleanup(func() { tracing.Use() })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "start"}} {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
	}
	sha, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()

	p := &plan.Plan{Repo: "test/repo", SHA: strings.TrimSpace(string(sha)), Steps: []plan.Step{
		{Name: "good", Image: "alpine:3", Run: "true", Inputs: []string{"."}},
		{Name: "bad", Image: "alpine:3", Run: "exit 2", Inputs: []string{"."}},
	}}
	eng := &Engine{Config: step.Config{Worker: "test", StateDir: t.TempDir()}, Slots: 2}
	_, err := eng.Execute(context.Background(), engine.Run{Mode: plan.Basic, Plan: p, CloneURL: dir, Context: plan.Context{Repo: p.Repo, SHA: p.SHA}})
	if err != nil {
		t.Fatal(err)
	}

	spans := seen.GetSpans()
	names := map[string]int{}
	traces := map[string]bool{}
	var root tracetest.SpanStub
	for _, s := range spans {
		names[s.Name]++
		traces[s.SpanContext.TraceID().String()] = true
		if !s.Parent.IsValid() {
			root = s
		}
	}
	if len(traces) != 1 {
		t.Errorf("the run's spans are in %d traces, want 1", len(traces))
	}
	for name, want := range map[string]int{"run test/repo": 1, "queued": 2, "step good": 1, "step bad": 1, "fetch the commit": 2, "run the command": 2} {
		if names[name] != want {
			var all []string
			for n, c := range names {
				all = append(all, n+"×"+string(rune('0'+c)))
			}
			sort.Strings(all)
			t.Errorf("%d span(s) named %q, want %d (have: %s)", names[name], name, want, strings.Join(all, ", "))
		}
	}
	if root.Name != "run test/repo" {
		t.Fatalf("the span with no parent is %q, want the run", root.Name)
	}
	attr := func(s tracetest.SpanStub, key string) string {
		for _, a := range s.Attributes {
			if string(a.Key) == key {
				return a.Value.Emit()
			}
		}
		return ""
	}
	if attr(root, "starci.ok") != "false" {
		t.Errorf("the run's span says ok = %q for a run with a failed step", attr(root, "starci.ok"))
	}
	for _, s := range spans {
		// Steps and their waits hang from the run, which is recorded last
		// and so is known to them only by the id it will have.
		if strings.HasPrefix(s.Name, "step ") || s.Name == "queued" {
			if s.Parent.SpanID() != root.SpanContext.SpanID() {
				t.Errorf("%q is not under the run", s.Name)
			}
		}
		if s.Name == "step bad" && (attr(s, "starci.status") != "failed" || attr(s, "starci.worker") != "test") {
			t.Errorf("the failed step's span says status %q on worker %q", attr(s, "starci.status"), attr(s, "starci.worker"))
		}
	}
}
