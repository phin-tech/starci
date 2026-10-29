package compile

import (
	"fmt"
	"strings"
)

// A small YAML writer for the targets whose files are a tree of values rather
// than a page of text (Argo): an obj keeps its keys in the order given, so that
// a file reads the way its author would write it.

type kv struct {
	k string
	v any
}

type obj []kv

// yamlLines writes v as YAML lines indented by indent spaces. Scalars are
// strings, ints and bools; a string is quoted unless it is several lines.
func yamlLines(v any, indent int) []string {
	pad := strings.Repeat(" ", indent)
	switch x := v.(type) {
	case obj:
		var lines []string
		for _, e := range x {
			switch c := e.v.(type) {
			case obj:
				if len(c) == 0 {
					lines = append(lines, fmt.Sprintf("%s%s: {}", pad, e.k))
					continue
				}
				lines = append(lines, pad+e.k+":")
				lines = append(lines, yamlLines(c, indent+2)...)
			case []any:
				if len(c) == 0 {
					lines = append(lines, fmt.Sprintf("%s%s: []", pad, e.k))
					continue
				}
				lines = append(lines, pad+e.k+":")
				lines = append(lines, yamlLines(c, indent+2)...)
			default:
				lines = append(lines, pad+e.k+": "+scalar(c, pad+"  "))
			}
		}
		return lines
	case []any:
		var lines []string
		for _, e := range x {
			switch c := e.(type) {
			case obj:
				inner := yamlLines(c, indent+2)
				inner[0] = pad + "- " + strings.TrimPrefix(inner[0], pad+"  ")
				lines = append(lines, inner...)
			default:
				lines = append(lines, pad+"- "+scalar(c, pad+"  "))
			}
		}
		return lines
	}
	return []string{pad + scalar(v, pad)}
}

func scalar(v any, indent string) string {
	switch x := v.(type) {
	case string:
		if strings.Contains(x, "\n") {
			return block(x, indent)
		}
		return q(x)
	case int:
		return fmt.Sprint(x)
	case bool:
		return fmt.Sprint(x)
	}
	panic(fmt.Sprintf("yaml: cannot write %T", v))
}

func strs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
