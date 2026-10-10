package step

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

// given is what a step that takes every output of a result is handed.
func given(from plan.Result) []plan.Given {
	var out []plan.Given
	for path, digest := range from.Outputs {
		out = append(out, plan.Given{Step: from.Name, Path: path, Digest: digest})
	}
	return out
}

func TestAnOutputReachesTheStepGivenIt(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src.txt": "one\n", "other.txt": "x"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	build := plan.Step{Name: "build", Inputs: []string{"src.txt"}, Outputs: []string{"dist"}, Cache: true,
		Run: "mkdir -p dist/sub && tr a-z A-Z < src.txt > dist/out.txt && echo deep > dist/sub/deep.txt && chmod 755 dist/out.txt"}

	built := run(t, cfg, r, sha, build)
	if built.Status != "passed" || built.Outputs["dist"] == "" {
		t.Fatalf("build = %+v, want a pass that kept dist", built)
	}

	// Given nothing from the repository at all: only what build made.
	use := plan.Step{Name: "use", Inputs: []string{}, Given: given(built), Cache: true,
		Run: "cat dist/out.txt dist/sub/deep.txt; test -x dist/out.txt && echo executable; ls"}
	got := run(t, cfg, r, sha, use)
	if got.Status != "passed" || got.Output != "ONE\ndeep\nexecutable\ndist\n" {
		t.Fatalf("use = %+v, want it to read what build produced and see nothing else", got)
	}

	// A step not given the output does not see it.
	if got := run(t, cfg, r, sha, plan.Step{Name: "blind", Inputs: []string{"other.txt"}, Run: "ls dist", Cache: false}); got.Status != "failed" {
		t.Errorf("a step that was not given dist could read it: %+v", got)
	}
}

func TestASkippedStepStillHandsOnItsOutputs(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src.txt": "one\n", "README": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	build := plan.Step{Name: "build", Inputs: []string{"src.txt"}, Outputs: []string{"out.txt"}, Cache: true, Run: "cp src.txt out.txt"}
	use := func(from plan.Result) plan.Step {
		return plan.Step{Name: "use", Inputs: []string{}, Given: given(from), Cache: true, Run: "cat out.txt"}
	}

	first := run(t, cfg, r, sha, build)
	run(t, cfg, r, sha, use(first))

	// A commit that changes nothing build reads: build is skipped, hands on
	// the same output, and so the step given it is skipped too.
	sha = r.commit(map[string]string{"README": "b"})
	again := run(t, cfg, r, sha, build)
	if !again.Cached || again.Outputs["out.txt"] != first.Outputs["out.txt"] {
		t.Fatalf("build on the next commit = %+v, want the earlier pass with the same output", again)
	}
	if got := run(t, cfg, r, sha, use(again)); !got.Cached || got.Output != "one\n" {
		t.Errorf("use on the next commit = %+v, want it skipped", got)
	}

	// A commit that changes what build produces runs the step given it again.
	sha = r.commit(map[string]string{"src.txt": "two\n"})
	changed := run(t, cfg, r, sha, build)
	if changed.Cached || changed.Outputs["out.txt"] == first.Outputs["out.txt"] {
		t.Fatalf("build after its input changed = %+v, want a fresh run with a different output", changed)
	}
	if got := run(t, cfg, r, sha, use(changed)); got.Cached || got.Output != "two\n" {
		t.Errorf("use after the output changed = %+v, want it run again on the new output", got)
	}
}

func TestTheSameFilesAreTheSameOutput(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	// Not cached, and on two machines: the digest has to come out the same
	// from two separate runs, written at two different times.
	build := plan.Step{Name: "build", Outputs: []string{"dist"}, Cache: false, Run: "mkdir dist && echo b > dist/b && echo a > dist/a && ln -s a dist/link"}
	one := run(t, Config{Worker: "one", StateDir: t.TempDir()}, r, sha, build)
	two := run(t, Config{Worker: "two", StateDir: t.TempDir()}, r, sha, build)
	if one.Status != "passed" || one.Outputs["dist"] == "" || one.Outputs["dist"] != two.Outputs["dist"] {
		t.Errorf("two runs that wrote the same files gave %q and %q", one.Outputs["dist"], two.Outputs["dist"])
	}
}

func TestADeclaredOutputMustBeProduced(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	s := plan.Step{Name: "build", Outputs: []string{"dist"}, Cache: true, Run: "echo did nothing"}
	got := run(t, cfg, r, sha, s)
	if got.Status != "failed" || !strings.Contains(got.Detail, "dist") || !strings.Contains(got.Detail, "did not produce") {
		t.Fatalf("a step that left no dist = %+v, want it failed for that reason", got)
	}
	if got.Output != "did nothing\n" {
		t.Errorf("what the step printed was lost: %q", got.Output)
	}
	// And nothing was remembered as a pass.
	if again := run(t, cfg, r, sha, s); again.Cached {
		t.Error("a step that failed to leave its output was reused as a pass")
	}
}

func TestAnOutputMayNotLeadOutOfItself(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	for name, cmd := range map[string]string{
		"an absolute link": "mkdir dist && ln -s /etc/passwd dist/p",
		"a link upwards":   "mkdir dist && ln -s ../a.txt dist/p",
		"a named pipe":     "mkdir dist && mkfifo dist/p",
	} {
		got := run(t, cfg, r, sha, plan.Step{Name: "build", Outputs: []string{"dist"}, Cache: false, Run: cmd})
		if got.Status != "failed" || !strings.Contains(got.Detail, "dist/p") {
			t.Errorf("%s: %+v, want the step failed, naming dist/p", name, got)
		}
	}
}

// An archive in a store is trusted no further than the step that wrote it.
func TestUnpackingRefusesWhatPackingWould(t *testing.T) {
	archive := func(t *testing.T, entries ...tar.Header) (path, digest string) {
		t.Helper()
		path = filepath.Join(t.TempDir(), "a.tar")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.New()
		tw := tar.NewWriter(io.MultiWriter(f, sum))
		for _, h := range entries {
			h := h
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
		}
		tw.Close()
		f.Close()
		return path, hex.EncodeToString(sum.Sum(nil))
	}
	dir := tar.Header{Name: "dist/", Typeflag: tar.TypeDir, Mode: 0o755}
	for name, entries := range map[string][]tar.Header{
		"a file outside the output":  {dir, {Name: "elsewhere", Typeflag: tar.TypeReg, Mode: 0o644}},
		"a file above the workspace": {dir, {Name: "dist/../../up", Typeflag: tar.TypeReg, Mode: 0o644}},
		"a link out of the output":   {dir, {Name: "dist/l", Typeflag: tar.TypeSymlink, Linkname: "../../etc"}},
		"an absolute link":           {dir, {Name: "dist/l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}},
		"a hard link":                {dir, {Name: "dist/l", Typeflag: tar.TypeLink, Linkname: "dist"}},
		"a device":                   {dir, {Name: "dist/d", Typeflag: tar.TypeChar}},
	} {
		t.Run(name, func(t *testing.T) {
			path, digest := archive(t, entries...)
			workspace := t.TempDir()
			if err := unpackOutput(path, digest, workspace, "dist"); err == nil {
				t.Error("it was unpacked")
			}
			if _, err := os.Lstat(filepath.Join(workspace, "elsewhere")); err == nil {
				t.Error("something was written outside the output")
			}
		})
	}

	t.Run("an archive that is not what it was kept as", func(t *testing.T) {
		path, _ := archive(t, dir)
		if err := unpackOutput(path, strings.Repeat("0", 64), t.TempDir(), "dist"); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Errorf("err = %v, want the checksum refused", err)
		}
	})
}

func TestAnOutputReplacesWhatTheRepositoryHasThere(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"dist/stale.txt": "from the repository", "dist/out.txt": "old", "src.txt": "new\n"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	built := run(t, cfg, r, sha, plan.Step{Name: "build", Inputs: []string{"src.txt"}, Outputs: []string{"dist"}, Cache: true, Run: "mkdir dist && cp src.txt dist/out.txt"})
	got := run(t, cfg, r, sha, plan.Step{Name: "use", Given: given(built), Cache: false, Run: "ls dist; cat dist/out.txt"})
	if got.Status != "passed" || got.Output != "out.txt\nnew\n" {
		t.Errorf("use = %+v, want only what build produced at dist", got)
	}
}

func TestOutputsCrossMachinesThroughASharedStore(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src.txt": "one\n"})
	shared := &memStore{}
	one := Config{Worker: "one", StateDir: t.TempDir(), Shared: shared}
	two := Config{Worker: "two", StateDir: t.TempDir(), Shared: shared}
	build := plan.Step{Name: "build", Outputs: []string{"out.txt"}, Cache: true, Run: "cp src.txt out.txt"}
	built := run(t, one, r, sha, build)
	use := plan.Step{Name: "use", Inputs: []string{}, Given: given(built), Cache: false, Run: "cat out.txt"}

	if got := run(t, two, r, sha, use); got.Status != "passed" || got.Output != "one\n" {
		t.Fatalf("on the second machine = %+v, want the first machine's output", got)
	}

	// Without a store in common there is nothing to be given, and the step
	// says so instead of running without it.
	alone := Config{Worker: "three", StateDir: t.TempDir()}
	got := run(t, alone, r, sha, use)
	if got.Status != "failed" || !strings.Contains(got.Detail, "out.txt from build is not on this machine") || !strings.Contains(got.Detail, "STARCI_STORE") {
		t.Errorf("on a machine with no shared store = %+v, want it to fail saying where outputs come from", got)
	}
	if got.Output != "" {
		t.Errorf("the step ran without what it was to be given, and printed %q", got.Output)
	}
}

func TestAPassWhoseOutputsAreGoneIsNotReused(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src.txt": "one\n"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	build := plan.Step{Name: "build", Outputs: []string{"out.txt"}, Cache: true, Run: "cp src.txt out.txt"}
	run(t, cfg, r, sha, build)
	if err := os.RemoveAll(filepath.Join(cfg.StateDir, "results", "outputs")); err != nil {
		t.Fatal(err)
	}
	got := run(t, cfg, r, sha, build)
	if got.Cached || got.Status != "passed" {
		t.Fatalf("build with its output gone = %+v, want it run again", got)
	}
	if !cfg.store().HasBlob(context.Background(), got.Outputs["out.txt"]) {
		t.Error("running it again did not keep the output again")
	}
}

func TestDeclaringAnOutputIsAReasonToRunAgain(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"src.txt": "one\n"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	s := plan.Step{Name: "build", Cache: true, Run: "cp src.txt out.txt"}
	run(t, cfg, r, sha, s)
	s.Outputs = []string{"out.txt"}
	if got := run(t, cfg, r, sha, s); got.Cached || got.Outputs["out.txt"] == "" {
		t.Errorf("after declaring an output = %+v, want a fresh run that kept it", got)
	}
}
