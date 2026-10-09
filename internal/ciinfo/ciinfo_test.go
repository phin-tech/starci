package ciinfo

import (
	"errors"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

func detect(t *testing.T, env map[string]string, payload string) (Detected, bool) {
	t.Helper()
	if payload != "" {
		env["GITHUB_EVENT_PATH"] = "/event.json"
	}
	return Detect(
		func(k string) string { return env[k] },
		func(path string) ([]byte, error) {
			if path == "/event.json" && payload != "" {
				return []byte(payload), nil
			}
			return nil, errors.New("no such file")
		},
		[]plan.Schedule{{Name: "nightly", Cron: "0 3 * * *"}, {Name: "weekly", Cron: "0 4 * * 1"}},
	)
}

func github(extra map[string]string) map[string]string {
	env := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "sam/app"}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func TestNotInACITool(t *testing.T) {
	if _, ok := detect(t, map[string]string{"HOME": "/home/sam"}, ""); ok {
		t.Error("a CI tool was detected in an ordinary environment")
	}
}

func TestGitHubPush(t *testing.T) {
	d, ok := detect(t, github(map[string]string{"GITHUB_EVENT_NAME": "push", "GITHUB_REF_TYPE": "branch", "GITHUB_REF_NAME": "main"}),
		`{"commits":[{"added":["src/new.py"],"modified":["README.md"]},{"modified":["src/new.py"],"removed":["old.txt"]}]}`)
	c := d.Context
	if !ok || d.Tool != "GitHub Actions" || c.Event != plan.EventPush || c.Ref != "main" || c.Repo != "sam/app" {
		t.Fatalf("detected = %+v", d)
	}
	if got := strings.Join(c.ChangedFiles, " "); got != "README.md old.txt src/new.py" || !d.ChangesKnown {
		t.Errorf("changed = %q (known: %v)", got, d.ChangesKnown)
	}

	// A branch pushed at an existing commit lists no commits: nothing is
	// known about what changed, which is not the same as nothing changing.
	d, _ = detect(t, github(map[string]string{"GITHUB_EVENT_NAME": "push", "GITHUB_REF_NAME": "new-branch"}), `{"commits":[]}`)
	if d.ChangesKnown {
		t.Error("a push listing no commits was taken to say what changed")
	}
}

func TestGitHubTag(t *testing.T) {
	d, _ := detect(t, github(map[string]string{"GITHUB_EVENT_NAME": "push", "GITHUB_REF_TYPE": "tag", "GITHUB_REF_NAME": "v1.2.0"}), `{}`)
	if c := d.Context; c.Event != plan.EventTag || c.Tag != "v1.2.0" || c.Ref != "" {
		t.Errorf("context = %+v", c)
	}
}

func TestGitHubPullRequest(t *testing.T) {
	env := map[string]string{"GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/pull/42/merge", "GITHUB_BASE_REF": "main", "GITHUB_HEAD_REF": "fix-login"}

	d, _ := detect(t, github(env), `{"number":42,"pull_request":{"head":{"repo":{"full_name":"sam/app"}}}}`)
	c := d.Context
	if c.Event != plan.EventPullRequest || c.Ref != "fix-login" || c.PR == nil {
		t.Fatalf("context = %+v", c)
	}
	if *c.PR != (plan.PullRequest{Number: 42, Base: "main", Head: "fix-login", Fork: false}) {
		t.Errorf("pr = %+v", *c.PR)
	}

	d, _ = detect(t, github(env), `{"number":42,"pull_request":{"head":{"repo":{"full_name":"stranger/app"}}}}`)
	if !d.Context.PR.Fork {
		t.Error("a pull request from another repository was not marked as a fork")
	}

	// With nothing to say whose code it is, it is treated as a stranger's.
	d, _ = detect(t, github(env), "")
	if pr := d.Context.PR; !pr.Fork || pr.Number != 42 {
		t.Errorf("without a payload, pr = %+v; want a fork, numbered from the ref", *pr)
	}
}

func TestGitHubScheduleIsGivenItsName(t *testing.T) {
	d, _ := detect(t, github(map[string]string{"GITHUB_EVENT_NAME": "schedule", "GITHUB_REF_NAME": "main"}), `{"schedule":"0 4 * * 1"}`)
	if c := d.Context; c.Event != plan.EventSchedule || c.Schedule != "weekly" {
		t.Errorf("context = %+v", c)
	}
	// A cron the script does not declare still counts as a schedule.
	d, _ = detect(t, github(map[string]string{"GITHUB_EVENT_NAME": "schedule"}), `{"schedule":"5 5 * * *"}`)
	if c := d.Context; c.Event != plan.EventSchedule || c.Schedule != "" {
		t.Errorf("context = %+v", c)
	}
}

func TestGitHubManualAndForgejo(t *testing.T) {
	d, _ := detect(t, github(map[string]string{"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF_NAME": "main", "FORGEJO_ACTIONS": "true"}), `{}`)
	if d.Tool != "Forgejo Actions" || d.Context.Event != plan.EventManual || d.Context.Ref != "main" {
		t.Errorf("detected = %+v", d)
	}
}

func TestCircleCI(t *testing.T) {
	base := map[string]string{"CIRCLECI": "true", "CIRCLE_PROJECT_USERNAME": "sam", "CIRCLE_PROJECT_REPONAME": "app", "CIRCLE_BRANCH": "fix-login"}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}

	d, ok := detect(t, with(nil), "")
	if c := d.Context; !ok || d.Tool != "CircleCI" || c.Event != plan.EventPush || c.Ref != "fix-login" || c.Repo != "sam/app" {
		t.Errorf("push: %+v", d)
	}
	d, _ = detect(t, with(map[string]string{"CIRCLE_TAG": "v2.0.0", "CIRCLE_BRANCH": ""}), "")
	if c := d.Context; c.Event != plan.EventTag || c.Tag != "v2.0.0" {
		t.Errorf("tag: %+v", c)
	}
	d, _ = detect(t, with(map[string]string{"CIRCLE_PULL_REQUEST": "https://github.com/sam/app/pull/17"}), "")
	if c := d.Context; c.Event != plan.EventPullRequest || c.PR.Number != 17 || c.PR.Head != "fix-login" || c.PR.Fork {
		t.Errorf("pull request: %+v, pr %+v", c, c.PR)
	}
	d, _ = detect(t, with(map[string]string{"CIRCLE_PULL_REQUEST": "https://github.com/sam/app/pull/18", "CIRCLE_PR_NUMBER": "18"}), "")
	if !d.Context.PR.Fork {
		t.Error("a CircleCI build of a forked pull request was not marked as a fork")
	}
}
