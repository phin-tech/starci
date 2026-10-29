// Package bundle turns a starci executable into one that carries a script (or
// the plan it evaluates to) and runs it, by appending the payload to a copy of
// the executable. A Go program outside this module cannot import starci's
// internal packages, so the runtime is starci itself.
//
// The file is: the executable, the payload as JSON, the payload's length as 8
// bytes (big endian), then a fixed trailer. Executables ignore what follows
// their own sections, so the copy still runs as starci would.
package bundle

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"git.phin.tech/sam/starci/internal/plan"
)

const trailer = "\x00starci-bundle-v1"

// Payload is what a bundled executable runs.
type Payload struct {
	// Name is the script's file name, for messages.
	Name string `json:"name"`
	// Script is the source. A bundle that is not frozen evaluates it when it
	// runs, against the event and checkout it is run in, so advanced scripts
	// and scripts that read ctx work.
	Script string `json:"script,omitempty"`
	// Plan is set for a frozen bundle: the script was evaluated when the
	// bundle was written and is not evaluated again.
	Plan *plan.Plan `json:"plan,omitempty"`
}

// Frozen reports whether the plan was fixed when the bundle was written.
func (p *Payload) Frozen() bool { return p != nil && p.Plan != nil }

// Write writes a copy of the executable at base with the payload appended.
func Write(out, base string, p Payload) error {
	exe, err := os.ReadFile(base)
	if err != nil {
		return err
	}
	if has, err := Has(exe); err != nil {
		return err
	} else if has {
		return fmt.Errorf("%s already carries a script; use an unbundled starci as the base", base)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(body)))
	var buf bytes.Buffer
	buf.Grow(len(exe) + len(body) + len(n) + len(trailer))
	buf.Write(exe)
	buf.Write(body)
	buf.Write(n[:])
	buf.WriteString(trailer)
	return os.WriteFile(out, buf.Bytes(), 0o755)
}

// Has reports whether an executable's bytes end with a payload.
func Has(exe []byte) (bool, error) { return bytes.HasSuffix(exe, []byte(trailer)), nil }

// Read returns the payload carried by the file at path, or nil if it has none.
func Read(path string) (*Payload, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	tail := int64(len(trailer) + 8)
	if st.Size() < tail {
		return nil, nil
	}
	end := make([]byte, tail)
	if _, err := f.ReadAt(end, st.Size()-tail); err != nil {
		return nil, err
	}
	if string(end[8:]) != trailer {
		return nil, nil
	}
	n := int64(binary.BigEndian.Uint64(end[:8]))
	if n <= 0 || n > st.Size()-tail {
		return nil, errors.New("the script carried by this executable is damaged")
	}
	body, err := io.ReadAll(io.NewSectionReader(f, st.Size()-tail-n, n))
	if err != nil {
		return nil, err
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("the script carried by this executable is damaged: %w", err)
	}
	return &p, nil
}

// Self returns the payload carried by the running executable, or nil.
func Self() (*Payload, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil
	}
	return Read(exe)
}
