package step

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// Blobs keeps files by the hash of their content, which is what an
// artifact's files and the list of them are kept as.
type Blobs interface {
	// PutBlob stores the file under hash, which is its content's sha256.
	PutBlob(ctx context.Context, hash, file string) error
	// GetBlob writes what is stored under hash to file.
	GetBlob(ctx context.Context, hash, file string) error
}

// FileBlobs keeps blobs as files under dir, private to one machine.
type FileBlobs struct{ Dir string }

func (f FileBlobs) path(hash string) string { return filepath.Join(f.Dir, hash[:2], hash) }

func (f FileBlobs) PutBlob(_ context.Context, hash, file string) error {
	dest := f.path(hash)
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return copyFile(file, dest)
}

func (f FileBlobs) GetBlob(_ context.Context, hash, file string) error {
	return copyFile(f.path(hash), file)
}

// copyFile copies src to dest by way of a temporary file beside it, so that
// a reader never finds half of one.
func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".starci-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// layeredBlobs puts each blob in both stores and finds it in the near one
// first, keeping a copy of what it found in the far one.
type layeredBlobs struct{ near, far Blobs }

func (l layeredBlobs) PutBlob(ctx context.Context, hash, file string) error {
	if err := l.near.PutBlob(ctx, hash, file); err != nil {
		return err
	}
	return l.far.PutBlob(ctx, hash, file)
}

func (l layeredBlobs) GetBlob(ctx context.Context, hash, file string) error {
	if l.near.GetBlob(ctx, hash, file) == nil {
		return nil
	}
	if err := l.far.GetBlob(ctx, hash, file); err != nil {
		return err
	}
	_ = l.near.PutBlob(ctx, hash, file)
	return nil
}

func (c Config) blobs() Blobs {
	local := FileBlobs{Dir: filepath.Join(c.StateDir, "blobs")}
	if far, ok := c.Shared.(Blobs); ok {
		return layeredBlobs{near: local, far: far}
	}
	return local
}

// entry is one file of an artifact, at its path inside what was kept: empty
// for an artifact that is a single file.
type entry struct {
	Path       string `json:"path"`
	Executable bool   `json:"executable,omitempty"`
	Hash       string `json:"hash"`
	Size       int64  `json:"size"`
}

// keep stores the paths a passing step keeps, and returns a pointer to each.
func keep(ctx context.Context, blobs Blobs, workspace string, paths []string) (map[string]plan.Pointer, error) {
	kept := map[string]plan.Pointer{}
	for _, p := range paths {
		root := filepath.Join(workspace, filepath.FromSlash(p))
		var files []entry
		add := func(file, rel string, info fs.FileInfo) error {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("keep %q: %s is not a regular file or directory", p, filepath.ToSlash(filepath.Join(p, rel)))
			}
			hash, err := hashFile(file)
			if err != nil {
				return fmt.Errorf("keep %q: %w", p, err)
			}
			if err := blobs.PutBlob(ctx, hash, file); err != nil {
				return fmt.Errorf("keep %q: storing it: %w", p, err)
			}
			files = append(files, entry{Path: filepath.ToSlash(rel), Executable: info.Mode()&0o111 != 0, Hash: hash, Size: info.Size()})
			return nil
		}
		info, err := os.Lstat(root)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("keep %q: the step passed without making it", p)
		case err != nil:
			return nil, fmt.Errorf("keep %q: %w", p, err)
		case info.IsDir():
			err = filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(root, file)
				return add(file, rel, info)
			})
		default:
			err = add(root, "", info)
		}
		if err != nil {
			return nil, err
		}
		pointer, err := storeList(ctx, blobs, files)
		if err != nil {
			return nil, fmt.Errorf("keep %q: storing it: %w", p, err)
		}
		kept[p] = pointer
	}
	return kept, nil
}

// storeList stores the list of an artifact's files, which the artifact's
// pointer names. The list is in path order, so the same files are always the
// same list.
func storeList(ctx context.Context, blobs Blobs, files []entry) (plan.Pointer, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if files == nil {
		files = []entry{}
	}
	data, err := json.Marshal(files)
	if err != nil {
		return plan.Pointer{}, err
	}
	tmp, err := os.CreateTemp("", "starci-list-")
	if err != nil {
		return plan.Pointer{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return plan.Pointer{}, err
	}
	if err := tmp.Close(); err != nil {
		return plan.Pointer{}, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if err := blobs.PutBlob(ctx, hash, tmp.Name()); err != nil {
		return plan.Pointer{}, err
	}
	var size int64
	for _, f := range files {
		size += f.Size
	}
	return plan.Pointer{Hash: hash, Size: size}, nil
}

// give writes the artifacts a step is handed into its workspace. One that
// would land on a file the step's inputs gave it is refused, as is one whose
// content is not what its pointer says.
func give(ctx context.Context, blobs Blobs, workspace string, artifacts map[string]plan.Pointer) error {
	at := make([]string, 0, len(artifacts))
	for p := range artifacts {
		at = append(at, p)
	}
	sort.Strings(at)
	for _, p := range at {
		pointer := artifacts[p]
		if pointer.Hash == "" {
			return fmt.Errorf("artifact at %q: %s of %q was never kept", p, pointer.Path, pointer.From)
		}
		data, err := readBlob(ctx, blobs, pointer.Hash)
		if err != nil {
			return fmt.Errorf("artifact at %q: %w", p, err)
		}
		var files []entry
		if err := json.Unmarshal(data, &files); err != nil {
			return fmt.Errorf("artifact at %q: its list of files: %w", p, err)
		}
		root := filepath.Join(workspace, filepath.FromSlash(p))
		for _, f := range files {
			dest := filepath.Join(root, filepath.FromSlash(f.Path))
			if rel, err := filepath.Rel(workspace, dest); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("artifact at %q: %q is outside the workspace", p, f.Path)
			}
			if _, err := os.Lstat(dest); err == nil {
				return fmt.Errorf("artifact at %q: %s is already in the workspace, from the step's inputs", p, filepath.ToSlash(filepath.Join(p, f.Path)))
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("artifact at %q: %w", p, err)
			}
			if err := blobs.GetBlob(ctx, f.Hash, dest); err != nil {
				return fmt.Errorf("artifact at %q: fetching %s: %w", p, f.Path, err)
			}
			if got, err := hashFile(dest); err != nil || got != f.Hash {
				return fmt.Errorf("artifact at %q: %s is not what was kept", p, f.Path)
			}
			mode := os.FileMode(0o644)
			if f.Executable {
				mode = 0o755
			}
			if err := os.Chmod(dest, mode); err != nil {
				return fmt.Errorf("artifact at %q: %w", p, err)
			}
		}
	}
	return nil
}

// readBlob reads a small blob, checking it is what its hash says.
func readBlob(ctx context.Context, blobs Blobs, hash string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "starci-blob-")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := blobs.GetBlob(ctx, hash, tmp.Name()); err != nil {
		return nil, fmt.Errorf("it is not in the store: %w", err)
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != hash {
		return nil, errors.New("what the store holds is not what was kept")
	}
	return data, nil
}

func hashFile(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// artifactKey is what a step's key adds for the artifacts it is handed and
// keeps. Nothing for a step with neither, so that it keeps the key it had.
func artifactKey(s plan.Step) []any {
	var parts []any
	if len(s.Artifacts) > 0 {
		given := [][2]string{}
		at := make([]string, 0, len(s.Artifacts))
		for p := range s.Artifacts {
			at = append(at, p)
		}
		sort.Strings(at)
		for _, p := range at {
			given = append(given, [2]string{p, s.Artifacts[p].Hash})
		}
		parts = append(parts, "artifacts", given)
	}
	if len(s.Keep) > 0 {
		parts = append(parts, "keep", s.Keep)
	}
	return parts
}
