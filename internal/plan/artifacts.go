package plan

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"go.starlark.net/starlark"
)

// An artifact is a file or directory a step keeps, so that a later step can
// be handed it. The store holds it by the hash of its content, and results
// and steps carry a pointer to it, never the bytes. A step sees another's
// artifact only when the script names it (docs/adr/0002-a-store-of-pointers.md).

// Pointer stands for an artifact in the store: a list of its files, found by
// the hash of that list.
type Pointer struct {
	Hash string `json:"hash,omitempty"`
	// Size is what its files add up to.
	Size int64 `json:"size,omitempty"`
	// From and Path stand for an artifact a step of a basic plan has yet to
	// keep: the step's name, and the path it keeps. Next puts the real
	// pointer in their place before the step that is handed it runs.
	From string `json:"from,omitempty"`
	Path string `json:"path,omitempty"`
}

func (p Pointer) placeholder() bool { return p.Hash == "" }

// artifactValue is a pointer, as a script holds it.
type artifactValue struct{ p Pointer }

func (a *artifactValue) String() string {
	if a.p.placeholder() {
		return fmt.Sprintf("<artifact %s of %q>", a.p.Path, a.p.From)
	}
	return fmt.Sprintf("<artifact %.12s>", a.p.Hash)
}
func (a *artifactValue) Type() string         { return "artifact" }
func (a *artifactValue) Freeze()              {}
func (a *artifactValue) Truth() starlark.Bool { return starlark.True }
func (a *artifactValue) Hash() (uint32, error) {
	return starlark.String(a.p.Hash + a.p.From + a.p.Path).Hash()
}

// artifactBuiltin is artifact(step, path): in ci(ctx), what a step will keep
// at path, for handing to a later step before either has run.
func artifactBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var from starlark.Value
	var kept string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "step", &from, "path", &kept); err != nil {
		return nil, err
	}
	if thread.Local(runnerKey) != nil {
		return nil, fmt.Errorf("artifact: in pipeline(ctx), hand on what a step kept as result.artifacts[%q]", kept)
	}
	var name string
	switch v := from.(type) {
	case *stepValue:
		name = v.Name
	case starlark.String:
		name = string(v)
	default:
		return nil, fmt.Errorf("artifact: expects a step or a step's name, got %s", from.Type())
	}
	if err := workspacePath(kept); err != nil {
		return nil, fmt.Errorf("artifact %q of %q: %v", kept, name, err)
	}
	return &artifactValue{Pointer{From: name, Path: path.Clean(kept)}}, nil
}

// workspacePath refuses a path that is not inside a step's workspace.
func workspacePath(p string) error {
	c := path.Clean(p)
	if p == "" || c == "." || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("must be a path inside the workspace")
	}
	return nil
}

// stepArtifacts reads a step's keep and artifacts arguments into s. A
// placeholder for another step's artifact makes s need that step.
func stepArtifacts(s *Step, keep *starlark.List, artifacts *starlark.Dict) error {
	if keep != nil {
		seen := map[string]bool{}
		for i := 0; i < keep.Len(); i++ {
			v, ok := keep.Index(i).(starlark.String)
			if !ok {
				return fmt.Errorf("step %q: keep holds paths, as strings; got %s", s.Name, keep.Index(i).Type())
			}
			if err := workspacePath(string(v)); err != nil {
				return fmt.Errorf("step %q: keep %q %v", s.Name, string(v), err)
			}
			if c := path.Clean(string(v)); !seen[c] {
				seen[c] = true
				s.Keep = append(s.Keep, c)
			}
		}
		sort.Strings(s.Keep)
	}
	if artifacts != nil {
		s.Artifacts = map[string]Pointer{}
		for _, item := range artifacts.Items() {
			at, ok := item[0].(starlark.String)
			if !ok {
				return fmt.Errorf("step %q: artifacts maps a path in the workspace to an artifact; got %s for a path", s.Name, item[0].Type())
			}
			a, ok := item[1].(*artifactValue)
			if !ok {
				return fmt.Errorf("step %q: artifacts maps a path in the workspace to an artifact (result.artifacts[...], or artifact(...) in ci(ctx)); got %s", s.Name, item[1].Type())
			}
			if err := workspacePath(string(at)); err != nil {
				return fmt.Errorf("step %q: artifact at %q %v", s.Name, string(at), err)
			}
			s.Artifacts[path.Clean(string(at))] = a.p
			if a.p.placeholder() && !contains(s.Needs, a.p.From) {
				s.Needs = append(s.Needs, a.p.From)
			}
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// checkArtifacts refuses a plan whose steps are handed artifacts that the
// steps they name do not keep.
func checkArtifacts(steps []Step) error {
	byName := map[string]Step{}
	for _, s := range steps {
		byName[s.Name] = s
	}
	for _, s := range steps {
		for _, k := range s.Keep {
			if err := workspacePath(k); err != nil {
				return fmt.Errorf("step %q: keep %q %v", s.Name, k, err)
			}
		}
		for at, p := range s.Artifacts {
			if err := workspacePath(at); err != nil {
				return fmt.Errorf("step %q: artifact at %q %v", s.Name, at, err)
			}
			if !p.placeholder() {
				continue
			}
			from, ok := byName[p.From]
			if !ok {
				return fmt.Errorf("step %q is handed an artifact of %q, which ci(ctx) did not return", s.Name, p.From)
			}
			if !contains(from.Keep, p.Path) {
				return fmt.Errorf("step %q is handed %s of %q, which %q does not keep (keep = [%q])", s.Name, p.Path, p.From, p.From, p.Path)
			}
		}
	}
	return nil
}

// resolve puts real pointers in place of a step's placeholders, from the
// results of the steps it names.
func resolve(s Step, byName map[string]Result) (Step, error) {
	placeholders := false
	for _, p := range s.Artifacts {
		placeholders = placeholders || p.placeholder()
	}
	if !placeholders {
		return s, nil
	}
	given := map[string]Pointer{}
	for at, p := range s.Artifacts {
		if p.placeholder() {
			real, ok := byName[p.From].Artifacts[p.Path]
			if !ok {
				return s, fmt.Errorf("%q passed without keeping %s", p.From, p.Path)
			}
			p = real
		}
		given[at] = p
	}
	s.Artifacts = given
	return s, nil
}

func artifactsValue(kept map[string]Pointer) starlark.Value {
	d := starlark.NewDict(len(kept))
	for _, k := range sortedPointerKeys(kept) {
		_ = d.SetKey(starlark.String(k), &artifactValue{kept[k]})
	}
	d.Freeze()
	return d
}

func sortedPointerKeys(m map[string]Pointer) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
