package plan

import "strings"

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
