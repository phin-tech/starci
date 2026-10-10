package step

import (
	"context"
	"encoding/json"
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

	// A store also keeps small documents by name, which is how what is known
	// about a step's last pass is kept. They are notes, not results: one
	// that is missing or cannot be stored costs an explanation and nothing
	// else.
	GetDoc(ctx context.Context, name string) ([]byte, bool)
	PutDoc(ctx context.Context, name string, data []byte) error
}

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

func (f FileStore) doc(name string) string {
	return filepath.Join(f.Dir, "docs", filepath.FromSlash(name))
}

func (f FileStore) GetDoc(_ context.Context, name string) ([]byte, bool) {
	data, err := os.ReadFile(f.doc(name))
	return data, err == nil
}

func (f FileStore) PutDoc(_ context.Context, name string, data []byte) error {
	path := f.doc(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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

// GetDoc asks Far first. A document is a note about the latest of
// something, and where machines share a store it is the shared one that
// knows what the others have done since.
func (l Layered) GetDoc(ctx context.Context, name string) ([]byte, bool) {
	if data, ok := l.Far.GetDoc(ctx, name); ok {
		return data, true
	}
	return l.Near.GetDoc(ctx, name)
}

func (l Layered) PutDoc(ctx context.Context, name string, data []byte) error {
	nearErr := l.Near.PutDoc(ctx, name, data)
	if err := l.Far.PutDoc(ctx, name, data); err != nil {
		return err
	}
	return nearErr
}
