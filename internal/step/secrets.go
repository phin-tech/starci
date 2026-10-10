package step

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Secrets is where a machine gets the secrets steps ask for.
//
// A script only ever names a secret. What the name stands for is the
// business of the machine that runs the step, so a source is anything that
// can turn a name into a value: a directory of files, a program that asks a
// vault or a cloud provider's secret manager.
type Secrets interface {
	// Secret returns the value kept under name, or ErrNoSecret if there is
	// none by that name.
	Secret(ctx context.Context, name string) (string, error)
}

// ErrNoSecret is what a source returns for a name it has nothing under.
var ErrNoSecret = errors.New("no such secret")

// OpenSecrets returns the source described by location, as STARCI_SECRETS
// gives it:
//
//	/run/secrets/steps          a directory holding one file per secret
//	exec:/usr/local/bin/secret  a program run with the name, printing the value
func OpenSecrets(location string) (Secrets, error) {
	if program, ok := strings.CutPrefix(location, "exec:"); ok {
		path, err := exec.LookPath(program)
		if err != nil {
			return nil, fmt.Errorf("the program that fetches secrets, %q, cannot be run: %v", program, err)
		}
		return ExecSecrets{Program: path}, nil
	}
	info, err := os.Stat(location)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory of secrets, nor exec: and a program that fetches them", location)
	}
	return DirSecrets{Dir: location}, nil
}

// DirSecrets keeps each secret in a file named after it.
type DirSecrets struct{ Dir string }

func (d DirSecrets) Secret(_ context.Context, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(d.Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoSecret
	}
	if err != nil {
		return "", err
	}
	return trimNewline(string(data)), nil
}

// ExecSecrets asks a program, which is how a machine fetches secrets from
// wherever they are really kept without starci knowing where that is. The
// program is run with the secret's name as its one argument and prints the
// value. Exit status 3 means there is no secret by that name; any other
// failure is a failure to find out.
type ExecSecrets struct{ Program string }

// How long a program may take to fetch one secret.
const secretTimeout = 30 * time.Second

func (e ExecSecrets) Secret(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, secretTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Program, name)
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return "", ErrNoSecret
		}
		// What it wrote to its error stream, and never what it printed: a
		// program that failed halfway may have printed half a secret.
		said := strings.TrimSpace(complaint.String())
		if i := strings.IndexByte(said, '\n'); i >= 0 {
			said = said[:i]
		}
		if said == "" {
			said = err.Error()
		}
		return "", errors.New(said)
	}
	return trimNewline(out.String()), nil
}

// trimNewline drops the one line ending a file or a program's output ends
// with, which is not part of the secret.
func trimNewline(s string) string {
	return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
}

// resolved is the secrets a step is to be given, by name.
type resolved map[string]string

// resolve fetches each named secret from source. A step is not started with
// some of what it asked for: any that cannot be had is the step's failure,
// and the message names it and never a value.
func resolve(ctx context.Context, source Secrets, names []string) (resolved, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if source == nil {
		return nil, fmt.Errorf("it asks for secrets (%s), and this machine has none: STARCI_SECRETS says where a machine's secrets are", strings.Join(names, ", "))
	}
	values := resolved{}
	for _, name := range names {
		value, err := source.Secret(ctx, name)
		switch {
		case errors.Is(err, ErrNoSecret):
			return nil, fmt.Errorf("it asks for the secret %s, and this machine has none by that name", name)
		case err != nil:
			return nil, fmt.Errorf("the secret %s could not be fetched: %v", name, err)
		case value == "":
			return nil, fmt.Errorf("the secret %s is empty", name)
		}
		values[name] = value
	}
	return values, nil
}

// environ is the secrets as NAME=value, for the environment of the program
// that starts the container.
func (r resolved) environ() []string {
	var env []string
	for name, value := range r {
		env = append(env, name+"="+value)
	}
	return env
}

// masked is what stands where a secret was.
const masked = "***"

// minMaskedLine is how long a line of a secret of several lines has to be
// before it is looked for on its own.
const minMaskedLine = 6

// masker returns a function that removes every secret from a line of output.
//
// It is the last line of defence and not a strong one. It finds a value
// written as it is; it cannot find one that a step has encoded, reversed or
// printed a character at a time. A step that is given a secret is trusted
// with it.
func (r resolved) masker() func(string) string {
	seen := map[string]bool{}
	var needles []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			needles = append(needles, s)
		}
	}
	for _, value := range r {
		// Output is masked a line at a time, so a secret of several lines,
		// such as a key, is looked for a line at a time.
		lines := strings.Split(value, "\n")
		for _, line := range lines {
			line = strings.TrimSuffix(line, "\r")
			// A line of a few characters in a longer secret, a brace or a
			// blank, is not the secret, and masking it wherever it turned
			// up would leave little of the output standing.
			if len(lines) > 1 && len(strings.TrimSpace(line)) < minMaskedLine {
				continue
			}
			add(line)
		}
	}
	if len(needles) == 0 {
		return func(line string) string { return line }
	}
	// Longest first, so that a secret which contains another is replaced
	// whole and not left with its ends showing.
	sort.Slice(needles, func(i, j int) bool {
		if len(needles[i]) != len(needles[j]) {
			return len(needles[i]) > len(needles[j])
		}
		return needles[i] < needles[j]
	})
	var pairs []string
	for _, n := range needles {
		pairs = append(pairs, n, masked)
	}
	return strings.NewReplacer(pairs...).Replace
}
