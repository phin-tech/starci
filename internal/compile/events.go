package compile

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// A workflow file has to say in advance what happens for every event, with no
// script to ask when one arrives. So the script is asked now, once for each
// kind of event, with stand-in details; a step that turns up for some kinds
// and not others becomes a job with a condition.
//
// This is faithful for anything a script decides by the kind of event. It is
// not for anything decided by a particular value: a rule about changed files,
// about a branch other than "is it the default one", about what a tag is
// called. Those are frozen at what the stand-in gave.

// probe is one kind of event, as the script is told about it and as GitHub
// describes it.
type probe struct {
	name string
	ctx  plan.Context
	// when is the GitHub expression that is true for this kind of event.
	when string
}

const (
	standInBranch = "a-branch"
	standInTag    = "v0.0.0"
)

// Where each fixed kind of event sits in the list probes returns. Schedules
// follow, then a manual run last.
const (
	pushDefault = iota
	pushOther
	tagPush
	pullRequest
	forkPullRequest
	firstSchedule
)

func probes(def string, schedules []plan.Schedule) []probe {
	ref := "refs/heads/" + def
	ps := []probe{
		{"a push to " + def, plan.Context{Event: plan.EventPush, Ref: def},
			fmt.Sprintf("(github.event_name == 'push' && github.ref == '%s')", ref)},
		{"a push to another branch", plan.Context{Event: plan.EventPush, Ref: standInBranch},
			fmt.Sprintf("(github.event_name == 'push' && github.ref_type == 'branch' && github.ref != '%s')", ref)},
		{"a tag", plan.Context{Event: plan.EventTag, Tag: standInTag},
			"(github.event_name == 'push' && github.ref_type == 'tag')"},
		// From this repository and from a fork are asked about separately:
		// the difference is who wrote the code, and a script that refuses to
		// do something for a stranger must still refuse once compiled.
		{"a pull request", plan.Context{Event: plan.EventPullRequest, Ref: standInBranch, PR: &plan.PullRequest{Number: 1, Base: def, Head: standInBranch}},
			"(github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)"},
		{"a pull request from a fork", plan.Context{Event: plan.EventPullRequest, Ref: standInBranch, PR: &plan.PullRequest{Number: 1, Base: def, Head: standInBranch, Fork: true}},
			"(github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name != github.repository)"},
	}
	for _, s := range schedules {
		ps = append(ps, probe{"the " + s.Name + " schedule", plan.Context{Event: plan.EventSchedule, Ref: def, Schedule: s.Name},
			fmt.Sprintf("(github.event_name == 'schedule' && github.event.schedule == '%s')", s.Cron)})
	}
	return append(ps, probe{"a manual run", plan.Context{Event: plan.EventManual, Ref: def}, "github.event_name == 'workflow_dispatch'"})
}

// GitHubEvents compiles a basic script to a GitHub Actions workflow that
// triggers on, and does the right thing for, each kind of event the script
// plans steps for.
func GitHubEvents(filename string, src []byte, o Options) (string, error) {
	if o.DefaultBranch == "" {
		o.DefaultBranch = "main"
	}
	schedules, err := plan.Schedules(filename, src)
	if err != nil {
		return "", err
	}
	all := probes(o.DefaultBranch, schedules)

	steps := map[string]plan.Step{}
	seenIn := map[string][]int{} // step name -> probes it appears in
	var order []string
	read := map[string]bool{}
	active := make([]bool, len(all))

	for i, pr := range all {
		p, fields, err := plan.EvalTracked(filename, src, pr.ctx)
		if err != nil {
			return "", fmt.Errorf("for %s: %w", pr.name, err)
		}
		for _, f := range fields {
			read[f] = true
		}
		active[i] = len(p.Steps) > 0
		for _, s := range p.Steps {
			if earlier, ok := steps[s.Name]; ok {
				// One job cannot be two things. What a step needs may differ
				// from event to event; nothing else about it may.
				a, b := earlier, s
				a.Needs, b.Needs = nil, nil
				if !reflect.DeepEqual(a, b) {
					return "", fmt.Errorf("step %q is not the same for %s as for %s; give the two different names", s.Name, pr.name, all[seenIn[s.Name][0]].name)
				}
				merged := earlier
				for _, n := range s.Needs {
					if !contains(merged.Needs, n) {
						merged.Needs = append(merged.Needs, n)
					}
				}
				steps[s.Name] = merged
			} else {
				steps[s.Name] = s
				order = append(order, s.Name)
			}
			seenIn[s.Name] = append(seenIn[s.Name], i)
		}
	}
	if len(order) == 0 {
		return "", fmt.Errorf("%s plans no steps for any kind of event; there is nothing to compile", filename)
	}

	// The events that lead to anything at all are the workflow's triggers.
	nActive := 0
	for _, a := range active {
		if a {
			nActive++
		}
	}
	o.conditions = map[string]string{}
	merged := &plan.Plan{}
	for _, name := range order {
		merged.Steps = append(merged.Steps, steps[name])
		if len(seenIn[name]) == nActive {
			continue // runs whenever the workflow does
		}
		var parts []string
		in := seenIn[name]
		// Both kinds of branch push together are simply "a push to a branch",
		// and both kinds of pull request simply "a pull request".
		if contains2(in, pushDefault) && contains2(in, pushOther) {
			parts = append(parts, "(github.event_name == 'push' && github.ref_type == 'branch')")
			in = without(in, pushDefault, pushOther)
		}
		if contains2(in, pullRequest) && contains2(in, forkPullRequest) {
			parts = append(parts, "github.event_name == 'pull_request'")
			in = without(in, pullRequest, forkPullRequest)
		}
		for _, i := range in {
			parts = append(parts, all[i].when)
		}
		o.conditions[name] = strings.Join(parts, " || ")
	}

	var on strings.Builder
	on.WriteString("on:\n")
	if active[pushDefault] || active[pushOther] || active[tagPush] {
		on.WriteString("  push:\n")
		switch {
		case active[pushOther]:
			on.WriteString("    branches: ['**']\n")
		case active[pushDefault]:
			fmt.Fprintf(&on, "    branches: [%s]\n", q(o.DefaultBranch))
		}
		if active[tagPush] {
			on.WriteString("    tags: ['**']\n")
		}
	}
	if active[pullRequest] || active[forkPullRequest] {
		on.WriteString("  pull_request:\n")
	}
	var crons []string
	for i, s := range schedules {
		if active[firstSchedule+i] {
			crons = append(crons, s.Cron)
		}
	}
	if len(crons) > 0 {
		on.WriteString("  schedule:\n")
		for _, c := range crons {
			fmt.Fprintf(&on, "    - cron: %s\n", q(c))
		}
	}
	if active[len(all)-1] {
		on.WriteString("  workflow_dispatch:\n")
	}
	o.on = on.String()

	// Say what was and was not possible to carry over.
	var fields []string
	for f := range read {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	o.Read = nil
	for _, f := range fields {
		switch f {
		case "changed_files", "changed_dirs":
			o.notes = appendOnce(o.notes, "The script decides by which files a commit changed. A workflow file cannot, so it does what the script does when nothing is known about the changes, on every run.")
		case "ref":
			o.notes = appendOnce(o.notes, fmt.Sprintf("The script looks at the branch. Whether it is %s carries over; any other rule about branch names does not, and every other branch is treated alike.", o.DefaultBranch))
		case "tag":
			o.notes = appendOnce(o.notes, "The script looks at the tag's name. That does not carry over: every tag is treated alike.")
		case "pr":
			o.notes = appendOnce(o.notes, fmt.Sprintf("The script looks at the pull request. Whether it comes from a fork carries over; any other rule about it does not, and every pull request is treated as one into %s.", o.DefaultBranch))
		}
	}
	for _, sch := range schedules {
		if sch.Overlap != plan.OverlapSkip || sch.CatchUp {
			o.notes = appendOnce(o.notes, "The script sets overlap or catch_up on a schedule. Neither carries over.")
		}
	}
	if len(schedules) > 0 {
		o.notes = appendOnce(o.notes, "A schedule's runs may overlap here, where starci would skip a firing while the last run is still going; and a missed firing is never made up.")
	}
	var quiet []string
	for i, a := range active {
		if !a {
			quiet = append(quiet, all[i].name)
		}
	}
	if len(quiet) > 0 {
		o.notes = append(o.notes, "The script plans nothing for "+join(quiet)+", so the workflow does not run for "+pronoun(len(quiet))+".")
	}

	if err := merged.Validate(); err != nil {
		return "", err
	}
	return GitHub(merged, o)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func contains2(is []int, i int) bool {
	for _, x := range is {
		if x == i {
			return true
		}
	}
	return false
}

func without(is []int, drop ...int) []int {
	var out []int
	for _, x := range is {
		if !contains2(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

func appendOnce(ss []string, s string) []string {
	if contains(ss, s) {
		return ss
	}
	return append(ss, s)
}

func join(ss []string) string {
	if len(ss) < 2 {
		return strings.Join(ss, "")
	}
	return strings.Join(ss[:len(ss)-1], ", ") + " or " + ss[len(ss)-1]
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
