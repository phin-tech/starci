package compile

import (
	"fmt"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// toolInstall is the shell that puts one of a step's tools in dir, the way a
// machine does: download it, check it against the checksum the script pinned,
// and unpack it. sum is the checksum for the architecture, arch the name the
// tool's URL uses for it, and download where the file lands first. dir and
// download are shell, so they may name variables.
func toolInstall(t plan.Tool, sum, arch, download, dir string) []string {
	fill := func(v string) string { return strings.ReplaceAll(v, "{arch}", arch) }
	return []string{
		fmt.Sprintf("mkdir -p \"%s\"", dir),
		fmt.Sprintf("curl -fsSL %s -o \"%s\"", shq(fill(t.URL)), download),
		fmt.Sprintf("echo \"%s  %s\" | sha256sum -c -", sum, download),
		unpackCommand(fill(t.URL), download, dir, t.Name),
	}
}
