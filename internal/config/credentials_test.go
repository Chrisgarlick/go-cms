package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Under systemd the unit's LoadCredential copy of the key wins over the
// root-only file the config names; without it, the config's path stands.
func TestKEKFromCredentials(t *testing.T) {
	dir := t.TempDir()
	c := Defaults()
	c.Keys.KEKFile = "/etc/example/kek"
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	applyEnv(&c)
	if c.Keys.KEKFile != "/etc/example/kek" {
		t.Fatalf("no credential: KEKFile = %q, want the configured path", c.Keys.KEKFile)
	}
	if err := os.WriteFile(filepath.Join(dir, "kek"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	applyEnv(&c)
	if want := filepath.Join(dir, "kek"); c.Keys.KEKFile != want {
		t.Fatalf("with a credential: KEKFile = %q, want %q", c.Keys.KEKFile, want)
	}
}
