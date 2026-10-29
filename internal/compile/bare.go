package compile

import (
	"fmt"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// bareRun is the shell that runs a bare step on the CI machine itself, the
// way a machine runs one: in the checkout, with a home and temporary
// directory of its own, the machine's PATH with its tools in front, and
// nothing else of the machine's environment. The command is in STARCI_RUN.
//
// toolDirs are the directories to put on PATH and caches the directory for
// each of the step's caches, as shell, keyed by the variable that names it.
func bareRun(s plan.Step, toolDirs []string, caches map[string]string) []string {
	lines := []string{
		`starci_root="$(mktemp -d)"`,
		`mkdir "$starci_root/home" "$starci_root/tmp"`,
	}
	args := []string{`HOME="$starci_root/home"`, `TMPDIR="$starci_root/tmp"`, "LANG=C.UTF-8", `STARCI_WORKSPACE="$PWD"`}
	for _, k := range sortedKeys(s.Env) {
		if k != "PATH" {
			args = append(args, shq(k+"="+s.Env[k]))
		}
	}
	for _, v := range sortedKeys(caches) {
		lines = append(lines, fmt.Sprintf(`mkdir -p "%s"`, caches[v]))
		args = append(args, fmt.Sprintf(`%s="%s"`, v, caches[v]))
	}
	// A step that sets its own PATH means it; tools still go in front.
	base := `"$PATH"`
	if p, ok := s.Env["PATH"]; ok {
		base = shq(p)
	}
	front := ""
	if len(toolDirs) > 0 {
		front = `"` + strings.Join(toolDirs, ":") + `:"`
	}
	args = append(args, "PATH="+front+base)
	run := "env -i \\\n"
	for _, a := range args {
		run += "  " + a + " \\\n"
	}
	return append(lines, run+`  /bin/sh -c "$STARCI_RUN"`)
}

// refusal says why a step cannot be compiled, if it cannot.
func refusal(s plan.Step) error {
	if len(s.Keep) > 0 || len(s.Artifacts) > 0 {
		return fmt.Errorf("step %q: artifacts are not compiled to a workflow yet; run the script with starci run, or a compiled executable", s.Name)
	}
	return bareRefusal(s)
}

// bareRefusal says why a bare step cannot be compiled, if it cannot.
func bareRefusal(s plan.Step) error {
	if s.Bare() && s.Offline {
		return fmt.Errorf("step %q: a bare step without the network cannot be kept off it by a workflow; give it an image, or the network", s.Name)
	}
	return nil
}
