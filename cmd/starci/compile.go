package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"git.phin.tech/sam/starci/internal/compile"
	"git.phin.tech/sam/starci/internal/plan"
)

// A compile target turns a script into text: what it writes, by file name.
// Most write one file, which is the entry named "" (standard output, or the
// file --check compares); a target that writes several gives their names, and
// is written into the directory -o names, or compared with the one --check
// names. An executable is not text and has its own path (compileBinary).
type target func(file string, src []byte, o compile.Options) (map[string]string, error)

var targets = map[string]target{
	"github": func(file string, src []byte, o compile.Options) (map[string]string, error) {
		// Asked once for each kind of event, so that what the script does
		// for a tag or a pull request or a schedule is in the workflow too.
		out, err := compile.GitHubEvents(file, src, o)
		return map[string]string{"": out}, err
	},
	"circleci": func(file string, src []byte, o compile.Options) (map[string]string, error) {
		// Compiled for a push about which nothing else is known.
		p, read, err := plan.EvalTracked(file, src, plan.Context{})
		if err != nil {
			return nil, err
		}
		if len(p.Steps) == 0 {
			return nil, fmt.Errorf("%s plans no steps for a push about which nothing is known; there is nothing to compile", file)
		}
		o.Read = read
		out, err := compile.CircleCI(p, o)
		return map[string]string{"": out}, err
	},
	"argo": func(file string, src []byte, o compile.Options) (map[string]string, error) {
		if mode, err := plan.Mode(file, src); err != nil {
			return nil, err
		} else if mode == plan.Advanced {
			return nil, fmt.Errorf("%s is an advanced script: its steps are only known as it runs, so it cannot be written out as a workflow. Something that asks `starci next` can carry it out (docs/driving.md)", file)
		}
		out, err := compile.Argo(file, src, o)
		return map[string]string{"": out}, err
	},
	"temporal": func(file string, src []byte, o compile.Options) (map[string]string, error) {
		if mode, err := plan.Mode(file, src); err != nil {
			return nil, err
		} else if mode == plan.Advanced {
			return nil, fmt.Errorf("%s is an advanced script: its steps are only known as it runs, so it cannot be written out as a workflow. Something that asks `starci next` can carry it out (docs/driving.md)", file)
		}
		return compile.Temporal(file, src, o)
	},
}

// compileCmd writes a basic script out as another CI system's configuration,
// for a repository hosted where starci is not running:
//
//	starci compile > .github/workflows/ci.yml
//	starci compile --target circleci > .circleci/config.yml
//	starci compile --target argo --init-image IMAGE > ci.yaml
//	starci compile --target temporal -o ci-workflow
//
// Forgejo and Gitea Actions read the GitHub file.
func compileCmd(args []string) error {
	fs := flag.NewFlagSet("compile", flag.ExitOnError)
	file := fs.String("file", ".ci.star", "CI script to compile")
	targetName := fs.String("target", "github", "what to write: github (also read by Forgejo and Gitea), circleci, argo, temporal or binary")
	runsOn := fs.String("runs-on", "", "the machines to run on: a runner label for github (default ubuntu-latest), a resource class for circleci (default medium)")
	arch := fs.String("arch", "amd64", "architecture of those machines: amd64 or arm64")
	defaultBranch := fs.String("default-branch", "main", "the repository's default branch")
	offline := fs.Bool("offline", false, "do not download the script's tools to check them first")
	check := fs.String("check", "", "write nothing: fail unless this file (or directory, for a target that writes several) is what would be written")
	initImage := fs.String("init-image", "", "with --target argo, the image of the container that puts a step's inputs and tools in place (git, curl, tar, unzip); no default")
	bareImage := fs.String("bare-image", "", "with --target argo, the image a step without one runs in")
	name := fs.String("name", "", "with --target argo, the WorkflowTemplate's name (default starci)")
	output := fs.String("o", "", "with --target binary, where to write the executable; with --target temporal, the directory to write the module into")
	freeze := fs.Bool("freeze", false, "with --target binary, evaluate the script now and carry the plan, not the script (basic scripts only)")
	base := fs.String("base", "", "with --target binary, the starci executable to copy (default: this one); use another to build for another OS or architecture")
	fs.Parse(args)

	if *targetName == "binary" {
		return compileBinary(*file, *output, *base, *freeze, *offline, *arch, *check)
	}
	build, ok := targets[*targetName]
	if !ok {
		return fmt.Errorf("unknown target %q: github, circleci, argo, temporal or binary", *targetName)
	}

	src, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	opts := compile.Options{Source: filepath.Base(*file), RunsOn: *runsOn, Arch: *arch, DefaultBranch: *defaultBranch, Name: *name, InitImage: *initImage, BareImage: *bareImage}
	files, err := build(*file, src, opts)
	if err != nil {
		return err
	}
	_, single := files[""]
	switch {
	case !single && *output == "" && *check == "":
		return fmt.Errorf("--target %s writes several files and needs -o, the directory to write them into", *targetName)
	case !single && *output != "" && *check != "":
		return errors.New("--check writes nothing: give it the directory to compare, and no -o")
	}

	// A compiled file is a copy, and a copy goes stale the moment the script
	// is edited without it. This is the question to ask on every push; it
	// needs no network, so it downloads nothing.
	if *check != "" {
		again := "starci compile"
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "check" && f.Name != "offline" {
				again += " --" + f.Name + " " + f.Value.String()
			}
		})
		if !single {
			again = fmt.Sprintf("starci compile --target %s -o %s", *targetName, *check)
		}
		for _, name := range sortedFiles(files) {
			at := *check
			if !single {
				at = filepath.Join(*check, name)
			}
			have, err := os.ReadFile(at)
			if err != nil {
				return fmt.Errorf("%w\nwrite it again with: %s", err, again)
			}
			if string(have) != files[name] {
				redo := again + " > " + at
				if !single {
					redo = again
				}
				return fmt.Errorf("%s is not what %s compiles to; %s\nwrite it again with: %s", at, *file, firstDifference(string(have), files[name]), redo)
			}
		}
		fmt.Printf("%s is what %s compiles to\n", *check, *file)
		return nil
	}

	// What is written is run later, somewhere else. A tool that cannot be
	// fetched is better found here than in every job that wants it.
	if !*offline {
		if err := checkDeclaredTools(*file, src, *arch); err != nil {
			return err
		}
	}
	if single {
		fmt.Print(files[""])
		return nil
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		return err
	}
	for _, name := range sortedFiles(files) {
		if err := os.WriteFile(filepath.Join(*output, name), []byte(files[name]), 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "wrote %d files to %s\n", len(files), *output)
	if *targetName == "temporal" {
		fmt.Fprintf(os.Stderr, "run `go mod tidy` there, then `go run .` on each machine (see the head of plan.go)\n")
	}
	return nil
}

func sortedFiles(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// checkDeclaredTools downloads every tool the script declares that has a build
// for arch and checks it, so that a pin that no longer holds is found when
// compiling and not in every job. One with no build for these machines is left
// to the compiler, which refuses a step that uses it.
func checkDeclaredTools(file string, src []byte, arch string) error {
	declared, err := plan.Tools(file, src)
	if err != nil {
		return err
	}
	var tools []plan.Tool
	for _, t := range declared {
		if _, ok := t.SHA256[arch]; ok {
			tools = append(tools, t)
		}
	}
	if failed := checkTools(os.Stderr, tools, []string{arch}); failed > 0 {
		return fmt.Errorf("%d tool download(s) failed the check; nothing was written (--offline skips the check)", failed)
	}
	return nil
}
