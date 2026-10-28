package plan

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"go.starlark.net/starlark"
)

// match_files and paths_filter are what scripts use for the things people
// reach for tj-actions/changed-files and dorny/paths-filter to do. They take a
// list of paths, usually ctx.changed_files, rather than ctx itself, so that
// reading the list is still seen as reading ctx.changed_files.
//
// Patterns are globs matched against the whole path: `*` and `?` stop at a
// `/`, `**` does not, `**/` also matches no directory at all, `{a,b}` is
// either, and `[abc]` is one of those characters. A dot is an ordinary
// character. A pattern starting with `!` removes what it matches from what the
// other patterns in its list matched.

var globs sync.Map // pattern -> *regexp.Regexp

func globRegexp(pattern string) (*regexp.Regexp, error) {
	if re, ok := globs.Load(pattern); ok {
		return re.(*regexp.Regexp), nil
	}
	var b strings.Builder
	b.WriteString("^")
	braces := 0
	p := pattern
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if strings.HasPrefix(p[i:], "**/") {
				b.WriteString("(?:.*/)?")
				i += 2
			} else if strings.HasPrefix(p[i:], "**") {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 2 {
				return nil, fmt.Errorf("pattern %q: [ is not closed", pattern)
			}
			class := p[i+1 : i+end]
			if class[0] == '!' {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end
		case '{':
			braces++
			b.WriteString("(?:")
		case '}':
			if braces == 0 {
				return nil, fmt.Errorf("pattern %q: } has no {", pattern)
			}
			braces--
			b.WriteString(")")
		case ',':
			if braces > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if braces != 0 {
		return nil, fmt.Errorf("pattern %q: { is not closed", pattern)
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %v", pattern, err)
	}
	globs.Store(pattern, re)
	return re, nil
}

// matches reports whether path is selected by patterns: some pattern without
// a `!` matches it and no pattern with one does.
func matches(patterns []string, path string) (bool, error) {
	in := false
	for _, pat := range patterns {
		negate := strings.HasPrefix(pat, "!")
		re, err := globRegexp(strings.TrimPrefix(pat, "!"))
		if err != nil {
			return false, err
		}
		if !re.MatchString(path) {
			continue
		}
		if negate {
			return false, nil
		}
		in = true
	}
	return in, nil
}

func stringsOf(name string, v starlark.Value) ([]string, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("%s: got %s, want a list of strings", name, v.Type())
	}
	var out []string
	iter := it.Iterate()
	defer iter.Done()
	var x starlark.Value
	for iter.Next(&x) {
		s, ok := starlark.AsString(x)
		if !ok {
			return nil, fmt.Errorf("%s: got %s in the list, want strings", name, x.Type())
		}
		out = append(out, s)
	}
	return out, nil
}

// match_files(files, include = [], exclude = []): the paths that match any
// include pattern (all of them, when there are none) and no exclude pattern.
func matchFilesBuiltin(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var filesV, includeV, excludeV starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "files", &filesV, "include?", &includeV, "exclude?", &excludeV); err != nil {
		return nil, err
	}
	files, err := stringsOf("files", filesV)
	if err != nil {
		return nil, err
	}
	include, err := stringsOf("include", includeV)
	if err != nil {
		return nil, err
	}
	exclude, err := stringsOf("exclude", excludeV)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if len(include) > 0 {
			if ok, err := matches(include, f); err != nil {
				return nil, err
			} else if !ok {
				continue
			}
		}
		if len(exclude) > 0 {
			if ok, err := matches(exclude, f); err != nil {
				return nil, err
			} else if ok {
				continue
			}
		}
		out = append(out, f)
	}
	return stringList(out), nil
}

// paths_filter(files, filters): a dict with a key for each filter, whose value
// is the paths that filter's patterns select. An empty list is false, so
// `if hits["api"]:` reads as "did anything in api change".
func pathsFilterBuiltin(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var filesV starlark.Value
	var filters *starlark.Dict
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "files", &filesV, "filters", &filters); err != nil {
		return nil, err
	}
	files, err := stringsOf("files", filesV)
	if err != nil {
		return nil, err
	}
	out := starlark.NewDict(filters.Len())
	for _, item := range filters.Items() {
		name, ok := starlark.AsString(item[0])
		if !ok {
			return nil, fmt.Errorf("paths_filter: filter names are strings, got %s", item[0].Type())
		}
		patterns, err := stringsOf("filter "+name, item[1])
		if err != nil {
			return nil, err
		}
		var hit []string
		for _, f := range files {
			if ok, err := matches(patterns, f); err != nil {
				return nil, err
			} else if ok {
				hit = append(hit, f)
			}
		}
		if err := out.SetKey(item[0], stringList(hit)); err != nil {
			return nil, err
		}
	}
	return out, nil
}
