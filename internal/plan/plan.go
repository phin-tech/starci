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
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// maxSteps bounds how long a script may run, so `while`-style mistakes (or a
// hostile script) end in an error rather than a hung planner.
const maxSteps = 10_000_000

// Context is everything a script is told about the run.
type Context struct {
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
	// Ref is the branch, for a push or a manual run of one; the branch a pull
	// request would merge from; empty for a tag.
	Ref          string   `json:"ref"`
	ChangedFiles []string `json:"changed_files"`
	// Event is why the run was started: push, tag, pull_request, schedule or
	// manual. Empty means push, which is what a run with nothing else known
	// about it is taken to be.
	Event string `json:"event,omitempty"`
	// Tag is the tag's name, for a tag.
	Tag string `json:"tag,omitempty"`
	// PR describes the pull request, for a pull_request.
	PR *PullRequest `json:"pr,omitempty"`
	// Schedule is the name of the schedule that fired, for a schedule.
	Schedule string `json:"schedule,omitempty"`
}

// PullRequest is what a script is told about a pull request.
type PullRequest struct {
	Number int `json:"number"`
	// Base is the branch it would merge into; Head the branch it comes from.
	Base string `json:"base"`
	Head string `json:"head"`
	// Fork is true when it comes from another repository, which is to say
	// from someone who cannot push to this one.
	Fork bool `json:"fork"`
}

// The events a run can be started by.
const (
	EventPush        = "push"
	EventTag         = "tag"
	EventPullRequest = "pull_request"
	EventSchedule    = "schedule"
	EventManual      = "manual"
)

// Schedule is a time-based trigger a script declares at its top level:
//
//	schedule("nightly", cron = "0 3 * * *")
//
// Every other event is caused by something. A schedule is caused by nothing,
// so it is the one trigger that has to be written down.
type Schedule struct {
	Name string `json:"name"`
	// Cron is five fields, read in UTC.
	Cron string `json:"cron"`
	// Overlap says what happens when the schedule fires while its last run
	// is still going: "skip" the new one (the default), "replace" the old
	// one, or "allow" both.
	Overlap string `json:"overlap"`
	// CatchUp asks for one run when a firing was missed because nothing was
	// there to fire it, however many were missed.
	CatchUp bool `json:"catch_up"`
}

// What Schedule.Overlap may be.
const (
	OverlapSkip    = "skip"
	OverlapReplace = "replace"
	OverlapAllow   = "allow"
)

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
	// Tools are put on the step's PATH, ahead of what the image has.
	Tools []Tool `json:"tools"`
	// Packages are installed on top of the image with apt before the step
	// runs, for the system libraries a tool cannot carry. The image must be
	// Debian or Ubuntu based.
	Packages []string `json:"packages"`
	// Offline runs the step with no network, so that it cannot pass on
	// something its inputs do not hold. It is the negative so that a plan
	// written before the field existed still means "with the network".
	Offline bool `json:"offline,omitempty"`
	// Outputs are paths in the step's working directory that it produces and
	// that are kept when it passes, for other steps to be given. They are
	// kept under their content, so a step that is skipped because of an
	// earlier pass hands on exactly what that pass produced.
	Outputs []string `json:"outputs,omitempty"`
	// From names the steps whose outputs this step is given, at the paths
	// they were produced at. It is what a script says; Given is what that
	// came to once those steps had run.
	From []string `json:"from,omitempty"`
	// Given is filled in as a run unfolds, never by a script: each output of
	// each step in From, with the content it turned out to have. It is part
	// of what the step's result is keyed on, as the content of an input is.
	Given []Given `json:"given,omitempty"`
}

// Given is one output of an earlier step, handed to a later one.
type Given struct {
	// Step is the step that produced it.
	Step string `json:"step"`
	// Path is where it was produced, and where it is put.
	Path string `json:"path"`
	// Digest identifies its content.
	Digest string `json:"digest"`
}

// Tool is a program a step wants on its PATH without it being in the image:
// an archive (or a single binary) to download, pinned by checksum.
//
// The checksum is the tool's identity. It is what a worker's copy is stored
// under and what a step's result is keyed on, so two tools with the same
// name and different contents are different tools, and "the latest version"
// cannot be expressed.
type Tool struct {
	Name string `json:"name"`
	// URL is where to download it. "{arch}" is replaced with the name of the
	// architecture the step's image runs on.
	URL string `json:"url"`
	// SHA256 is the checksum of the download for each architecture, keyed by
	// amd64 or arm64. An architecture not listed is not supported.
	SHA256 map[string]string `json:"sha256"`
	// ArchNames is how this tool's publisher spells an architecture, where it
	// differs: {"amd64": "x64", "arm64": "aarch64"}.
	ArchNames map[string]string `json:"arch_names"`
	// Bin lists the directories inside the archive to put on PATH, "{arch}"
	// replaced as in URL. The default is the archive's top level.
	Bin []string `json:"bin"`
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
		// A step given another's outputs may need nothing from the repository.
		if len(s.Inputs) == 0 && len(s.From) == 0 && len(s.Given) == 0 {
			return fmt.Errorf("step %q: inputs must not be empty", s.Name)
		}
		for _, in := range s.Inputs {
			if c := path.Clean(in); in == "" || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
				return fmt.Errorf("step %q: input %q must be a path inside the repository", s.Name, in)
			}
		}
		for _, out := range s.Outputs {
			if !outputPath(out) {
				return fmt.Errorf("step %q: output %q must be a file or directory inside the working directory, written without wildcards", s.Name, out)
			}
		}
		for name, dir := range s.Caches {
			if !cacheName.MatchString(name) || !path.IsAbs(dir) {
				return fmt.Errorf("step %q: cache %q -> %q is not a valid name and absolute path", s.Name, name, dir)
			}
		}
		for _, t := range s.Tools {
			if err := t.validate(); err != nil {
				return fmt.Errorf("step %q: %w", s.Name, err)
			}
		}
		for _, pkg := range s.Packages {
			if !packageName.MatchString(pkg) {
				return fmt.Errorf("step %q: %q is not an apt package name", s.Name, pkg)
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
	p, _, err := EvalTracked(filename, src, c)
	return p, err
}

// EvalTracked is Eval that also reports which fields of ctx the script read.
// A plan that read none of them is the same for every commit; one that read
// ctx.changed_files or ctx.ref is this commit's plan and may not be the next
// one's. Anything that wants to freeze a plan needs to know which it has.
func EvalTracked(filename string, src []byte, c Context) (*Plan, []string, error) {
	p, read, err := evalTracking(filename, src, c)
	sort.Strings(read)
	return p, read, err
}

func evalTracking(filename string, src []byte, c Context) (*Plan, []string, error) {
	tracked := &trackedContext{inner: contextValue(c).(starlark.HasAttrs), read: map[string]bool{}}
	p, err := evalWith(filename, src, c, tracked)
	var read []string
	for name := range tracked.read {
		read = append(read, name)
	}
	return p, read, err
}

// trackedContext is ctx, remembering which of its fields were asked for.
type trackedContext struct {
	inner starlark.HasAttrs
	read  map[string]bool
}

func (t *trackedContext) String() string        { return t.inner.String() }
func (t *trackedContext) Type() string          { return "ctx" }
func (t *trackedContext) Freeze()               { t.inner.Freeze() }
func (t *trackedContext) Truth() starlark.Bool  { return starlark.True }
func (t *trackedContext) Hash() (uint32, error) { return 0, fmt.Errorf("ctx is not hashable") }
func (t *trackedContext) AttrNames() []string   { return t.inner.AttrNames() }
func (t *trackedContext) Attr(name string) (starlark.Value, error) {
	v, err := t.inner.Attr(name)
	if v != nil {
		t.read[name] = true
	}
	return v, err
}

func evalWith(filename string, src []byte, c Context, ctxValue starlark.Value) (*Plan, error) {
	thread := newThread("plan")
	globals, _, err := load(thread, filename, src)
	if err != nil {
		return nil, err
	}

	ci, ok := globals["ci"].(starlark.Callable)
	if !ok {
		if _, advanced := globals["pipeline"].(starlark.Callable); advanced {
			return nil, fmt.Errorf("%s: defines pipeline(ctx), whose steps are only known as it runs; there is no plan to compute", filename)
		}
		return nil, fmt.Errorf("%s: must define a function ci(ctx)", filename)
	}
	result, err := starlark.Call(thread, ci, starlark.Tuple{ctxValue}, nil)
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

	event := c.Event
	if event == "" {
		event = EventPush
	}
	// None when there is no pull request, so that `if ctx.pr:` reads naturally.
	var pr starlark.Value = starlark.None
	if c.PR != nil {
		pr = starlarkstruct.FromStringDict(starlark.String("pr"), starlark.StringDict{
			"number": starlark.MakeInt(c.PR.Number),
			"base":   starlark.String(c.PR.Base),
			"head":   starlark.String(c.PR.Head),
			"fork":   starlark.Bool(c.PR.Fork),
		})
	}

	return starlarkstruct.FromStringDict(starlark.String("ctx"), starlark.StringDict{
		"repo":          starlark.String(c.Repo),
		"sha":           starlark.String(c.SHA),
		"ref":           starlark.String(c.Ref),
		"changed_files": stringList(files),
		"changed_dirs":  stringList(dirs),
		"event":         starlark.String(event),
		"tag":           starlark.String(c.Tag),
		"pr":            pr,
		"schedule":      starlark.String(c.Schedule),
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
	var needs, inputs, tools, packages, outputs *starlark.List
	var env, caches, worker, prefer, avoid *starlark.Dict
	cache, network := true, true
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"name", &name, "image", &image, "run", &run, "needs?", &needs, "env?", &env,
		"inputs?", &inputs, "caches?", &caches, "cache?", &cache,
		"worker?", &worker, "prefer?", &prefer, "avoid?", &avoid, "tools?", &tools,
		"packages?", &packages, "network?", &network, "outputs?", &outputs); err != nil {
		return nil, err
	}
	if name == "" || image == "" || run == "" {
		return nil, fmt.Errorf("step: name, image and run must not be empty")
	}

	s := Step{
		Name: name, Image: image, Run: run, Cache: cache, Offline: !network,
		Needs: []string{}, Env: map[string]string{}, Inputs: []string{"."}, Caches: map[string]string{},
		Worker: map[string]string{}, Prefer: map[string]string{}, Avoid: map[string]string{},
		Tools: []Tool{}, Packages: []string{},
	}
	if packages != nil {
		seen := map[string]bool{}
		for i := 0; i < packages.Len(); i++ {
			v, ok := packages.Index(i).(starlark.String)
			if !ok || !packageName.MatchString(string(v)) {
				return nil, fmt.Errorf("step %q: packages must be apt package names, optionally name=version; got %s", name, packages.Index(i).String())
			}
			if !seen[string(v)] {
				seen[string(v)] = true
				s.Packages = append(s.Packages, string(v))
			}
		}
		// Sorted, so that the same set always names the same environment.
		sort.Strings(s.Packages)
	}
	if tools != nil {
		seen := map[string]bool{}
		for i := 0; i < tools.Len(); i++ {
			tv, ok := tools.Index(i).(*toolValue)
			if !ok {
				return nil, fmt.Errorf("step %q: tools must hold tool(...) values, got %s", name, tools.Index(i).Type())
			}
			if seen[tv.Name] {
				return nil, fmt.Errorf("step %q: tool %q is listed twice", name, tv.Name)
			}
			seen[tv.Name] = true
			s.Tools = append(s.Tools, tv.Tool)
		}
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
		paths, from, err := inputPaths(name, inputs)
		if err != nil {
			return nil, err
		}
		s.Inputs, s.From = paths, from
	}
	if outputs != nil {
		seen := map[string]bool{}
		for i := 0; i < outputs.Len(); i++ {
			v, ok := outputs.Index(i).(starlark.String)
			if !ok || !outputPath(string(v)) {
				return nil, fmt.Errorf("step %q: outputs must be files or directories inside the working directory, written without wildcards; got %s", name, outputs.Index(i).String())
			}
			if p := path.Clean(string(v)); !seen[p] {
				seen[p] = true
				s.Outputs = append(s.Outputs, p)
			}
		}
		// Sorted, as inputs are: the order they are written in is not a
		// reason to run a step again.
		sort.Strings(s.Outputs)
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

// toolValue is what tool() returns, for a step's tools.
type toolValue struct{ Tool }

func (t *toolValue) String() string        { return fmt.Sprintf("<tool %q>", t.Name) }
func (t *toolValue) Type() string          { return "tool" }
func (t *toolValue) Freeze()               {}
func (t *toolValue) Truth() starlark.Bool  { return starlark.True }
func (t *toolValue) Hash() (uint32, error) { return starlark.String(t.Name).Hash() }

var (
	// What apt accepts as a package, with an optional =version. Nothing that
	// could be read as an option or as shell.
	packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(:[a-z0-9]+)?(=[A-Za-z0-9.+:~-]+)?$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	archs       = map[string]bool{"amd64": true, "arm64": true}
)

func toolBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, url string
	var sums, archNames *starlark.Dict
	var bin *starlark.List
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"name", &name, "url", &url, "sha256", &sums, "arch_names?", &archNames, "bin?", &bin); err != nil {
		return nil, err
	}
	t := Tool{Name: name, URL: url, SHA256: map[string]string{}, ArchNames: map[string]string{}, Bin: []string{"."}}
	strs := func(d *starlark.Dict, what string, into map[string]string) error {
		if d == nil {
			return nil
		}
		for _, item := range d.Items() {
			k, kok := item[0].(starlark.String)
			v, vok := item[1].(starlark.String)
			if !kok || !vok {
				return fmt.Errorf("tool %q: %s must map strings to strings", name, what)
			}
			into[string(k)] = string(v)
		}
		return nil
	}
	if err := strs(sums, "sha256", t.SHA256); err != nil {
		return nil, err
	}
	if err := strs(archNames, "arch_names", t.ArchNames); err != nil {
		return nil, err
	}
	if bin != nil {
		t.Bin = nil
		for i := 0; i < bin.Len(); i++ {
			v, ok := bin.Index(i).(starlark.String)
			if !ok {
				return nil, fmt.Errorf("tool %q: bin must list directories inside the archive", name)
			}
			t.Bin = append(t.Bin, string(v))
		}
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	if declared, ok := thread.Local(toolsKey).(*[]Tool); ok {
		*declared = append(*declared, t)
	}
	return &toolValue{t}, nil
}

const toolsKey = "starci.tools"

// Tools returns every tool the script declares, in the order it declares
// them, whether or not a step uses it: a tool kept for one branch is as much
// a part of the script as one every commit needs.
//
// The file's top level is evaluated, and in basic mode ci(ctx) is called for
// a commit about which nothing is known. A tool declared inside pipeline(ctx),
// or in a part of ci(ctx) that commit does not reach, is not seen; declaring
// tools at the top level, as a script normally does, avoids both.
func Tools(filename string, src []byte) ([]Tool, error) {
	thread := newThread("tools")
	declared := []Tool{}
	thread.SetLocal(toolsKey, &declared)
	globals, _, err := load(thread, filename, src)
	if err != nil {
		return nil, err
	}
	if ci, ok := globals["ci"].(starlark.Callable); ok {
		if _, err := starlark.Call(thread, ci, starlark.Tuple{contextValue(Context{})}, nil); err != nil {
			return nil, describe(err)
		}
	} else if _, ok := globals["pipeline"].(starlark.Callable); !ok {
		return nil, fmt.Errorf("%s: must define ci(ctx) or pipeline(ctx)", filename)
	}

	// The same declaration reached twice, by a helper called in a loop, is
	// one tool.
	seen := map[string]bool{}
	tools := []Tool{}
	for _, t := range declared {
		id, _ := json.Marshal(t)
		if !seen[string(id)] {
			seen[string(id)] = true
			tools = append(tools, t)
		}
	}
	return tools, nil
}

func (t Tool) validate() error {
	if !cacheName.MatchString(t.Name) {
		return fmt.Errorf("tool %q: a name may only use letters, digits, '-' and '_'", t.Name)
	}
	if !strings.HasPrefix(t.URL, "https://") && !strings.HasPrefix(t.URL, "http://") {
		return fmt.Errorf("tool %q: url must be an http(s) address, got %q", t.Name, t.URL)
	}
	if len(t.SHA256) == 0 {
		return fmt.Errorf("tool %q: sha256 must give a checksum for at least one of amd64, arm64", t.Name)
	}
	for arch, sum := range t.SHA256 {
		if !archs[arch] {
			return fmt.Errorf("tool %q: sha256 is keyed by amd64 or arm64, not %q", t.Name, arch)
		}
		if !sha256Hex.MatchString(sum) {
			return fmt.Errorf("tool %q: the %s checksum must be 64 lowercase hex digits", t.Name, arch)
		}
	}
	for arch := range t.ArchNames {
		if !archs[arch] {
			return fmt.Errorf("tool %q: arch_names is keyed by amd64 or arm64, not %q", t.Name, arch)
		}
	}
	if len(t.Bin) == 0 {
		return fmt.Errorf("tool %q: bin must not be empty (omit it for the archive's top level)", t.Name)
	}
	for _, b := range t.Bin {
		if c := path.Clean(b); b == "" || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
			return fmt.Errorf("tool %q: bin entry %q must be a directory inside the archive", t.Name, b)
		}
	}
	return nil
}

// inputPaths cleans a step's inputs to sorted, unique paths inside the
// repository. Order and spelling must not matter, because the paths feed the
// key a step's result is stored under.
//
// An input may also be a step, which stands for that step's outputs. Those
// are returned apart, as the names of the steps: what they hold is not known
// until the steps have run.
func inputPaths(step string, inputs *starlark.List) (paths, from []string, err error) {
	seen, seenStep := map[string]bool{}, map[string]bool{}
	paths = []string{}
	for i := 0; i < inputs.Len(); i++ {
		if producer, ok := inputs.Index(i).(*stepValue); ok {
			if len(producer.Outputs) == 0 {
				return nil, nil, fmt.Errorf("step %q: step %q is given as an input, but declares no outputs", step, producer.Name)
			}
			if producer.Name == step {
				return nil, nil, fmt.Errorf("step %q: a step cannot be given its own outputs", step)
			}
			if !seenStep[producer.Name] {
				seenStep[producer.Name] = true
				from = append(from, producer.Name)
			}
			continue
		}
		v, ok := inputs.Index(i).(starlark.String)
		if !ok {
			return nil, nil, fmt.Errorf("step %q: inputs must be paths, or steps that declare outputs; got %s", step, inputs.Index(i).Type())
		}
		p := path.Clean(string(v))
		if string(v) == "" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return nil, nil, fmt.Errorf("step %q: input %q must be a path inside the repository", step, string(v))
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(from)
	if len(paths) == 0 {
		if len(from) == 0 {
			return nil, nil, fmt.Errorf("step %q: inputs must not be empty (omit it to give the step the whole repository)", step)
		}
		// Only what other steps produced, and nothing from the repository.
		return paths, from, nil
	}
	if seen["."] {
		return []string{"."}, from, nil
	}
	sort.Strings(paths)
	return paths, from, nil
}

// outputPath reports whether p names one file or directory beneath a step's
// working directory. Not the directory itself: a step that kept everything
// would hand its inputs on as if it had made them.
func outputPath(p string) bool {
	c := path.Clean(p)
	return p != "" && c != "." && !path.IsAbs(c) && c != ".." && !strings.HasPrefix(c, "../") && !strings.ContainsAny(p, "*?[")
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
		step := s.Step
		// A step given another's outputs has to come after it. In a script
		// that runs its own steps the order is the script's; here it has to
		// be said, and saying it twice would be noise.
		for _, producer := range step.From {
			if !contains(step.Needs, producer) {
				step.Needs = append(append([]string(nil), step.Needs...), producer)
			}
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
		for _, n := range s.From {
			producer, ok := byName[n]
			if !ok {
				return fmt.Errorf("step %q is given the outputs of %q, which ci(ctx) did not return", s.Name, n)
			}
			if len(producer.Outputs) == 0 {
				return fmt.Errorf("step %q is given the outputs of %q, which declares none", s.Name, n)
			}
			if !contains(s.Needs, n) {
				return fmt.Errorf("step %q is given the outputs of %q but does not need it", s.Name, n)
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
