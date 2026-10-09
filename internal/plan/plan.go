// Package plan turns a Starlark CI script into a fixed list of steps.
//
// A script defines ci(ctx) and returns the steps to run:
//
//	def ci(ctx):
//	    build = step("build", image = "node:22", run = "make build")
//	    return [build, step("test", image = "node:22", run = "make test", needs = [build])]
//
// Evaluation is pure. The script can read ctx and call step(), and nothing
// else: Starlark has no file, network or clock access, and load() is refused.
// The same script and Context therefore always produce the same Plan, which is
// what lets a plan be computed once and executed elsewhere.
package plan

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// maxSteps bounds how long a script may run, so `while`-style mistakes (or a
// hostile script) end in an error rather than a hung planner.
const maxSteps = 10_000_000

// Context is everything a script is told about the run.
type Context struct {
	Repo         string   `json:"repo"`
	SHA          string   `json:"sha"`
	Ref          string   `json:"ref"`
	ChangedFiles []string `json:"changed_files"`
}

// Step is one container to run.
type Step struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Run   string            `json:"run"`
	Needs []string          `json:"needs"`
	Env   map[string]string `json:"env"`
}

// Plan is the evaluated script: steps in the order the script returned them.
type Plan struct {
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	Ref   string `json:"ref"`
	Steps []Step `json:"steps"`
}

// Eval runs the script in src and returns its plan. filename is used only in
// error messages.
func Eval(filename string, src []byte, c Context) (*Plan, error) {
	thread := &starlark.Thread{
		Name:  "plan",
		Print: func(*starlark.Thread, string) {}, // output would be the one impure thing a script can do
		Load: func(_ *starlark.Thread, module string) (starlark.StringDict, error) {
			return nil, fmt.Errorf("load(%q): loading other files is not supported", module)
		},
	}
	thread.SetMaxExecutionSteps(maxSteps)

	predeclared := starlark.StringDict{
		"step": starlark.NewBuiltin("step", stepBuiltin),
	}
	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, filename, src, predeclared)
	if err != nil {
		return nil, describe(err)
	}

	ci, ok := globals["ci"].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("%s: must define a function ci(ctx)", filename)
	}
	result, err := starlark.Call(thread, ci, starlark.Tuple{contextValue(c)}, nil)
	if err != nil {
		return nil, describe(err)
	}

	steps, err := collect(result)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	if err := validate(steps); err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return &Plan{Repo: c.Repo, SHA: c.SHA, Ref: c.Ref, Steps: steps}, nil
}

// describe keeps the Starlark backtrace, which names the line in the script.
func describe(err error) error {
	var evalErr *starlark.EvalError
	if errors.As(err, &evalErr) {
		return errors.New(evalErr.Backtrace())
	}
	return err
}

func contextValue(c Context) starlark.Value {
	files := append([]string(nil), c.ChangedFiles...)
	sort.Strings(files)

	// Top-level directory of each changed file ("." for files at the root),
	// the granularity most "only test what changed" rules want.
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		d := "."
		if i := strings.Index(path.Clean(f), "/"); i >= 0 {
			d = path.Clean(f)[:i]
		}
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)

	return starlarkstruct.FromStringDict(starlark.String("ctx"), starlark.StringDict{
		"repo":          starlark.String(c.Repo),
		"sha":           starlark.String(c.SHA),
		"ref":           starlark.String(c.Ref),
		"changed_files": stringList(files),
		"changed_dirs":  stringList(dirs),
	})
}

func stringList(ss []string) *starlark.List {
	vals := make([]starlark.Value, len(ss))
	for i, s := range ss {
		vals[i] = starlark.String(s)
	}
	l := starlark.NewList(vals)
	l.Freeze()
	return l
}

// stepValue is what step() returns: an opaque handle a script can pass in
// another step's `needs`.
type stepValue struct{ Step }

func (s *stepValue) String() string        { return fmt.Sprintf("<step %q>", s.Name) }
func (s *stepValue) Type() string          { return "step" }
func (s *stepValue) Freeze()               {}
func (s *stepValue) Truth() starlark.Bool  { return starlark.True }
func (s *stepValue) Hash() (uint32, error) { return starlark.String(s.Name).Hash() }

func stepBuiltin(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, image, run string
	var needs *starlark.List
	var env *starlark.Dict
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"name", &name, "image", &image, "run", &run, "needs?", &needs, "env?", &env); err != nil {
		return nil, err
	}
	if name == "" || image == "" || run == "" {
		return nil, fmt.Errorf("step: name, image and run must not be empty")
	}

	s := Step{Name: name, Image: image, Run: run, Needs: []string{}, Env: map[string]string{}}
	if needs != nil {
		for i := 0; i < needs.Len(); i++ {
			switch v := needs.Index(i).(type) {
			case *stepValue:
				s.Needs = append(s.Needs, v.Name)
			case starlark.String:
				s.Needs = append(s.Needs, string(v))
			default:
				return nil, fmt.Errorf("step %q: needs must hold steps or step names, got %s", name, v.Type())
			}
		}
	}
	if env != nil {
		for _, item := range env.Items() {
			k, kok := item[0].(starlark.String)
			v, vok := item[1].(starlark.String)
			if !kok || !vok {
				return nil, fmt.Errorf("step %q: env keys and values must be strings", name)
			}
			s.Env[string(k)] = string(v)
		}
	}
	return &stepValue{s}, nil
}

func collect(result starlark.Value) ([]Step, error) {
	iter, ok := result.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("ci(ctx) must return a list of steps, got %s", result.Type())
	}
	it := iter.Iterate()
	defer it.Done()

	steps := []Step{}
	var v starlark.Value
	for it.Next(&v) {
		s, ok := v.(*stepValue)
		if !ok {
			return nil, fmt.Errorf("ci(ctx) must return only steps, got %s", v.Type())
		}
		steps = append(steps, s.Step)
	}
	return steps, nil
}

// validate rejects plans an executor could not run: duplicate names, needs
// that point at a step the script did not return, and dependency cycles.
func validate(steps []Step) error {
	byName := map[string]Step{}
	for _, s := range steps {
		if _, dup := byName[s.Name]; dup {
			return fmt.Errorf("two steps are named %q", s.Name)
		}
		byName[s.Name] = s
	}
	for _, s := range steps {
		for _, n := range s.Needs {
			if _, ok := byName[n]; !ok {
				return fmt.Errorf("step %q needs %q, which ci(ctx) did not return", s.Name, n)
			}
		}
	}

	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(name string, trail []string) error
	visit = func(name string, trail []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("steps depend on each other in a cycle: %s", strings.Join(append(trail, name), " -> "))
		}
		state[name] = visiting
		for _, n := range byName[name].Needs {
			if err := visit(n, append(trail, name)); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, s := range steps {
		if err := visit(s.Name, nil); err != nil {
			return err
		}
	}
	return nil
}
