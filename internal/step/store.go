package step

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"git.phin.tech/sam/starci/internal/plan"
)

// Store remembers passing results by key.
//
// Whoever can write to a store can make every machine that reads it skip a
// step, by storing a pass the step never earned. Write access belongs only to
// machines trusted to run steps honestly.
type Store interface {
	// Get returns the result stored under key, if there is one. A store that
	// cannot be reached answers as if it were empty: a missing result costs a
	// step being run, never a wrong answer.
	Get(ctx context.Context, key string) (plan.Result, bool)
	Put(ctx context.Context, key string, r plan.Result) error

	// A store also keeps the outputs of steps: archives, each under the
	// checksum of its content. GetBlob writes one to the file at dest, and
	// returns ErrNoBlob if the store does not have it. PutBlob stores the
	// file at src. Whoever takes a blob out checks it against its digest, so
	// a store need not.
	GetBlob(ctx context.Context, digest, dest string) error
	PutBlob(ctx context.Context, digest, src string) error
	HasBlob(ctx context.Context, digest string) bool
}

// ErrNoBlob is what GetBlob returns for a blob a store does not hold.
var ErrNoBlob = errors.New("not in the store")

// FileStore keeps results as files under dir. It is fast and private to one
// machine.
type FileStore struct{ Dir string }

func (f FileStore) path(key string) string { return filepath.Join(f.Dir, key[:2], key+".json") }

func (f FileStore) Get(_ context.Context, key string) (plan.Result, bool) {
	data, err := os.ReadFile(f.path(key))
	if err != nil {
		return plan.Result{}, false
	}
	var r plan.Result
	if json.Unmarshal(data, &r) != nil || r.Status != "passed" {
		return plan.Result{}, false
	}
	return r, true
}

func (f FileStore) Put(_ context.Context, key string, r plan.Result) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := f.path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Written beside its final place and moved in, so a reader never sees
	// half a result.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	return os.Rename(tmp.Name(), path)
}

func (f FileStore) blob(digest string) string {
	return filepath.Join(f.Dir, "outputs", digest[:2], digest+".tar")
}

func (f FileStore) HasBlob(_ context.Context, digest string) bool {
	_, err := os.Stat(f.blob(digest))
	return err == nil
}

func (f FileStore) GetBlob(_ context.Context, digest, dest string) error {
	err := copyFile(f.blob(digest), dest)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoBlob
	}
	return err
}

func (f FileStore) PutBlob(_ context.Context, digest, src string) error {
	path := f.blob(digest)
	if _, err := os.Stat(path); err == nil {
		return nil // the same content, already here
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	tmp.Close()
	if err := copyFile(src, tmp.Name()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Layered reads from Near first and falls back to Far, and writes to both.
// Near is this machine's own store; Far is one shared between machines. A
// result found only in Far is copied to Near, so that it is asked for once.
type Layered struct{ Near, Far Store }

func (l Layered) Get(ctx context.Context, key string) (plan.Result, bool) {
	if r, ok := l.Near.Get(ctx, key); ok {
		return r, true
	}
	r, ok := l.Far.Get(ctx, key)
	if ok {
		_ = l.Near.Put(ctx, key, r)
	}
	return r, ok
}

// Put stores the result in both. Failing to reach the shared store is
// reported but is not a reason to forget the result here.
func (l Layered) Put(ctx context.Context, key string, r plan.Result) error {
	nearErr := l.Near.Put(ctx, key, r)
	if err := l.Far.Put(ctx, key, r); err != nil {
		return err
	}
	return nearErr
}

func (l Layered) HasBlob(ctx context.Context, digest string) bool {
	return l.Near.HasBlob(ctx, digest) || l.Far.HasBlob(ctx, digest)
}

func (l Layered) GetBlob(ctx context.Context, digest, dest string) error {
	if err := l.Near.GetBlob(ctx, digest, dest); err == nil {
		return nil
	}
	if err := l.Far.GetBlob(ctx, digest, dest); err != nil {
		return err
	}
	_ = l.Near.PutBlob(ctx, digest, dest)
	return nil
}

// PutBlob stores the blob in both. One that is kept here but did not reach
// the shared store comes back as NotShared, which says so.
func (l Layered) PutBlob(ctx context.Context, digest, src string) error {
	if err := l.Near.PutBlob(ctx, digest, src); err != nil {
		return err
	}
	if err := l.Far.PutBlob(ctx, digest, src); err != nil {
		return &NotShared{err}
	}
	return nil
}

// NotShared is the error for something stored on this machine that could not
// be stored where other machines would find it.
type NotShared struct{ Err error }

func (e *NotShared) Error() string { return e.Err.Error() }
func (e *NotShared) Unwrap() error { return e.Err }
