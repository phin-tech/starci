package compile

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"go/format"
	"strconv"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

//go:embed temporal/*.go.txt
var temporalSource embed.FS

// temporalSDK is the Temporal Go SDK the files are written against.
const (
	temporalSDK       = "v1.50.0"
	temporalEnvconfig = "v1.0.2" // its own module, with its own versions
)

// temporalQueue is where a step's activity goes: the task queue named by the
// machines it asks for, so that a worker offering exactly those takes it. A
// step that asks for nothing goes to "starci".
func temporalQueue(on map[string]string) string {
	if len(on) == 0 {
		return "starci"
	}
	var parts []string
	for _, k := range sortedKeys(on) {
		parts = append(parts, k+"="+on[k])
	}
	return "starci/" + strings.Join(parts, ",")
}

// Temporal compiles a basic script to a Go module for Temporal: a workflow
// that carries out the plan, an activity that runs each step with
// `starci step`, and a worker. The files are keyed by name.
//
// The plan is frozen as Workflow and Activity code the way the workflow
// targets freeze it, asked once for each kind of event. The steps themselves
// are not compiled to anything: they travel as the JSON `starci step` takes,
// so a machine that runs them is the same machine that runs them everywhere.
func Temporal(filename string, src []byte, o Options) (map[string]string, error) {
	if o.DefaultBranch == "" {
		o.DefaultBranch = "main"
	}
	u, err := eventUnion(filename, src, o)
	if err != nil {
		return nil, err
	}
	merged := &plan.Plan{Steps: u.steps}
	if err := merged.Validate(); err != nil {
		return nil, err
	}
	for _, s := range u.steps {
		if err := temporalRefusal(s); err != nil {
			return nil, err
		}
	}

	notes := u.notes(o.DefaultBranch)
	var placed, keeps []string
	for _, s := range u.steps {
		if len(s.Prefer) > 0 || len(s.Avoid) > 0 {
			placed = append(placed, s.Name)
		}
		if len(s.Keep) > 0 {
			keeps = append(keeps, s.Name)
		}
	}
	if len(placed) > 0 {
		notes = append(notes, fmt.Sprintf("`prefer` and `avoid` are dropped (%s): a task queue is offered by a worker or it is not, and cannot be preferred.", strings.Join(placed, ", ")))
	}
	if len(keeps) > 0 {
		notes = append(notes, fmt.Sprintf("Steps that keep files (%s) hand them on only through a store the workers share: set STARCI_STORE on every worker.", strings.Join(keeps, ", ")))
	}
	if len(u.schedules) > 0 {
		notes = append(notes, "A schedule is a Temporal Schedule you create: start the workflow with the context's `event` set to schedule and `schedule` to its name. Overlap and catch-up are Temporal's to decide.")
	}
	if quiet := u.quiet(); len(quiet) > 0 {
		notes = append(notes, "The script plans nothing for "+join(quiet)+", so a run for "+pronoun(len(quiet))+" does nothing and passes.")
	}
	notes = append(notes, "Steps are numbered by their place in this plan, not in the plan of the event that started the run, so a run's numbers have gaps.")

	byName := map[string]int{}
	for i, s := range u.steps {
		byName[s.Name] = i
	}

	var w strings.Builder
	w.WriteString(temporalHeader(o, "plan.go", notes))
	w.WriteString("package main\n\n")
	w.WriteString("// steps is the plan, merged over every kind of event. Each is run as an\n// activity by StarciRun once the steps it needs have passed. asked is the step\n// as `starci next` would hand it out, exactly what `starci step` reads.\n")
	w.WriteString("var steps = []struct {\n\tNumber int\n\tName   string\n\tQueue  string\n\tNeeds  []int\n\twhen   func(c Context) bool\n\tasked  string\n}{\n")
	for i, s := range u.steps {
		asked, err := askedJSON(plan.Asked{Number: i + 1, Step: s})
		if err != nil {
			return nil, err
		}
		var needs []string
		for _, n := range s.Needs {
			needs = append(needs, strconv.Itoa(byName[n]))
		}
		fmt.Fprintf(&w, "\t{\n\t\tNumber: %d,\n\t\tName:   %s,\n\t\tQueue:  %s,\n", i+1, q(s.Name), q(temporalQueue(s.On)))
		if len(needs) > 0 {
			fmt.Fprintf(&w, "\t\tNeeds:  []int{%s},\n", strings.Join(needs, ", "))
		}
		if cond := u.condition(s.Name, `(c.Event == "" || c.Event == "push")`, `c.Event == "pull_request"`, func(p probe) string { return p.goWhen }); cond != "" {
			fmt.Fprintf(&w, "\t\twhen:   func(c Context) bool { return %s },\n", cond)
		}
		fmt.Fprintf(&w, "\t\tasked:  %s,\n\t},\n", goString(asked))
	}
	w.WriteString("}\n")

	files := map[string]string{"plan.go": w.String()}
	for _, name := range []string{"workflow.go", "activity.go", "main.go"} {
		body, err := temporalSource.ReadFile("temporal/" + name + ".txt")
		if err != nil {
			return nil, err
		}
		files[name] = temporalHeader(o, name, nil) + string(body)
	}
	// What is written is gofmt'd, and so known to parse.
	for name, text := range files {
		formatted, err := format.Source([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("the %s written for Temporal does not parse (a bug in starci): %w", name, err)
		}
		files[name] = string(formatted)
	}
	files["go.mod"] = fmt.Sprintf("module starci-ci\n\ngo 1.26\n\nrequire (\n\tgo.temporal.io/sdk %s\n\tgo.temporal.io/sdk/contrib/envconfig %s\n)\n", temporalSDK, temporalEnvconfig)
	return files, nil
}

// temporalRefusal says why a step cannot be a Temporal activity, if it cannot.
func temporalRefusal(s plan.Step) error {
	if len(s.Artifacts) > 0 {
		return fmt.Errorf("step %q is handed artifacts, which the Temporal target cannot yet resolve: `starci step` is given steps as they are, and only `starci next` puts real pointers in place of artifact() placeholders", s.Name)
	}
	return bareRefusal(s)
}

func temporalHeader(o Options, file string, notes []string) string {
	source := o.Source
	if source == "" {
		source = "a starci script"
	}
	var w strings.Builder
	fmt.Fprintf(&w, "// Code generated by `starci compile --target temporal` from %s. DO NOT EDIT.\n", source)
	if file == "plan.go" {
		w.WriteString("//\n// A Temporal workflow that carries out the plan, and a worker that runs each\n// step with `starci step`. Workers need starci (STARCI names it, else PATH)\n// and Docker, and are started with:\n//\n//	STARCI_QUEUES=starci go run .\n//\n// A run is started with the context and clone URL `starci next CHECKOUT`\n// prints the first time:\n//\n//	temporal workflow start --task-queue starci --type StarciRun --workflow-id RUN \\\n//	  --input \"$(starci next . </dev/null | jq -c '{context, clone_url}')\"\n//\n// What does not carry over:\n")
		for _, n := range notes {
			for i, line := range wrap(n, 74) {
				if i == 0 {
					w.WriteString("//  - " + line + "\n")
				} else {
					w.WriteString("//    " + line + "\n")
				}
			}
		}
	}
	w.WriteString("\n")
	return w.String()
}

// askedJSON is a step as `starci next` and `starci step` write it.
func askedJSON(a plan.Asked) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// goString writes s as a Go string literal, raw when it can be.
func goString(s string) string {
	if !strings.ContainsAny(s, "`\r") {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}
