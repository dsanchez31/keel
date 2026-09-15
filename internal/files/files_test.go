package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped artifacts, relative to this package.
const (
	shippedWorld = "../../examples/worlds/reference.yaml"
	shippedPacks = "../../doctrine-packs"
)

func TestReadWorld(t *testing.T) {
	w, err := ReadWorld(shippedWorld)
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "reference" || len(w.Fleet) == 0 {
		t.Fatalf("world %q with %d vectors", w.Name, len(w.Fleet))
	}
	if _, err := ReadWorld(filepath.Join(t.TempDir(), "none.yaml")); err == nil || !strings.Contains(err.Error(), "reading the world") {
		t.Fatalf("a missing world: err %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: keel.world/v0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWorld(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("a malformed world: err %v, want it to name the file", err)
	}
}

func TestReadPacks(t *testing.T) {
	r, err := ReadPacks(shippedPacks)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Refs()) < 3 {
		t.Fatalf("registry holds %v, want every shipped pack", r.Refs())
	}
	if _, err := ReadPacks(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no doctrine pack") {
		t.Fatalf("an empty directory: err %v", err)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(bad, []byte("not: a pack\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPacks(dir); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("a broken pack: err %v, want it to name the file", err)
	}
}
