package step

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// mapSecrets is a source that holds what it is given.
type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", ErrNoSecret
	}
	return v, nil
}

const token = "s3cr3t-value-0123456789"

func TestAStepIsGivenItsSecretsAndTheyAreMasked(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir(), Secrets: mapSecrets{"NPM_TOKEN": token, "OTHER": "not-asked-for"}}
	s := plan.Step{Name: "publish", Image: testImage, Inputs: []string{"."}, Secrets: []string{"NPM_TOKEN"}, Cache: true,
		Run: `test "$NPM_TOKEN" = ` + token + ` && echo right; echo "token is $NPM_TOKEN."; echo "other is [${OTHER:-}]"`}

	var logged []string
	got := Run(context.Background(), cfg, r.dir, sha, s, func(line string) { logged = append(logged, line) })
	if got.Status != "passed" {
		t.Fatalf("%+v", got)
	}
	// The step had the value, and only the secret it asked for.
	if want := "right\ntoken is ***.\nother is []\n"; got.Output != want {
		t.Errorf("output = %q, want %q", got.Output, want)
	}
	if strings.Contains(strings.Join(logged, "\n"), token) {
		t.Error("the secret was logged")
	}

	// Nor is it in anything this machine kept.
	filepath.WalkDir(cfg.StateDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if data, _ := os.ReadFile(path); strings.Contains(string(data), token) {
				t.Errorf("the secret is stored in %s", path)
			}
		}
		return nil
	})

	// A step that fails says what it printed, and that is masked too.
	s.Run, s.Cache = `echo "leaking $NPM_TOKEN"; exit 1`, false
	if got := run(t, cfg, r, sha, s); got.Status != "failed" || strings.Contains(got.Detail+got.Output, token) || !strings.Contains(got.Detail, "leaking ***") {
		t.Errorf("a failed step = %+v, want its output kept and masked", got)
	}
}

func TestASecretOfSeveralLinesIsMaskedLineByLine(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	key := "-----BEGIN KEY-----\nabcdefghijklmnop\nqrstuvwxyz012345\n}\n-----END KEY-----"
	cfg := Config{Worker: "one", StateDir: t.TempDir(), Secrets: mapSecrets{"KEY": key}}
	got := run(t, cfg, r, sha, plan.Step{Name: "sign", Secrets: []string{"KEY"}, Cache: false, Run: `echo "$KEY"; echo "a brace } is not the secret"`})
	if got.Status != "passed" || got.Output != "***\n***\n***\n}\n***\na brace } is not the secret\n" {
		t.Errorf("output = %q", got.Output)
	}
}

func TestSecretsAreInAKeyByNameOnly(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	secrets := mapSecrets{"A": "first-value", "B": "another-value"}
	cfg := Config{Worker: "one", StateDir: t.TempDir(), Secrets: secrets}
	s := plan.Step{Name: "use", Secrets: []string{"A"}, Cache: true, Run: "echo ran"}

	if got := run(t, cfg, r, sha, s); got.Cached || got.Status != "passed" {
		t.Fatalf("first run = %+v", got)
	}
	// A secret that has been rotated is not a reason to run again.
	secrets["A"] = "second-value"
	if got := run(t, cfg, r, sha, s); !got.Cached {
		t.Errorf("after the secret changed = %+v, want the earlier pass", got)
	}
	// Being given a different set of secrets is.
	s.Secrets = []string{"A", "B"}
	if got := run(t, cfg, r, sha, s); got.Cached {
		t.Errorf("with another secret = %+v, want a fresh run", got)
	}
	// And a pass with secrets is not a pass without them.
	s.Secrets = nil
	if got := run(t, cfg, r, sha, s); got.Cached {
		t.Errorf("without secrets = %+v, want a fresh run", got)
	}
}

func TestAStepIsNotRunWithoutASecretItAskedFor(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	s := plan.Step{Name: "publish", Secrets: []string{"HAVE", "MISSING"}, Cache: false, Run: "echo ran anyway"}
	for name, c := range map[string]struct {
		source Secrets
		want   string
	}{
		"a machine with no secrets":       {nil, "this machine has none: STARCI_SECRETS"},
		"a secret the machine lacks":      {mapSecrets{"HAVE": "a-value"}, "the secret MISSING, and this machine has none by that name"},
		"a secret with nothing in it":     {mapSecrets{"HAVE": "a-value", "MISSING": ""}, "the secret MISSING is empty"},
		"a source that cannot be reached": {failing{}, "the secret HAVE could not be fetched: the vault is sealed"},
	} {
		t.Run(name, func(t *testing.T) {
			got := run(t, Config{Worker: "one", StateDir: t.TempDir(), Secrets: c.source}, r, sha, s)
			if got.Status != "failed" || !strings.Contains(got.Detail, c.want) {
				t.Errorf("%+v, want it failed saying %q", got, c.want)
			}
			if got.Output != "" {
				t.Errorf("the step ran, and printed %q", got.Output)
			}
		})
	}
}

type failing struct{}

func (failing) Secret(context.Context, string) (string, error) {
	return "", errors.New("the vault is sealed")
}

func TestSecretsAreWithheldEvenFromAnEarlierPass(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir(), Secrets: mapSecrets{"NPM_TOKEN": token}}
	s := plan.Step{Name: "publish", Secrets: []string{"NPM_TOKEN"}, Cache: true, Run: "echo published"}
	if got := run(t, cfg, r, sha, s); got.Status != "passed" {
		t.Fatalf("%+v", got)
	}
	// The same step, in a run that may not have secrets: neither run, nor
	// answered with what the run that had them printed.
	s.Withheld = true
	got := run(t, cfg, r, sha, s)
	if got.Status != "failed" || got.Cached || got.Output != "" || !strings.Contains(got.Detail, "a pull request from a fork is given none") {
		t.Errorf("%+v, want it failed for being from a fork", got)
	}
}

func TestSecretsFromADirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "NPM_TOKEN"), []byte(token+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "KEY"), []byte("one\ntwo\n"), 0o600)
	source, err := OpenSecrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The line ending a file ends with is not part of the secret.
	if got, err := source.Secret(context.Background(), "NPM_TOKEN"); err != nil || got != token {
		t.Errorf("NPM_TOKEN = %q, %v", got, err)
	}
	if got, _ := source.Secret(context.Background(), "KEY"); got != "one\ntwo" {
		t.Errorf("KEY = %q", got)
	}
	if _, err := source.Secret(context.Background(), "ABSENT"); err != ErrNoSecret {
		t.Errorf("a name with no file: %v, want ErrNoSecret", err)
	}
	if _, err := OpenSecrets(filepath.Join(dir, "nowhere")); err == nil {
		t.Error("a directory that is not there was accepted")
	}
}

func TestSecretsFromAProgram(t *testing.T) {
	program := filepath.Join(t.TempDir(), "secret")
	script := `#!/bin/sh
case "$1" in
  NPM_TOKEN) echo ` + token + ` ;;
  ABSENT) exit 3 ;;
  *) echo half-a-secret; echo "the vault is sealed" >&2; echo "and more" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := OpenSecrets("exec:" + program)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := source.Secret(context.Background(), "NPM_TOKEN"); err != nil || got != token {
		t.Errorf("NPM_TOKEN = %q, %v", got, err)
	}
	if _, err := source.Secret(context.Background(), "ABSENT"); err != ErrNoSecret {
		t.Errorf("exit status 3: %v, want ErrNoSecret", err)
	}
	// What it complained of, in a line, and nothing it printed.
	_, err = source.Secret(context.Background(), "BROKEN")
	if err == nil || err.Error() != "the vault is sealed" {
		t.Errorf("a failing program: %v, want its first line of complaint", err)
	}
	if _, err := OpenSecrets("exec:" + program + "-missing"); err == nil {
		t.Error("a program that is not there was accepted")
	}
}

// The value is handed to the container through the environment of the
// program that starts it, and so is in nobody's list of processes.
func TestASecretIsNotAnArgument(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "one", StateDir: t.TempDir(), Secrets: mapSecrets{"NPM_TOKEN": token}}
	done := make(chan plan.Result, 1)
	go func() {
		done <- run(t, cfg, r, sha, plan.Step{Name: "slow", Secrets: []string{"NPM_TOKEN"}, Cache: false, Run: "echo started; sleep 3"})
	}()
	seen := false
	for running := true; running; {
		select {
		case got := <-done:
			if got.Status != "passed" {
				t.Fatalf("%+v", got)
			}
			running = false
		case <-time.After(200 * time.Millisecond):
			out, _ := exec.Command("ps", "-A", "-o", "args").Output()
			seen = seen || strings.Contains(string(out), "sleep 3")
			if strings.Contains(string(out), token) {
				t.Fatal("the secret is in the arguments of a running process")
			}
		}
	}
	if !seen {
		t.Skip("the step's own process was never seen in the list, so nothing was shown")
	}
}
