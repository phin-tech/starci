// Command starci evaluates a repository's Starlark CI script.
//
//	starci plan [--file .ci.star] [--repo owner/name] [--sha SHA] [--ref REF] [--changed a,b,c]
//
// It prints the plan as JSON. Nothing is executed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "plan" {
		fmt.Fprintln(os.Stderr, "usage: starci plan [--file .ci.star] [--repo owner/name] [--sha SHA] [--ref REF] [--changed a,b,c]")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	file := fs.String("file", ".ci.star", "CI script to evaluate")
	repo := fs.String("repo", "", "repository, as owner/name")
	sha := fs.String("sha", "", "commit being built")
	ref := fs.String("ref", "", "git ref being built")
	changed := fs.String("changed", "", "comma-separated files changed by the commit")
	fs.Parse(os.Args[2:])

	src, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starci:", err)
		os.Exit(1)
	}

	c := plan.Context{Repo: *repo, SHA: *sha, Ref: *ref}
	for _, f := range strings.Split(*changed, ",") {
		if f = strings.TrimSpace(f); f != "" {
			c.ChangedFiles = append(c.ChangedFiles, f)
		}
	}

	p, err := plan.Eval(*file, src, c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starci:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // commands contain && and <, which are not HTML here
	enc.Encode(p)
}
