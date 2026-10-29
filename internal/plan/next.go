package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.starlark.net/starlark"
)

// A run is carried out by asking what is next. Next is given the results so
// far and answers with the steps to run now, or that the run is over; it
// keeps nothing between calls. Whoever runs the steps keeps the results, and
// is the record a run resumes from.
//
// Each step a run asks for takes the next number: in a basic plan the order
// the plan lists them, in an advanced script the order it asks, a parallel
// batch taking consecutive numbers. A step is known by its number, and its
// name is for people.

// Asked is a step a run has asked for, and its number in the run.
type Asked struct {
	Number int  `json:"number"`
	Step   Step `json:"step"`
}

// Fingerprint stands for the step as it was asked for. A result keeps it, so
// that a script asking for something else at the same number is caught.
func (a Asked) Fingerprint() string {
	data, _ := json.Marshal(a.Step)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// Answer marks r as the result of a.
func (a Asked) Answer(r Result) Result {
	r.Number, r.Fingerprint = a.Number, a.Fingerprint()
	if r.Name == "" {
		r.Name = a.Step.Name
	}
	return r
}

// Decision is what Next answers.
type Decision struct {
	// Run is every step the run has asked for that has no result yet. Asking
	// again before they finish returns them again; whoever runs them skips
	// the numbers it has already started.
	Run []Asked `json:"run,omitempty"`
	// Done is set once the run will ask for nothing more.
	Done bool `json:"done"`
	OK   bool `json:"ok"`
	// Error is why a run ended early: a failed step an advanced script did
	// not allow for, its own fail(), a mistake in it, or its asking for
	// different steps than it did before.
	Error string `json:"error,omitempty"`
	// Results are the run's results so far, by number, with the steps a
	// basic plan skipped because something they need did not pass.
	Results []Result `json:"results"`
}

// answers are the results a run has been given, looked up by number.
type answers struct {
	byNumber map[int]Result
	taken    map[int]bool
}

func newAnswers(results []Result) (*answers, error) {
	a := &answers{byNumber: map[int]Result{}, taken: map[int]bool{}}
	for _, r := range results {
		if r.Number < 1 {
			return nil, fmt.Errorf("the result for %q has no step number", r.Name)
		}
		if _, dup := a.byNumber[r.Number]; dup {
			return nil, fmt.Errorf("two results for step #%d", r.Number)
		}
		a.byNumber[r.Number] = r
	}
	return a, nil
}

// take returns the result for asked, if there is one, and refuses one that
// answers a different step.
func (a *answers) take(asked Asked) (Result, bool, error) {
	r, ok := a.byNumber[asked.Number]
	if !ok {
		return Result{}, false, nil
	}
	if r.Name != asked.Step.Name || (r.Fingerprint != "" && r.Fingerprint != asked.Fingerprint()) {
		return Result{}, false, fmt.Errorf("step #%d is now %q, where before it was %q: given the same results, a script must ask for the same steps", asked.Number, asked.Step.Name, r.Name)
	}
	a.taken[asked.Number] = true
	return r, true, nil
}

// untaken complains of results for steps the run never asked for.
func (a *answers) untaken() error {
	var extra []string
	for n := range a.byNumber {
		if !a.taken[n] {
			extra = append(extra, fmt.Sprintf("#%d", n))
		}
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Strings(extra)
	return fmt.Errorf("there are results for steps the run did not ask for: %s", strings.Join(extra, ", "))
}

// Next decides what a basic plan runs now. A step is next once everything it
// needs has passed, and skipped once something it needs has not.
func (p *Plan) Next(results []Result) (Decision, error) {
	if err := p.Validate(); err != nil {
		return Decision{}, err
	}
	ans, err := newAnswers(results)
	if err != nil {
		return Decision{}, err
	}
	number := map[string]int{}
	for i, s := range p.Steps {
		number[s.Name] = i + 1
	}
	byName := map[string]Result{}
	var d Decision
	for _, layer := range p.Layers() {
		for _, s := range layer {
			asked := Asked{Number: number[s.Name], Step: s}
			var blocked []string
			waiting := false
			for _, n := range s.Needs {
				need, done := byName[n]
				switch {
				case !done:
					waiting = true
				case !need.ok():
					blocked = append(blocked, n)
				}
			}
			if len(blocked) > 0 {
				// Decided here every time; one handed back is the same.
				_, _, _ = ans.take(asked)
				byName[s.Name] = asked.Answer(Result{Status: "skipped", Detail: "needs " + strings.Join(blocked, ", ")})
				continue
			}
			if waiting {
				// Nothing it needs has finished, so nothing can answer it;
				// a result that does is caught as one never asked for.
				continue
			}
			// Handed what the steps it needs kept, now that they have. The
			// step as resolved is what ran, and what its result answers.
			resolved, err := resolve(s, byName)
			if err != nil {
				byName[s.Name] = asked.Answer(Result{Status: "skipped", Detail: err.Error()})
				continue
			}
			asked.Step = resolved
			r, ok, err := ans.take(asked)
			if err != nil {
				return ended(d, err), nil
			}
			if ok {
				byName[s.Name] = r
				continue
			}
			d.Run = append(d.Run, asked)
		}
	}
	if err := ans.untaken(); err != nil {
		return ended(d, err), nil
	}
	for _, s := range p.Steps {
		if r, ok := byName[s.Name]; ok {
			d.Results = append(d.Results, r)
		}
	}
	sort.Slice(d.Run, func(i, j int) bool { return d.Run[i].Number < d.Run[j].Number })
	if len(d.Results) == len(p.Steps) {
		d.Done, d.OK = true, Passed(d.Results)
	}
	return d, nil
}

// NextInPipeline decides what an advanced script runs now. It evaluates the
// script from the top, answering each step it asks for from results, until
// it asks for one there is no result for.
func NextInPipeline(ctx context.Context, filename string, src []byte, c Context, results []Result) (Decision, error) {
	ans, err := newAnswers(results)
	if err != nil {
		return Decision{}, err
	}
	thread := newThread("pipeline")
	rec := &recorder{answers: ans}
	thread.SetLocal(runnerKey, rec)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("cancelled")
		case <-done:
		}
	}()

	d := Decision{}
	globals, _, err := load(thread, filename, src)
	if err != nil {
		return ended(d, err), nil
	}
	fn, ok := globals["pipeline"].(starlark.Callable)
	if !ok {
		return ended(d, fmt.Errorf("%s: must define a function pipeline(ctx)", filename)), nil
	}
	rec.started = true
	_, err = starlark.Call(thread, fn, starlark.Tuple{contextValue(c)}, nil)
	d.Results = rec.results
	if rec.wanted != nil {
		d.Run = rec.wanted
		return d, nil
	}
	if err == nil {
		err = ans.untaken()
	}
	if err != nil {
		var failed *stepsFailed
		if !errors.As(err, &failed) {
			err = describe(err)
		}
		return ended(d, err), nil
	}
	d.Done, d.OK = true, true
	return d, nil
}

func ended(d Decision, err error) Decision {
	d.Run, d.Done, d.OK, d.Error = nil, true, false, err.Error()
	return d
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
