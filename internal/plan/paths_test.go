package plan

import (
	"strings"
	"testing"
)

func TestMatchFilesAndPathsFilter(t *testing.T) {
	files := []string{".github/ci.yml", "README.md", "api/main.go", "api/sub/util.go", "api/sub/util_test.go", "docs/a/b.md", "ui/app.tsx", "ui/app.ts"}
	src := `
def ci(ctx):
    got = []
    got.append(",".join(match_files(ctx.changed_files, include = ["**/*.md"])))
    got.append(",".join(match_files(ctx.changed_files, include = ["api/**"], exclude = ["**/*_test.go"])))
    got.append(",".join(match_files(ctx.changed_files, include = ["api/*"])))
    got.append(",".join(match_files(ctx.changed_files, include = ["ui/*.{ts,tsx}"])))
    got.append(",".join(match_files(ctx.changed_files, include = [".github/**"])))
    got.append(",".join(match_files(ctx.changed_files, exclude = ["**"])))
    got.append(",".join(match_files(ctx.changed_files, include = ["api/**", "!api/sub/**"])))
    hits = paths_filter(ctx.changed_files, {"go": ["**/*.go"], "docs": ["docs/**", "*.md"], "none": ["zzz/**"]})
    got.append("%d/%d/%d" % (len(hits["go"]), len(hits["docs"]), len(hits["none"])))
    got.append("none=%s" % bool(hits["none"]))
    return [step("%d %s" % (i, g.replace(",", " ") or "empty"), image = "alpine", run = "true") for i, g in enumerate(got)]
`
	p, err := Eval("t.ci.star", []byte(src), Context{Repo: "r", SHA: "s", ChangedFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.Steps {
		names = append(names, s.Name[strings.Index(s.Name, " ")+1:])
	}
	want := []string{
		"README.md docs/a/b.md",
		"api/main.go api/sub/util.go",
		"api/main.go",
		"ui/app.ts ui/app.tsx",
		".github/ci.yml",
		"empty",
		"api/main.go",
		"3/2/0",
		"none=False",
	}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("got\n  %q\nwant\n  %q", names, want)
	}
}

func TestMatchFilesReadsChangedFiles(t *testing.T) {
	src := `
def ci(ctx):
    return [step("a", image = "alpine", run = "true")] if match_files(ctx.changed_files, include = ["x"]) == [] else []
`
	_, read, err := EvalTracked("t.ci.star", []byte(src), Context{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(read, ",") != "changed_files" {
		t.Errorf("read = %v, want changed_files", read)
	}
}

func TestBadPatterns(t *testing.T) {
	for _, pat := range []string{"a[", "{a,b", "a}"} {
		src := `
def ci(ctx):
    match_files(["a"], include = ["` + pat + `"])
    return []
`
		if _, err := Eval("t.ci.star", []byte(src), Context{}); err == nil {
			t.Errorf("pattern %q was accepted", pat)
		}
	}
}
