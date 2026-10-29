package compile

import (
	"fmt"
	"path"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// CircleCI renders a plan as a CircleCI configuration.
//
// As with GitHub, each step becomes a job that does what a starci machine
// does. Two things come out closer to starci here than they do there. The
// record of a pass is keyed on the exact list of files the inputs select,
// asked of git with the same pathspecs a machine uses, rather than on globs.
// And a job that has already passed ends in one place, instead of every later
// step carrying a condition.
//
// Jobs use the machine executor, because the step's container is started by
// the job and is given the checkout as a mount, which the Docker executor
// cannot do.
func CircleCI(p *plan.Plan, o Options) (string, error) {
	if o.Arch == "" {
		o.Arch = "amd64"
	}
	if o.RunsOn == "" {
		o.RunsOn = "medium"
		if o.Arch == "arm64" {
			o.RunsOn = "arm.medium"
		}
	}
	if err := p.Validate(); err != nil {
		return "", err
	}

	ids := jobIDs(p.Steps)
	var w strings.Builder

	var placed []string
	for _, s := range p.Steps {
		if len(s.On) > 0 || len(s.Prefer) > 0 || len(s.Avoid) > 0 {
			placed = append(placed, s.Name)
		}
	}
	notes := commitNote(o)
	if len(placed) > 0 {
		notes = append(notes, fmt.Sprintf("`on`, `prefer` and `avoid` have no equivalent here and are dropped (%s): CircleCI chooses machines by resource class, not by label. Every job uses %s.", strings.Join(placed, ", "), o.RunsOn))
	}
	notes = append(notes, "Passes are remembered in this project's CircleCI cache, which expires. starci's own store does not.")
	writeHeader(&w, "starci compile --target circleci", o, notes)

	w.WriteString("version: 2.1\n\njobs:\n")
	for i, s := range p.Steps {
		if i > 0 {
			w.WriteString("\n")
		}
		if err := circleJob(&w, s, ids[s.Name], o); err != nil {
			return "", err
		}
	}

	w.WriteString("\nworkflows:\n  ci:\n    jobs:\n")
	for _, s := range p.Steps {
		if len(s.Needs) == 0 {
			fmt.Fprintf(&w, "      - %s\n", ids[s.Name])
			continue
		}
		var needs []string
		for _, n := range s.Needs {
			needs = append(needs, ids[n])
		}
		fmt.Fprintf(&w, "      - %s:\n          requires: [%s]\n", ids[s.Name], strings.Join(needs, ", "))
	}
	return w.String(), nil
}

// circleEscape protects text from CircleCI's own templating: in a 2.1
// configuration "<<" begins a parameter, anywhere, including inside a command.
func circleEscape(s string) string {
	return strings.ReplaceAll(s, "<<", `\<<`)
}

// circleRun writes one run step. lines are shell, one command per entry.
func circleRun(w *strings.Builder, name string, env map[string]string, lines ...string) {
	fmt.Fprintf(w, "      - run:\n          name: %s\n", q(name))
	if len(env) > 0 {
		w.WriteString("          environment:\n")
		for _, k := range sortedKeys(env) {
			fmt.Fprintf(w, "            %s: %s\n", k, block(circleEscape(env[k]), "              "))
		}
	}
	w.WriteString("          command: |\n")
	for _, l := range lines {
		for _, part := range strings.Split(circleEscape(l), "\n") {
			fmt.Fprintf(w, "            %s\n", part)
		}
	}
}

// gitPathspec selects an input the way a starci machine does.
func gitPathspec(in string) string {
	switch {
	case in == ".":
		return ":(top)"
	case isPattern(in):
		return ":(top,glob)" + in
	}
	return ":(top,literal)" + in
}

func circleJob(w *strings.Builder, s plan.Step, id string, o Options) error {
	if err := refusal(s); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %s:\n    machine:\n      image: ubuntu-2404:current\n    resource_class: %s\n    steps:\n      - checkout\n", id, o.RunsOn)

	// Taken out of the checkout while the whole of it is still there.
	given := "\"$HOME/starci-services\""
	if lines := copyServiceFiles(s, given); len(lines) > 0 {
		circleRun(w, "Files for the services", nil, lines...)
	}

	whole := len(s.Inputs) == 1 && s.Inputs[0] == "."
	if !whole {
		var patterns []string
		for _, in := range s.Inputs {
			patterns = append(patterns, shq(sparsePattern(in)))
		}
		// Everything the step did not ask for is removed from the checkout,
		// which is as close as a job gets to a step that cannot see it.
		circleRun(w, "Keep only this step's inputs", nil, "git sparse-checkout set --no-cone "+strings.Join(patterns, " "))
	}

	passKey := ""
	if s.Cache {
		var specs []string
		for _, in := range append(append([]string{}, s.Inputs...), serviceFiles(s)...) {
			specs = append(specs, shq(gitPathspec(in)))
		}
		// The same question a machine asks git: which files, with which
		// contents, do these inputs select at this commit?
		circleRun(w, "Identify the inputs", nil, "git ls-files -s -- "+strings.Join(specs, " ")+" > /tmp/starci-inputs")
		passKey = fmt.Sprintf("starci-passed-%s-%s-{{ checksum \"/tmp/starci-inputs\" }}", id, stepKey(s, o.Arch))
		fmt.Fprintf(w, "      - restore_cache:\n          name: Has this already passed with these inputs?\n          keys:\n            - %s\n", passKey)
		circleRun(w, "Stop here if it has", nil,
			"if [ -e ~/starci-passed/at ]; then",
			"  echo \"Passed before with the same inputs, on $(cat ~/starci-passed/at). Nothing to do.\"",
			"  circleci-agent step halt",
			"fi")
	}

	var mounts, pathDirs, hostDirs []string
	for _, t := range s.Tools {
		sum, ok := t.Sum("linux/" + o.Arch)
		if !ok {
			return fmt.Errorf("step %q: tool %q has no build for %s, which is what %s machines are", s.Name, t.Name, o.Arch, o.RunsOn)
		}
		arch := o.Arch
		if name, ok := t.ArchNames[o.Arch]; ok {
			arch = name
		}
		fill := func(v string) string { return strings.ReplaceAll(v, "{arch}", arch) }
		dir := "$HOME/starci-tools/" + t.Name
		download := "/tmp/" + t.Name + ".download"
		circleRun(w, "Tool: "+t.Name, nil,
			fmt.Sprintf("mkdir -p \"%s\"", dir),
			fmt.Sprintf("curl -fsSL %s -o %s", shq(fill(t.URL)), download),
			fmt.Sprintf("echo \"%s  %s\" | sha256sum -c -", sum, download),
			unpackCommand(fill(t.URL), download, dir, t.Name))
		mounts = append(mounts, fmt.Sprintf("-v \"%s:/opt/starci/tools/%s:ro\"", dir, t.Name))
		for _, b := range t.Bin {
			pathDirs = append(pathDirs, path.Join("/opt/starci/tools", t.Name, fill(b)))
			hostDirs = append(hostDirs, path.Join(dir, fill(b)))
		}
	}

	image := s.Image
	if len(s.Packages) > 0 {
		image = "starci-env-" + id
		circleRun(w, "Image with "+strings.Join(s.Packages, ", "), nil,
			fmt.Sprintf("docker build -q -t %s - <<'DOCKERFILE'", image),
			"FROM "+s.Image,
			fmt.Sprintf("RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*", strings.Join(s.Packages, " ")),
			"DOCKERFILE")
	}

	cacheDirs := map[string]string{}
	for _, name := range sortedKeys(s.Caches) {
		// The newest cache whose key starts this way.
		fmt.Fprintf(w, "      - restore_cache:\n          name: %s\n          keys:\n            - starci-cache-%s-\n", q("Cache: "+name), name)
		mounts = append(mounts, fmt.Sprintf("-v \"$HOME/starci-cache/%s:%s\"", name, s.Caches[name]))
		cacheDirs[s.Caches[name]] = "$HOME/starci-cache/" + name
	}

	// CIRCLE_BUILD_NUM is this job's own number.
	network := "starci-$CIRCLE_BUILD_NUM-" + id
	if len(s.Services) > 0 {
		circleRun(w, "Services: "+strings.Join(serviceNames(s), ", "), nil, startServices(s, network, given)...)
	}

	var lines []string
	if s.Bare() {
		// On the machine itself, which is what a bare step asks for.
		lines = bareRun(s, hostDirs, cacheDirs)
	}
	args := []string{"-v \"$PWD:/workspace\"", "-w /workspace"}
	switch {
	case len(s.Services) > 0:
		args = append(args, fmt.Sprintf("--network \"%s\"", network))
	case s.Offline:
		args = append(args, "--network none")
	}
	args = append(args, mounts...)
	if len(pathDirs) > 0 && !s.Bare() {
		base := "$image_path"
		if p, ok := s.Env["PATH"]; ok {
			base = p
		} else {
			if len(s.Packages) == 0 {
				lines = append(lines, "docker pull -q "+image)
			}
			// Tools go in front of the image's own PATH, which has to be asked for.
			lines = append(lines, fmt.Sprintf("image_path=$(docker image inspect %s --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^PATH=//p')", image))
		}
		args = append(args, fmt.Sprintf("-e \"PATH=%s:%s\"", strings.Join(pathDirs, ":"), base))
	}
	for _, k := range sortedKeys(s.Env) {
		if k == "PATH" && len(pathDirs) > 0 {
			continue
		}
		args = append(args, "-e "+shq(k+"="+s.Env[k]))
	}
	if !s.Bare() {
		run := "docker run --rm \\\n"
		for _, a := range args {
			run += "  " + a + " \\\n"
		}
		run += "  " + image + " sh -c \"$STARCI_RUN\""
		lines = append(lines, run)
	}
	// The command travels in an environment variable, so that nothing in it
	// needs escaping for the shell that starts the container.
	circleRun(w, s.Name, map[string]string{"STARCI_RUN": s.Command()}, lines...)
	if len(s.Services) > 0 {
		// Whatever became of the step. The machine is thrown away after the
		// job, so this is tidiness, and the same as the other targets.
		w.WriteString("      - run:\n          name: Stop the services\n          when: always\n          command: |\n")
		for _, l := range stopServices(s, network) {
			fmt.Fprintf(w, "            %s\n", circleEscape(l))
		}
	}

	for _, name := range sortedKeys(s.Caches) {
		fmt.Fprintf(w, "      - save_cache:\n          name: %s\n          key: starci-cache-%s-{{ .Revision }}\n          paths:\n            - ~/starci-cache/%s\n", q("Save cache: "+name), name, name)
	}
	if s.Cache {
		circleRun(w, "Remember this pass", nil, "mkdir -p ~/starci-passed && date > ~/starci-passed/at")
		fmt.Fprintf(w, "      - save_cache:\n          key: %s\n          paths:\n            - ~/starci-passed\n", passKey)
	}
	return nil
}
