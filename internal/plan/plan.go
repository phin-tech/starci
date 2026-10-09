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
	"regexp"
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
	// Inputs are the repository paths the step is given, and the only ones it
	// can see. They are also what its result is keyed on: a step whose image,
	// command, env and input contents match an earlier passing run is not run
	// again. Defaults to the whole repository.
	Inputs []string `json:"inputs"`
	// Caches are directories kept between runs on a worker (package manager
	// caches and the like), as name -> path in the container. They speed a
	// step up; they are not part of what its result is keyed on.
	Caches map[string]string `json:"caches"`
	// Cache is false for a step that must run every time, because it depends
	// on something outside its inputs (the network, the date).
	Cache bool `json:"cache"`
	// Worker restricts the step to workers carrying these labels, for steps
	// that need particular hardware. Empty means any worker.
	Worker map[string]string `json:"worker"`
	// Prefer names labels the step would like but can do without: a matching
	// worker is chosen when one is free, any other when not.
	Prefer map[string]string `json:"prefer"`
	// Avoid keeps the step off workers carrying these labels, for work that
	// must not run in a particular place.
	Avoid map[string]string `json:"avoid"`
}

// Plan is the evaluated script: steps in the order the script returned them.
type Plan struct {
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	Ref   string `json:"ref"`
	Steps []Step `json:"steps"`
}

// Validate reports whether the plan can be executed. Eval only returns valid
// plans; this is for a plan that arrived as JSON from somewhere else.
func (p *Plan) Validate() error {
	for _, s := range p.Steps {
		if s.Name == "" || s.Image == "" || s.Run == "" {
			return fmt.Errorf("step %q: name, image and run must not be empty", s.Name)
		}
		if len(s.Inputs) == 0 {
			return fmt.Errorf("step %q: inputs must not be empty", s.Name)
		}
		for _, in := range s.Inputs {
			if c := path.Clean(in); in == "" || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
				return fmt.Errorf("step %q: input %q must be a path inside the repository", s.Name, in)
			}
		}
		for name, dir := range s.Caches {
			if !cacheName.MatchString(name) || !path.IsAbs(dir) {
				return fmt.Errorf("step %q: cache %q -> %q is not a valid name and absolute path", s.Name, name, dir)
			}
		}
	}
	return validate(p.Steps)
}

// Layers groups the steps so that each layer depends only on earlier ones.
// Within a layer, steps keep the order the script returned them, so an
// executor that starts them in this order starts them identically every time.
func (p *Plan) Layers() [][]Step {
	done := map[string]bool{}
	remaining := append([]Step(nil), p.Steps...)
	var layers [][]Step
	for len(remaining) > 0 {
		var ready, rest []Step
		for _, s := range remaining {
			ok := true
			for _, n := range s.Needs {
				ok = ok && done[n]
			}
			if ok {
				ready = append(ready, s)
			} else {
				rest = append(rest, s)
			}
		}
		if len(ready) == 0 {
			break // a cycle; Validate reports it
		}
		for _, s := range ready {
			done[s.Name] = true
		}
		layers = append(layers, ready)
		remaining = rest
	}
	return layers
}

// Eval runs the script in src and returns its plan. filename is used only in
// error messages.
func Eval(filename string, src []byte, c Context) (*Plan, error) {
	thread := newThread("plan")
	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, filename, src, predeclared())
	if err != nil {
		return nil, describe(err)
	}

	ci, ok := globals["ci"].(starlark.Callable)
	if !ok {
		if _, advanced := globals["pipeline"].(starlark.Callable); advanced {
			return nil, fmt.Errorf("%s: defines pipeline(ctx), whose steps are only known as it runs; there is no plan to compute", filename)
		}
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
	var needs, inputs *starlark.List
	var env, caches, worker, prefer, avoid *starlark.Dict
	cache := true
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"name", &name, "image", &image, "run", &run, "needs?", &needs, "env?", &env,
		"inputs?", &inputs, "caches?", &caches, "cache?", &cache,
		"worker?", &worker, "prefer?", &prefer, "avoid?", &avoid); err != nil {
		return nil, err
	}
	if name == "" || image == "" || run == "" {
		return nil, fmt.Errorf("step: name, image and run must not be empty")
	}

	s := Step{
		Name: name, Image: image, Run: run, Cache: cache,
		Needs: []string{}, Env: map[string]string{}, Inputs: []string{"."}, Caches: map[string]string{},
		Worker: map[string]string{}, Prefer: map[string]string{}, Avoid: map[string]string{},
	}
	// A label can be constrained one way per step: Hatchet takes a single
	// rule per label, and "require gpu, avoid gpu" has no meaning anyway.
	constrained := map[string]string{}
	for _, c := range []struct {
		arg  string
		from *starlark.Dict
		into map[string]string
	}{{"worker", worker, s.Worker}, {"prefer", prefer, s.Prefer}, {"avoid", avoid, s.Avoid}} {
		if c.from == nil {
			continue
		}
		for _, item := range c.from.Items() {
			k, kok := item[0].(starlark.String)
			v, vok := item[1].(starlark.String)
			if !kok || !vok || k == "" {
				return nil, fmt.Errorf("step %q: %s labels must be non-empty strings mapped to strings", name, c.arg)
			}
			if other, taken := constrained[string(k)]; taken {
				return nil, fmt.Errorf("step %q: label %q is in both %s and %s", name, string(k), other, c.arg)
			}
			constrained[string(k)] = c.arg
			c.into[string(k)] = string(v)
		}
	}
	if inputs != nil {
		paths, err := inputPaths(name, inputs)
		if err != nil {
			return nil, err
		}
		s.Inputs = paths
	}
	if caches != nil {
		for _, item := range caches.Items() {
			k, kok := item[0].(starlark.String)
			v, vok := item[1].(starlark.String)
			if !kok || !vok {
				return nil, fmt.Errorf("step %q: caches must map a name to a path", name)
			}
			if !cacheName.MatchString(string(k)) {
				return nil, fmt.Errorf("step %q: cache name %q may only use letters, digits, '-' and '_'", name, string(k))
			}
			if !path.IsAbs(string(v)) {
				return nil, fmt.Errorf("step %q: cache %q needs an absolute path in the container, got %q", name, string(k), string(v))
			}
			s.Caches[string(k)] = string(v)
		}
	}
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

var cacheName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// inputPaths cleans a step's inputs to sorted, unique paths inside the
// repository. Order and spelling must not matter, because the paths feed the
// key a step's result is stored under.
func inputPaths(step string, inputs *starlark.List) ([]string, error) {
	seen := map[string]bool{}
	paths := []string{}
	for i := 0; i < inputs.Len(); i++ {
		v, ok := inputs.Index(i).(starlark.String)
		if !ok {
			return nil, fmt.Errorf("step %q: inputs must be paths, got %s", step, inputs.Index(i).Type())
		}
		p := path.Clean(string(v))
		if string(v) == "" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("step %q: input %q must be a path inside the repository", step, string(v))
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("step %q: inputs must not be empty (omit it to give the step the whole repository)", step)
	}
	if seen["."] {
		return []string{"."}, nil
	}
	sort.Strings(paths)
	return paths, nil
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
