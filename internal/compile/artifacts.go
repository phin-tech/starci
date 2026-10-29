package compile

import (
	"fmt"
	"sort"

	"git.phin.tech/sam/starci/internal/plan"
)

// A compiled job keeps and is handed artifacts the way a machine does, as
// shell. What a job keeps goes in its record of a pass, beside the mark that
// it passed, so that a job skipped because it passed before still hands on
// what it kept then. It is handed on through the CI tool's own way of moving
// files between the jobs of one run, packed with tar so that executable files
// stay executable. A job handed an artifact is keyed on a hash of its
// content, worked out the same way wherever it is asked: the same files are
// the same key, whichever job made them and whether it ran or was skipped.

// keptHash is a shell function: the hash of the files under a path, each by
// its content, whether it is executable, and where it is under the path.
const keptHash = `kept_hash() { find "$1" -type f | LC_ALL=C sort | while IFS= read -r f; do printf '%s %s %s\n' "$(sha256sum < "$f" | cut -c1-64)" "$([ -x "$f" ] && echo x || echo -)" "${f#"$1"}"; done | sha256sum | cut -c1-64; }`

// keepsArtifacts reports whether any step of p keeps something.
func keepsArtifacts(p *plan.Plan) bool {
	for _, s := range p.Steps {
		if len(s.Keep) > 0 {
			return true
		}
	}
	return false
}

// keepLines copy the paths a step keeps from the workspace into dir, as
// shell, failing for one the step did not make.
func keepLines(s plan.Step, dirs ...string) []string {
	var lines []string
	for _, d := range dirs {
		lines = append(lines, fmt.Sprintf("rm -rf %s && mkdir -p %s", d, d))
	}
	for _, k := range s.Keep {
		lines = append(lines,
			fmt.Sprintf("if [ ! -e %s ]; then echo %s; exit 1; fi", shq(k), shq("keep "+k+": the step passed without making it")))
		for _, d := range dirs {
			to := d + "/" + shq(k)
			lines = append(lines, fmt.Sprintf("mkdir -p \"$(dirname %s)\" && cp -R %s %s", to, shq(k), to))
		}
	}
	return lines
}

// handedFrom lists the steps whose artifacts s is handed, each once, in order.
func handedFrom(s plan.Step) []string {
	seen := map[string]bool{}
	var from []string
	for _, p := range s.Artifacts {
		if !seen[p.From] {
			seen[p.From] = true
			from = append(from, p.From)
		}
	}
	sort.Strings(from)
	return from
}

// artifactPaths are where s is handed artifacts, in order.
func artifactPaths(s plan.Step) []string {
	paths := make([]string, 0, len(s.Artifacts))
	for p := range s.Artifacts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// keptIndex is which of a step's kept paths p is, counted from 1, for naming
// the output that carries its hash.
func keptIndex(from plan.Step, p string) int {
	for i, k := range from.Keep {
		if k == p {
			return i + 1
		}
	}
	return 0
}

// placeLines put each artifact s is handed at its path in the workspace, as
// shell. source gives, for the step an artifact is from, the directory its
// kept paths were unpacked into.
func placeLines(s plan.Step, source func(from string) string) []string {
	var lines []string
	for _, at := range artifactPaths(s) {
		p := s.Artifacts[at]
		lines = append(lines,
			fmt.Sprintf("if [ -e %s ]; then echo %s; exit 1; fi", shq(at), shq("artifact at "+at+": it is already in the workspace, from the step's inputs")),
			fmt.Sprintf("mkdir -p \"$(dirname %s)\" && cp -R %s/%s %s", shq(at), source(p.From), shq(p.Path), shq(at)))
	}
	return lines
}

// checkPlaceholders refuses an artifact a compiled job could not be handed:
// in a plan, every one stands for what a step of the plan keeps.
func checkPlaceholders(s plan.Step) error {
	for at, p := range s.Artifacts {
		if p.From == "" {
			return fmt.Errorf("step %q: the artifact at %q is already a pointer, which a workflow cannot fetch", s.Name, at)
		}
	}
	return nil
}
