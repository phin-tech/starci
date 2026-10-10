package plan

import (
	"fmt"
	"sort"
	"strings"
)

// RunPlan executes a basic plan through r: one layer of steps at a time, each
// layer started together, a step skipped when something it needs did not
// pass. It returns a result for every step, in the plan's order.
//
// A step could start the moment its own dependencies finish rather than when
// its whole layer is ready. Going layer by layer trades that for a start
// order that never varies, which an engine that replays a run depends on.
func RunPlan(p *Plan, r Runner) ([]Result, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	results := map[string]Result{}
	for _, layer := range p.Layers() {
		var runnable []Step
		for _, s := range layer {
			var blocked []string
			for _, n := range s.Needs {
				if !results[n].ok() {
					blocked = append(blocked, n)
				}
			}
			if len(blocked) > 0 {
				results[s.Name] = Result{Name: s.Name, Status: "skipped", Detail: "needs " + strings.Join(blocked, ", ")}
				continue
			}
			if missing := give(&s, results); missing != "" {
				results[s.Name] = Result{Name: s.Name, Status: "failed", Detail: missing}
				continue
			}
			runnable = append(runnable, s)
		}
		if len(runnable) == 0 {
			continue
		}
		ran, err := r.Run(runnable)
		if err != nil {
			return nil, err
		}
		for i, s := range runnable {
			results[s.Name] = ran[i]
		}
	}
	out := make([]Result, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = results[s.Name]
	}
	return out, nil
}

// Passed reports whether every result passed.
func Passed(results []Result) bool {
	for _, r := range results {
		if !r.ok() {
			return false
		}
	}
	return true
}

// give fills in what a step is handed from the steps it names in From, now
// that they have run. It is done here, above any engine, so that every
// engine carries a step that already says exactly what it is to be given.
//
// It returns a sentence when a step named in From did not leave what it
// declared, which is a reason not to run this one.
func give(s *Step, results map[string]Result) (missing string) {
	s.Given = nil
	for _, producer := range s.From {
		r, ok := results[producer]
		if !ok || !r.ok() {
			return fmt.Sprintf("%s has not passed in this run, so there are no outputs of it to give", producer)
		}
		if len(r.Outputs) == 0 {
			return fmt.Sprintf("%s passed but its outputs were not kept", producer)
		}
		paths := make([]string, 0, len(r.Outputs))
		for p := range r.Outputs {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			s.Given = append(s.Given, Given{Step: producer, Path: p, Digest: r.Outputs[p]})
		}
	}
	return ""
}
