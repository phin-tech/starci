package step

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// toolsRoot is where tools appear inside a step's container.
const toolsRoot = "/opt/starci/tools"

// image is what Run needs to know about the image a step runs in.
type image struct {
	// id is the content id of the image that will actually run. A tag can be
	// re-pointed; this cannot.
	id string
	// arch is what the image runs as, which is what a tool must be built
	// for. It is not necessarily the architecture of this machine.
	arch string
	// path is the image's own PATH, which tools are put in front of.
	path string
}

// inspectImage describes an image, pulling it first if it is not here.
func inspectImage(name string) (image, error) {
	inspect := func() (image, error) {
		out, err := exec.Command("docker", "image", "inspect", "--format",
			`{{.Id}}|{{.Architecture}}|{{json .Config.Env}}`, name).Output()
		if err != nil {
			return image{}, err
		}
		parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
		if len(parts) != 3 {
			return image{}, fmt.Errorf("unexpected description of image %s", name)
		}
		img := image{id: parts[0], arch: parts[1], path: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
		var env []string
		if json.Unmarshal([]byte(parts[2]), &env) == nil {
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, "PATH="); ok {
					img.path = v
				}
			}
		}
		return img, nil
	}
	if img, err := inspect(); err == nil {
		return img, nil
	}
	if out, err := exec.Command("docker", "pull", "-q", name).CombinedOutput(); err != nil {
		return image{}, fmt.Errorf("cannot pull image %s: %s", name, strings.TrimSpace(string(out)))
	}
	return inspect()
}

// installed is a tool present on this machine, ready to mount.
type installed struct {
	name string
	// sum is the checksum of the download, which is the tool's identity.
	sum string
	// dir is where it is unpacked on this machine.
	dir string
	// bins are the directories inside it to put on PATH.
	bins []string
}

// install makes a tool available on this machine for the given architecture,
// downloading and unpacking it unless that has been done before.
//
// A tool lives in a directory named after its checksum and is never changed
// once there, so every step that asks for it can mount the same copy
// read-only, and a different build of the "same" tool can never be mistaken
// for it.
func install(stateDir string, t plan.Tool, arch string) (installed, error) {
	sum, ok := t.SHA256[arch]
	if !ok {
		return installed{}, fmt.Errorf("tool %q has no build for %s, which is what the step's image runs on", t.Name, arch)
	}
	vendorArch := arch
	if name, ok := t.ArchNames[arch]; ok {
		vendorArch = name
	}
	fill := func(s string) string { return strings.ReplaceAll(s, "{arch}", vendorArch) }

	in := installed{name: t.Name, sum: sum, dir: filepath.Join(stateDir, "tools", sum)}
	for _, b := range t.Bin {
		in.bins = append(in.bins, path.Clean(fill(b)))
	}
	if _, err := os.Stat(in.dir); err == nil {
		return in, nil
	}

	parent := filepath.Dir(in.dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return installed{}, err
	}
	work, err := os.MkdirTemp(parent, ".installing-")
	if err != nil {
		return installed{}, err
	}
	defer os.RemoveAll(work)

	url := fill(t.URL)
	download := filepath.Join(work, "download")
	got, err := fetchFile(url, download)
	if err != nil {
		return installed{}, fmt.Errorf("tool %q: %v", t.Name, err)
	}
	if got != sum {
		return installed{}, fmt.Errorf("tool %q: %s has checksum %s, not the %s the script pins it to", t.Name, url, got, sum)
	}

	unpacked := filepath.Join(work, "unpacked")
	if err := os.Mkdir(unpacked, 0o755); err != nil {
		return installed{}, err
	}
	if err := unpack(download, url, unpacked, t.Name); err != nil {
		return installed{}, fmt.Errorf("tool %q: %v", t.Name, err)
	}
	for _, b := range in.bins {
		if info, err := os.Stat(filepath.Join(unpacked, b)); err != nil || !info.IsDir() {
			return installed{}, fmt.Errorf("tool %q: there is no directory %q in the download", t.Name, b)
		}
	}
	// Unpacked beside its final place and moved in one step, so a half
	// unpacked tool is never visible. If another step got there first, its
	// copy is identical; keep it.
	if err := os.Rename(unpacked, in.dir); err != nil {
		if _, statErr := os.Stat(in.dir); statErr != nil {
			return installed{}, err
		}
	}
	return in, nil
}

func fetchFile(url, dest string) (sum string, err error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("cannot download %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cannot download %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", fmt.Errorf("cannot download %s: %v", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unpack lays a download out in dir: a zip or tar archive is extracted, and
// anything else is taken to be the program itself, saved under the tool's
// name.
func unpack(file, url, dir, name string) error {
	lower := strings.ToLower(strings.SplitN(url, "?", 2)[0])
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return unzip(file, dir)
	case strings.Contains(lower, ".tar.") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar"):
		if out, err := exec.Command("tar", "-xf", file, "-C", dir).CombinedOutput(); err != nil {
			return fmt.Errorf("cannot unpack: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), data, 0o755)
}

func unzip(file, dir string) error {
	r, err := zip.OpenReader(file)
	if err != nil {
		return fmt.Errorf("cannot unpack: %v", err)
	}
	defer r.Close()
	for _, f := range r.File {
		// An entry may not name a place outside the directory it is unpacked into.
		dest := filepath.Join(dir, f.Name)
		if rel, err := filepath.Rel(dir, dest); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("cannot unpack: entry %q escapes the archive", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm()|0o200)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(out, src)
		src.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// toolArgs returns the docker arguments that put tools on a step's PATH, in
// front of basePath, and what identifies them for the step's key.
func toolArgs(tools []installed, basePath string) (args []string, identity [][]string) {
	var dirs []string
	for _, t := range tools {
		mount := toolsRoot + "/" + t.name
		args = append(args, "-v", t.dir+":"+mount+":ro")
		for _, b := range t.bins {
			dirs = append(dirs, path.Join(mount, b))
		}
		identity = append(identity, append([]string{t.name, t.sum}, t.bins...))
	}
	if len(dirs) > 0 {
		args = append(args, "-e", "PATH="+strings.Join(dirs, ":")+":"+basePath)
	}
	return args, identity
}
