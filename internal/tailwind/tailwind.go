// Package tailwind builds a site theme's Tailwind CSS with the standalone
// Tailwind CLI, so a site can use Tailwind without Node (spec 4.12).
//
// A theme opts in with theme/tailwind/input.css (and usually
// tailwind.config.js beside it). The output, theme/assets/tailwind.css, is
// committed: the server never builds CSS. The CLI is a pinned release,
// downloaded on first use and checked against checksums held here, in the
// cms source, not fetched alongside the binary.
package tailwind

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Version is the Tailwind CLI every site builds with.
const Version = "v3.4.17"

// checksums are the SHA-256 of each platform's v3.4.17 standalone binary
// (from the release's sha256sums.txt, checked in so a swapped download fails).
var checksums = map[string]string{
	"linux-arm64": "69b1378b8133192d7d2feb12a116fa12d035594f58db3eff215879e4ad8cf39b",
	"linux-x64":   "7d24f7fa191d2193b78cd5f5a42a6093e14409521908529f42d80b11fde1f1d4",
	"macos-arm64": "a1d0c7985759accca0bf12e51ac1dcbf0f6cf2fffb62e6e0f62d091c477a10a3",
	"macos-x64":   "6cbdad74be776c087ffa5e9a057512c54898f9fe8828d3362212dfe32fc933a3",
}

// Paths inside a site bundle.
const (
	Input  = "theme/tailwind/input.css"
	Config = "theme/tailwind/tailwind.config.js"
	Output = "theme/assets/tailwind.css"
)

// ErrStale: the committed CSS doesn't match its sources.
var ErrStale = errors.New(Output + " is out of date: run `cms tailwind` (cms dev does it as you edit) and commit it")

// Used reports whether a site bundle has a Tailwind theme.
func Used(siteDir string) bool {
	_, err := os.Stat(filepath.Join(siteDir, Input))
	return err == nil
}

func platform() (string, error) {
	osName := map[string]string{"linux": "linux", "darwin": "macos"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	p := osName + "-" + arch
	if _, ok := checksums[p]; osName == "" || arch == "" || !ok {
		return "", fmt.Errorf("tailwind: no pinned CLI for %s/%s (set CMS_TAILWIND_BIN to a %s binary)", runtime.GOOS, runtime.GOARCH, Version)
	}
	return p, nil
}

// Client fetches the CLI (tests swap it).
var Client = &http.Client{Timeout: 5 * time.Minute}

// downloadURL is where a platform's binary comes from (tests swap it).
var downloadURL = func(p string) string {
	return "https://github.com/tailwindlabs/tailwindcss/releases/download/" + Version + "/tailwindcss-" + p
}

// Binary returns the path of the pinned CLI, downloading and verifying it
// into the user cache directory the first time. CMS_TAILWIND_BIN overrides
// it (an offline machine, an unsupported platform).
func Binary(ctx context.Context) (string, error) {
	if p := os.Getenv("CMS_TAILWIND_BIN"); p != "" {
		return p, nil
	}
	plat, err := platform()
	if err != nil {
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "cms", "tailwind", Version)
	bin := filepath.Join(dir, "tailwindcss-"+plat)
	if ok, _ := matches(bin, checksums[plat]); ok {
		return bin, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL(plat), nil)
	if err != nil {
		return "", err
	}
	resp, err := Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tailwind: downloading the CLI: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tailwind: downloading the CLI: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 200<<20)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != checksums[plat] {
		return "", fmt.Errorf("tailwind: the downloaded CLI's checksum is %s, want %s: not using it", got, checksums[plat])
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // an executable
		return "", err
	}
	if err := os.Rename(tmp.Name(), bin); err != nil {
		return "", err
	}
	return bin, nil
}

func matches(path, sum string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // our cache
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sum, nil
}

// compile runs the CLI and returns the CSS.
func compile(ctx context.Context, siteDir string) ([]byte, error) {
	bin, err := Binary(ctx)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "cms-tailwind-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	out := filepath.Join(tmp, "tailwind.css")
	args := []string{"-i", Input, "-o", out, "--minify"}
	if _, err := os.Stat(filepath.Join(siteDir, Config)); err == nil {
		args = append(args, "-c", Config)
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // the pinned CLI
	cmd.Dir = siteDir
	cmd.Env = append(os.Environ(), "NODE_ENV=production", "BROWSERSLIST_IGNORE_OLD_DATA=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tailwind: %w\n%s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return os.ReadFile(out) //nolint:gosec // our temp file
}

// Build compiles the site's CSS into theme/assets/tailwind.css, writing it
// only when it changed. It reports whether it wrote. A site without
// theme/tailwind/input.css is left alone.
func Build(ctx context.Context, siteDir string) (bool, error) {
	if !Used(siteDir) {
		return false, nil
	}
	css, err := compile(ctx, siteDir)
	if err != nil {
		return false, err
	}
	dst := filepath.Join(siteDir, Output)
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, css) { //nolint:gosec // the site's own file
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, css, 0o644); err != nil { //nolint:gosec // a public stylesheet
		return false, err
	}
	return true, os.Rename(tmp, dst)
}

// Check compiles the CSS and compares it with the committed file (CI).
func Check(ctx context.Context, siteDir string) error {
	if !Used(siteDir) {
		return nil
	}
	css, err := compile(ctx, siteDir)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(filepath.Join(siteDir, Output)) //nolint:gosec // the site's own file
	if err != nil || !bytes.Equal(old, css) {
		return ErrStale
	}
	return nil
}
