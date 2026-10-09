package step

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// building serialises builds of the same environment within this process, so
// that several steps wanting the same packages build it once and share it.
var building sync.Map // tag -> *sync.Mutex

// withPackages returns the name of an image that is base plus the given apt
// packages, building it on this machine if it is not here already.
//
// The image is named after the base image's content and the package list, so
// it is built once per machine and then reused by every step that asks for
// the same combination. It is not rebuilt when a newer version of a package
// appears: an environment, once made, stays as it was.
func withPackages(base string, baseID string, packages []string) (string, error) {
	sum := sha256.Sum256([]byte(baseID + "\n" + strings.Join(packages, "\n")))
	tag := "starci-env:" + hex.EncodeToString(sum[:])[:32]

	lock, _ := building.LoadOrStore(tag, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	if exec.Command("docker", "image", "inspect", tag).Run() == nil {
		return tag, nil
	}

	// The check for apt-get comes first so that an image without it fails
	// with a sentence, not with "apt-get: not found" somewhere in a build log.
	dockerfile := fmt.Sprintf(`FROM %s
RUN if ! command -v apt-get >/dev/null 2>&1; then echo "STARCI: no apt-get in this image" >&2; exit 97; fi
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends %s \
 && rm -rf /var/lib/apt/lists/*
`, base, strings.Join(packages, " "))

	cmd := exec.Command("docker", "build", "-q", "-t", tag, "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := string(out)
		if strings.Contains(text, "STARCI: no apt-get in this image") {
			return "", fmt.Errorf("packages need a Debian or Ubuntu based image; %s has no apt-get", base)
		}
		lines := strings.Split(strings.TrimSpace(text), "\n")
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		return "", fmt.Errorf("installing packages %s on %s failed:\n%s", strings.Join(packages, " "), base, strings.Join(lines, "\n"))
	}
	return tag, nil
}
