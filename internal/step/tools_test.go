package step

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

// A "tool" for these tests is a shell script, which runs in any image.
func script(says string) []byte { return []byte("#!/bin/sh\necho " + says + "\n") }

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	zw.Close()
	return buf.Bytes()
}

func sumOf(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// downloads serves files by path and counts how often each is fetched.
type downloads struct {
	*httptest.Server
	hits atomic.Int64
}

func serve(t *testing.T, files map[string][]byte) *downloads {
	t.Helper()
	d := &downloads{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		d.hits.Add(1)
		w.Write(data)
	}))
	t.Cleanup(d.Close)
	return d
}

// archOf is the architecture the test image runs as on this machine.
func archOf(t *testing.T) string {
	t.Helper()
	img, err := inspectImage(testImage)
	if err != nil {
		t.Fatal(err)
	}
	return img.arch
}

func TestToolsAreOnThePath(t *testing.T) {
	needDocker(t)
	arch := archOf(t)
	archive := tarGz(t, map[string][]byte{"pkg-" + arch + "/bin/greet": script("hello from a tar")})
	zipFile := zipped(t, map[string][]byte{"shout": script("hello from a zip")})
	single := script("hello from a single file")
	srv := serve(t, map[string][]byte{"/greet-" + arch + ".tar.gz": archive, "/shout.zip": zipFile, "/whisper": single})

	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	tools := []plan.Tool{
		{Name: "greet", URL: srv.URL + "/greet-{arch}.tar.gz", SHA256: map[string]string{arch: sumOf(archive)}, Bin: []string{"pkg-{arch}/bin"}},
		{Name: "shout", URL: srv.URL + "/shout.zip", SHA256: map[string]string{arch: sumOf(zipFile)}, Bin: []string{"."}},
		{Name: "whisper", URL: srv.URL + "/whisper", SHA256: map[string]string{arch: sumOf(single)}, Bin: []string{"."}},
	}
	s := plan.Step{Name: "use", Run: "greet && shout && whisper && ls /bin/sh >/dev/null", Tools: tools, Cache: false}

	got := run(t, cfg, r, sha, s)
	if got.Status != "passed" {
		t.Fatalf("status = %s, detail: %s", got.Status, got.Detail)
	}
	if want := "hello from a tar\nhello from a zip\nhello from a single file\n"; got.Output != want {
		t.Errorf("output = %q, want %q", got.Output, want)
	}

	// Installed once per machine, however many steps ask.
	run(t, cfg, r, sha, s)
	if hits := srv.hits.Load(); hits != 3 {
		t.Errorf("the three tools were downloaded %d times in total, want 3", hits)
	}

	// A step cannot alter a tool for the steps after it.
	tamper := plan.Step{Name: "tamper", Run: "echo x > /opt/starci/tools/shout/shout", Tools: tools}
	if got := run(t, cfg, r, sha, tamper); got.Status != "failed" {
		t.Error("a step was able to write to a tool")
	}
}

func TestToolWithTheWrongChecksumIsRefused(t *testing.T) {
	needDocker(t)
	arch := archOf(t)
	srv := serve(t, map[string][]byte{"/tool": script("not what was pinned")})
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}

	got := run(t, cfg, r, sha, plan.Step{Name: "use", Run: "tool", Tools: []plan.Tool{
		{Name: "tool", URL: srv.URL + "/tool", SHA256: map[string]string{arch: strings.Repeat("0", 64)}, Bin: []string{"."}},
	}})
	if got.Status != "failed" || !strings.Contains(got.Detail, "not the "+strings.Repeat("0", 64)+" the script pins it to") {
		t.Fatalf("result = %+v, want a refusal naming the pinned checksum", got)
	}
}

func TestToolProblemsFailClearly(t *testing.T) {
	needDocker(t)
	arch := archOf(t)
	other := map[string]string{"amd64": "arm64", "arm64": "amd64"}[arch]
	data := script("x")
	srv := serve(t, map[string][]byte{"/tool": data})
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}

	cases := []struct {
		name string
		tool plan.Tool
		want string
	}{
		{"no build for this architecture", plan.Tool{Name: "tool", URL: srv.URL + "/tool", SHA256: map[string]string{other: sumOf(data)}, Bin: []string{"."}}, "has no build for " + arch},
		{"download missing", plan.Tool{Name: "tool", URL: srv.URL + "/gone", SHA256: map[string]string{arch: sumOf(data)}, Bin: []string{"."}}, "404"},
		{"bin directory not in the download", plan.Tool{Name: "tool", URL: srv.URL + "/tool", SHA256: map[string]string{arch: sumOf(data)}, Bin: []string{"bin"}}, `no directory "bin" in the download`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := run(t, cfg, r, sha, plan.Step{Name: "use", Run: "tool", Tools: []plan.Tool{c.tool}})
			if got.Status != "failed" || !strings.Contains(got.Detail, c.want) {
				t.Fatalf("result = %+v, want a failure mentioning %q", got, c.want)
			}
		})
	}
}

// A result is keyed on the tool's contents, not its name or where it came from.
func TestChangingAToolRunsTheStepAgain(t *testing.T) {
	needDocker(t)
	arch := archOf(t)
	v1, v2 := script("version one"), script("version two")
	srv := serve(t, map[string][]byte{"/v1": v1, "/v2": v2, "/v1-mirror": v1})
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	cfg := Config{Worker: "test", StateDir: t.TempDir()}
	step := func(path string, data []byte) plan.Step {
		return plan.Step{Name: "use", Run: "tool", Cache: true, Tools: []plan.Tool{
			{Name: "tool", URL: srv.URL + path, SHA256: map[string]string{arch: sumOf(data)}, Bin: []string{"."}},
		}}
	}

	if got := run(t, cfg, r, sha, step("/v1", v1)); got.Cached || got.Output != "version one\n" {
		t.Fatalf("first run = %+v", got)
	}
	if got := run(t, cfg, r, sha, step("/v1-mirror", v1)); !got.Cached {
		t.Error("the same tool from another address ran the step again")
	}
	if got := run(t, cfg, r, sha, step("/v2", v2)); got.Cached || got.Output != "version two\n" {
		t.Errorf("after the tool changed = %+v, want a fresh run with version two", got)
	}
}
