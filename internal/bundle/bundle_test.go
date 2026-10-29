package bundle

import (
	"os"
	"path/filepath"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	os.WriteFile(base, []byte("\x7fELF pretend executable"), 0o755)

	if p, err := Read(base); err != nil || p != nil {
		t.Fatalf("an unbundled file carries nothing, got %v, %v", p, err)
	}

	out := filepath.Join(dir, "ci")
	want := Payload{Name: ".ci.star", Script: "def ci(ctx): pass", Plan: &plan.Plan{Steps: []plan.Step{{Name: "a"}}}}
	if err := Write(out, base, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(out)
	if err != nil || got == nil {
		t.Fatalf("read: %v, %v", got, err)
	}
	if got.Name != want.Name || got.Script != want.Script || !got.Frozen() || got.Plan.Steps[0].Name != "a" {
		t.Fatalf("got %+v", got)
	}
	if st, _ := os.Stat(out); st.Mode().Perm()&0o100 == 0 {
		t.Fatal("the bundle is not executable")
	}

	if err := Write(filepath.Join(dir, "again"), out, want); err == nil {
		t.Fatal("bundling a bundle should be refused")
	}
}

func TestDamaged(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	os.WriteFile(f, append([]byte("junk\xff\xff\xff\xff\xff\xff\xff\xff"), trailer...), 0o755)
	if _, err := Read(f); err == nil {
		t.Fatal("a bad length should be an error")
	}
}
