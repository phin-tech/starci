package step

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// A step run in a pod (the Argo target) is not run by Run, so it cannot be
// keyed the way Run keys it: there is no Docker to give the image's content id.
// What stands for it is the image's name, as the compiled GitHub and CircleCI
// workflows do, so a tag that moves does not run a step again. The two keys
// are not the same and never meet: this one says "pod" in its material.

// InputsID identifies the files a step's inputs select in the checkout in dir,
// by path, mode and blob id, asked of git with the pathspecs a machine uses.
// It is contentID for a checkout that is not a machine's own.
func InputsID(dir string, inputs []string) (string, error) {
	specs := make([]string, len(inputs))
	for i, in := range inputs {
		out, err := exec.Command("git", "-C", dir, "ls-files", "--", pathspec(in)).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("input %q: %s", in, strings.TrimSpace(string(out)))
		}
		if len(out) == 0 {
			return "", fmt.Errorf("input %q matches nothing in this commit", in)
		}
		specs[i] = pathspec(in)
	}
	out, err := exec.Command("git", append([]string{"-C", dir, "ls-files", "-s", "--"}, specs...)...).Output()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:]), nil
}

// PodKey is the key a pass of s is stored under when s is run in a pod: its
// image by name, its command, env, tools and services, the architecture of
// the nodes, and content, which InputsID gives.
func PodKey(s plan.Step, arch, content string) string {
	env := [][2]string{}
	for _, k := range sortedKeys(s.Env) {
		env = append(env, [2]string{k, s.Env[k]})
	}
	tools := [][]string{}
	for _, t := range s.Tools {
		sum, _ := t.Sum("linux/" + arch)
		tools = append(tools, []string{t.Name, sum})
	}
	parts := []any{keyVersion, "pod", arch, s.Image, s.Run, env, content, tools}
	if len(s.Argv) > 0 {
		parts = append(parts, "argv", s.Argv)
	}
	if len(s.Services) > 0 {
		parts = append(parts, "services", s.Services)
	}
	material, _ := json.Marshal(parts)
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

// Results is the store of passes this machine uses: its own directory, and
// the shared store if there is one.
func (c Config) Results() Store { return c.store() }
