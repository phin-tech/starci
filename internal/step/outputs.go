package step

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// maxOutput is how large one output may be. Outputs are for what a build
// produces, and something has to stop a step that writes without end from
// filling the store.
const maxOutput = 2 << 30

// An output is kept as one archive, named by the checksum of the archive.
// Two things follow from writing that archive the same way every time, with
// entries in order and nothing in them that varies from run to run. A step
// that produces the same files twice produces the same digest, so the steps
// given them are not run again. And a digest in a stored result says exactly
// what a step given it will see.
//
// What a step leaves behind was written by the step, and is not trusted:
// packing refuses anything that is not a plain file, a directory or a link
// that stays inside the output, and unpacking refuses the same again, since
// what comes back from a store is trusted no further.

// packOutput writes the output at rel, a path inside the workspace that root
// is opened on, to the file at dest, and returns its digest.
//
// The worker reads the output from its own side of the workspace, where a
// link means what it means on the worker and not what it meant in the step's
// container. So everything is read through root, which will not follow a
// path out of the workspace, and a link anywhere on the way to the output is
// refused: `ln -s /run/secrets out` would otherwise make an output of
// whatever the worker can read.
func packOutput(root *os.Root, rel, dest string) (digest string, err error) {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		info, err := root.Lstat(filepath.FromSlash(dir))
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a link, and an output cannot be reached through one", dir)
		}
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	sum := sha256.New()
	limited := &capped{w: io.MultiWriter(out, sum), left: maxOutput}
	tw := tar.NewWriter(limited)

	// Entries are written in name order and links are recorded, never
	// followed, which are the two things the archive's being repeatable and
	// safe depend on.
	var add func(name string) error
	add = func(name string) error {
		local := filepath.FromSlash(name)
		info, err := root.Lstat(local)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		switch {
		case info.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			f, err := root.Open(local)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		case info.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			dir, err := root.Open(local)
			if err != nil {
				return err
			}
			entries, err := dir.ReadDir(-1)
			dir.Close()
			if err != nil {
				return err
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				if err := add(name + "/" + e.Name()); err != nil {
					return err
				}
			}
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := root.Readlink(local)
			if err != nil {
				return err
			}
			if !linkStaysWithin(rel, name, target) {
				return fmt.Errorf("%s is a link to %s, which is outside the output", name, target)
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, target
			return tw.WriteHeader(h)
		}
		return fmt.Errorf("%s is neither a file, a directory nor a link", name)
	}
	err = add(rel)
	if err == nil {
		err = tw.Close()
	}
	if errors.Is(err, errTooLarge) {
		return "", fmt.Errorf("it is larger than the %d GB an output may be", maxOutput>>30)
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// unpackOutput puts the output archived at src, which was produced at rel, into
// workspace at the same path, replacing whatever is there. The archive must
// have the digest it was asked for by.
func unpackOutput(src, digest, workspace, rel string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != digest {
		return fmt.Errorf("what the store holds has checksum %.12s, not the %.12s it was kept under", got, digest)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// Everything is created through a root, which refuses any path that
	// leaves the workspace, by a link or otherwise, whatever the checks
	// below let through.
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	// An output stands in for whatever the repository has at its path. Left
	// in place, a file the repository has and the output does not would make
	// what a step sees depend on its other inputs.
	if err := root.RemoveAll(filepath.FromSlash(rel)); err != nil {
		return err
	}
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return err
		}
	}

	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if name != rel && !strings.HasPrefix(name, rel+"/") {
			return fmt.Errorf("the archive holds %s, which is not part of %s", h.Name, rel)
		}
		local := filepath.FromSlash(name)
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			// Writable by its owner while it is being filled, whatever it
			// was; a directory nobody can write to cannot be given files.
			if err := root.Mkdir(local, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			out, err := root.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			// The mode asked for at creation is subject to the umask.
			if err := root.Chmod(local, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !linkStaysWithin(rel, name, h.Linkname) {
				return fmt.Errorf("%s is a link to %s, which is outside the output", name, h.Linkname)
			}
			if err := root.Symlink(h.Linkname, local); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is neither a file, a directory nor a link", h.Name)
		}
	}
}

// linkStaysWithin reports whether a link at name, pointing at target, leads
// somewhere inside the output at rel. An output is handed to other steps on
// other machines, where a link out of it would point at whatever happens to
// be there.
func linkStaysWithin(rel, name, target string) bool {
	if target == "" || path.IsAbs(target) || filepath.IsAbs(target) {
		return false
	}
	to := path.Join(path.Dir(name), filepath.ToSlash(target))
	return to == rel || strings.HasPrefix(to, rel+"/")
}

var errTooLarge = errors.New("too large")

// capped is a writer that refuses to write more than it has left.
type capped struct {
	w    io.Writer
	left int64
}

func (c *capped) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, errTooLarge
	}
	c.left -= int64(len(p))
	return c.w.Write(p)
}

// keep archives each output a step declared and stores it, returning what
// goes into the step's result: each path with its digest. scratch is a
// directory to work in.
func keep(ctx context.Context, results Store, workspace, scratch string, outputs []string, log func(string)) (map[string]string, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	kept := map[string]string{}
	for i, rel := range outputs {
		archive := filepath.Join(scratch, fmt.Sprintf("output-%d.tar", i))
		digest, err := packOutput(root, rel, archive)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("it declares the output %s and did not produce it", rel)
		}
		if err != nil {
			return nil, fmt.Errorf("output %s: %v", rel, err)
		}
		var unshared *NotShared
		if err := results.PutBlob(ctx, digest, archive); errors.As(err, &unshared) {
			// Kept here, which is enough for steps on this machine. A step
			// elsewhere will say that it could not get it.
			log("warning: output " + rel + " is kept on this machine only: " + err.Error())
		} else if err != nil {
			return nil, fmt.Errorf("output %s could not be kept: %v", rel, err)
		}
		os.Remove(archive)
		kept[rel] = digest
	}
	return kept, nil
}

// receive puts what a step is given into its workspace.
func receive(ctx context.Context, results Store, workspace, scratch string, given []plan.Given) error {
	for i, g := range given {
		archive := filepath.Join(scratch, fmt.Sprintf("given-%d.tar", i))
		if err := results.GetBlob(ctx, g.Digest, archive); err != nil {
			if errors.Is(err, ErrNoBlob) {
				return fmt.Errorf("%s from %s is not on this machine. Outputs made on one machine reach another through a shared store (STARCI_STORE)", g.Path, g.Step)
			}
			return fmt.Errorf("fetching %s from %s: %v", g.Path, g.Step, err)
		}
		if err := unpackOutput(archive, g.Digest, workspace, g.Path); err != nil {
			return fmt.Errorf("unpacking %s from %s: %v", g.Path, g.Step, err)
		}
		os.Remove(archive)
	}
	return nil
}

// held reports whether every output in a stored result can still be had. A
// result that promises outputs nobody holds is no use to the steps that
// would be given them, so it is not reused.
func held(ctx context.Context, results Store, r plan.Result, outputs []string) bool {
	for _, rel := range outputs {
		digest, ok := r.Outputs[rel]
		if !ok || !results.HasBlob(ctx, digest) {
			return false
		}
	}
	return true
}
