// Package ciinfo works out why a run was started from the environment of the
// CI tool starci is running inside, so that a script sees the same ctx there
// as it would if starci had received the event itself.
package ciinfo

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// Detected is what a CI tool's environment says about the run.
type Detected struct {
	// Tool names the CI tool, for telling the person what was understood.
	Tool string
	// Context carries the event and its details. SHA is left for the caller:
	// what is built is the checkout, whatever the tool calls the commit.
	Context plan.Context
	// ChangesKnown is true when Context.ChangedFiles came from the event,
	// as opposed to there being nothing to go on.
	ChangesKnown bool
}

// Detect reads the environment. getenv and readFile stand in for the real
// ones; schedules are those the script declares, which is how a cron
// expression that fired is given back its name. It reports false when no CI
// tool it knows is running.
func Detect(getenv func(string) string, readFile func(string) ([]byte, error), schedules []plan.Schedule) (Detected, bool) {
	switch {
	case getenv("GITHUB_ACTIONS") == "true":
		tool := "GitHub Actions"
		// Forgejo and Gitea set GitHub's variables so that workflows written
		// for it run unchanged, and one of their own to say who they are.
		if getenv("FORGEJO_ACTIONS") == "true" {
			tool = "Forgejo Actions"
		} else if getenv("GITEA_ACTIONS") == "true" {
			tool = "Gitea Actions"
		}
		return githubActions(tool, getenv, readFile, schedules), true
	case getenv("CIRCLECI") == "true":
		return circleCI(getenv), true
	}
	return Detected{}, false
}

// githubEvent is the part of the event payload that the environment
// variables do not carry.
type githubEvent struct {
	Number      int    `json:"number"`
	Schedule    string `json:"schedule"`
	PullRequest struct {
		Head struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
}

func githubActions(tool string, getenv func(string) string, readFile func(string) ([]byte, error), schedules []plan.Schedule) Detected {
	d := Detected{Tool: tool, Context: plan.Context{Repo: getenv("GITHUB_REPOSITORY")}}
	c := &d.Context

	var event githubEvent
	havePayload := false
	if path := getenv("GITHUB_EVENT_PATH"); path != "" {
		if data, err := readFile(path); err == nil {
			havePayload = json.Unmarshal(data, &event) == nil
		}
	}

	switch getenv("GITHUB_EVENT_NAME") {
	case "push":
		if getenv("GITHUB_REF_TYPE") == "tag" {
			c.Event, c.Tag = plan.EventTag, getenv("GITHUB_REF_NAME")
		} else {
			c.Event, c.Ref = plan.EventPush, getenv("GITHUB_REF_NAME")
		}
		if havePayload {
			seen := map[string]bool{}
			for _, commit := range event.Commits {
				for _, list := range [][]string{commit.Added, commit.Modified, commit.Removed} {
					for _, f := range list {
						if !seen[f] {
							seen[f] = true
							c.ChangedFiles = append(c.ChangedFiles, f)
						}
					}
				}
			}
			sort.Strings(c.ChangedFiles)
			// A push with no commits listed (a new branch at an old commit)
			// says nothing about what changed.
			d.ChangesKnown = len(event.Commits) > 0
		}
	case "pull_request", "pull_request_target":
		c.Event, c.Ref = plan.EventPullRequest, getenv("GITHUB_HEAD_REF")
		pr := &plan.PullRequest{Number: event.Number, Base: getenv("GITHUB_BASE_REF"), Head: getenv("GITHUB_HEAD_REF")}
		if pr.Number == 0 {
			// refs/pull/<number>/merge
			if parts := strings.Split(getenv("GITHUB_REF"), "/"); len(parts) >= 3 && parts[1] == "pull" {
				pr.Number, _ = strconv.Atoi(parts[2])
			}
		}
		// Unless the payload says the head is this repository, it is taken
		// to be someone else's. Guessing the other way would be the unsafe one.
		head := event.PullRequest.Head.Repo.FullName
		pr.Fork = !havePayload || head == "" || head != c.Repo
		c.PR = pr
	case "schedule":
		c.Event, c.Ref = plan.EventSchedule, getenv("GITHUB_REF_NAME")
		for _, s := range schedules {
			if s.Cron == strings.Join(strings.Fields(event.Schedule), " ") {
				c.Schedule = s.Name
			}
		}
	default:
		// workflow_dispatch, and anything else a person or another system
		// set off deliberately.
		c.Event, c.Ref = plan.EventManual, getenv("GITHUB_REF_NAME")
	}
	return d
}

func circleCI(getenv func(string) string) Detected {
	d := Detected{Tool: "CircleCI"}
	c := &d.Context
	if user, repo := getenv("CIRCLE_PROJECT_USERNAME"), getenv("CIRCLE_PROJECT_REPONAME"); user != "" && repo != "" {
		c.Repo = user + "/" + repo
	}
	switch {
	case getenv("CIRCLE_TAG") != "":
		c.Event, c.Tag = plan.EventTag, getenv("CIRCLE_TAG")
	case getenv("CIRCLE_PULL_REQUEST") != "":
		// CircleCI builds the branch a pull request comes from; it names the
		// pull request but not the branch it would merge into.
		c.Event, c.Ref = plan.EventPullRequest, getenv("CIRCLE_BRANCH")
		pr := &plan.PullRequest{Head: getenv("CIRCLE_BRANCH")}
		url := getenv("CIRCLE_PULL_REQUEST")
		pr.Number, _ = strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
		// Set only for a build of a pull request from a fork.
		pr.Fork = getenv("CIRCLE_PR_NUMBER") != ""
		c.PR = pr
	default:
		c.Event, c.Ref = plan.EventPush, getenv("CIRCLE_BRANCH")
	}
	return d
}
