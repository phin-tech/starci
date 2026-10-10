package s3

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/step"
)

// TestBlobs puts an output into a bucket and takes it out again. It needs a
// bucket to talk to, so it runs only when given one, written as STARCI_STORE
// is, with the AWS_* variables beside it:
//
//	STARCI_TEST_STORE='s3://bucket/prefix?endpoint=host' go test ./internal/store/s3
func TestBlobs(t *testing.T) {
	location := os.Getenv("STARCI_TEST_STORE")
	if location == "" {
		t.Skip("set STARCI_TEST_STORE, and the AWS_* variables, to run against a bucket")
	}
	store, err := Open(location)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Check(ctx); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	src, dest := filepath.Join(dir, "src"), filepath.Join(dir, "dest")
	content := strings.Repeat("an output\n", 1000)
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Any name will do: a store does not check a blob against its digest.
	digest := strings.Repeat("ab", 32)
	absent := strings.Repeat("cd", 32)

	if store.HasBlob(ctx, absent) {
		t.Error("the store says it has a blob nobody put there")
	}
	if err := store.GetBlob(ctx, absent, dest); !errors.Is(err, step.ErrNoBlob) {
		t.Errorf("getting a blob that is not there: %v, want ErrNoBlob", err)
	}
	if err := store.PutBlob(ctx, digest, src); err != nil {
		t.Fatal(err)
	}
	if !store.HasBlob(ctx, digest) {
		t.Error("the store does not have the blob just put there")
	}
	if err := store.GetBlob(ctx, digest, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != content {
		t.Errorf("what came back is %d bytes, not the %d put in", len(got), len(content))
	}
}
