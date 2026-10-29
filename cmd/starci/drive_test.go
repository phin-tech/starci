package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

// Something other than starci can carry a run out with next and step, keep
// the results, and resume from them: only what has no result is run again.
func TestNextAndStepCarryARunOutAndResumeIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds starci and runs it; skipped with -short")
	}
	dir := t.TempDir()
	starci := filepath.Join(dir, "starci")
	if out, err := exec.Command("go", "build", "-o", starci, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	repo := filepath.Join(dir, "repo")
	ran := filepath.Join(dir, "ran")
	script := `def pipeline(ctx):
    files = run(step("list", run = "printf 'a\\nb\\n'")).output.splitlines()
    parallel([step("test", run = "echo " + f + " >> ` + ran + `") for f in files])
`
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".ci.star"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	call := func(in any, args ...string) []byte {
		t.Helper()
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(starci, args...)
		cmd.Env = append(os.Environ(), "STARCI_STATE="+filepath.Join(dir, "state"))
		cmd.Stdin = bytes.NewReader(data)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("starci %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
		}
		return out
	}
	// drive carries the run out from state, stopping after stopAfter steps
	// if that is not zero, and returns the state and the last answer.
	drive := func(state nextState, stopAfter int) (nextState, nextAnswer) {
		t.Helper()
		steps := 0
		for {
			var answer nextAnswer
			if err := json.Unmarshal(call(state, "next", repo), &answer); err != nil {
				t.Fatal(err)
			}
			state.Context, state.CloneURL = &answer.Context, answer.CloneURL
			if answer.Done {
				return state, answer
			}
			for _, asked := range answer.Run {
				if stopAfter > 0 && steps == stopAfter {
					return state, answer
				}
				var r plan.Result
				job := stepJob{Step: asked, Context: answer.Context, CloneURL: answer.CloneURL}
				if err := json.Unmarshal(call(job, "step", "--bare"), &r); err != nil {
					t.Fatal(err)
				}
				state.Results = append(state.Results, r)
				steps++
			}
		}
	}

	state, _ := drive(nextState{}, 2)
	if got := len(state.Results); got != 2 {
		t.Fatalf("stopped with %d results, want 2", got)
	}
	_, answer := drive(state, 0)
	if !answer.OK || len(answer.Results) != 3 {
		t.Fatalf("answer = %+v, want a passed run of three steps", answer)
	}
	for i, r := range answer.Results {
		if r.Number != i+1 {
			t.Errorf("result %d (%s) is #%d", i, r.Name, r.Number)
		}
	}
	data, err := os.ReadFile(ran)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); strings.Join(got, " ") != "a b" {
		t.Errorf("the tests ran as %q, want each once: a, then b on resuming", got)
	}
}
