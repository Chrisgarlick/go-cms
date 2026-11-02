package tailwind

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI is a stand-in for the Tailwind binary: it writes the input file,
// upper-cased, to the -o path.
const fakeCLI = `#!/bin/sh
in=""; out=""
while [ $# -gt 0 ]; do
  case "$1" in -i) in="$2"; shift;; -o) out="$2"; shift;; esac
  shift
done
tr a-z A-Z < "$in" > "$out"
`

func site(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "theme/tailwind"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Input), []byte("body{color:red}"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "tailwindcss")
	if err := os.WriteFile(bin, []byte(fakeCLI), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("CMS_TAILWIND_BIN", bin)
	return dir
}

func TestBuildAndCheck(t *testing.T) {
	ctx := context.Background()
	if Used(t.TempDir()) {
		t.Fatal("a site without theme/tailwind uses Tailwind")
	}
	dir := site(t)
	if err := Check(ctx, dir); !errors.Is(err, ErrStale) {
		t.Errorf("check before any build: %v", err)
	}
	wrote, err := Build(ctx, dir)
	if err != nil || !wrote {
		t.Fatalf("first build: %v %v", wrote, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, Output)); string(b) != "BODY{COLOR:RED}" {
		t.Errorf("output %q", b)
	}
	if wrote, err := Build(ctx, dir); err != nil || wrote {
		t.Errorf("an unchanged build wrote the file: %v %v", wrote, err)
	}
	if err := Check(ctx, dir); err != nil {
		t.Errorf("check after building: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, Input), []byte("body{color:blue}"), 0o644)
	if err := Check(ctx, dir); !errors.Is(err, ErrStale) {
		t.Errorf("check after editing the source: %v", err)
	}
}

func TestBinaryVerifiesTheDownload(t *testing.T) {
	plat, err := platform()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("CMS_TAILWIND_BIN", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	body := "#!/bin/sh\necho tailwind\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	oldURL, oldSum := downloadURL, checksums[plat]
	t.Cleanup(func() { downloadURL, checksums[plat] = oldURL, oldSum })
	downloadURL = func(string) string { return srv.URL }

	// The real checksum doesn't match the fake download: refused.
	if _, err := Binary(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a download with the wrong checksum: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	checksums[plat] = hex.EncodeToString(sum[:])
	bin, err := Binary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(bin); err != nil || st.Mode()&0o111 == 0 {
		t.Errorf("binary not installed executable: %v", err)
	}
	// Cached: a second call doesn't download.
	srv.Close()
	if again, err := Binary(context.Background()); err != nil || again != bin {
		t.Errorf("cached binary: %v %v", again, err)
	}
}
