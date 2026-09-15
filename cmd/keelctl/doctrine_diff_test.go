package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/eventlog"
)

// diverging is a pack directory holding the recorded pack, 2.2.0, and 3.0.0,
// the same with the whole battery held in reserve.
func diverging(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(shippedPacks, "recon-standard-v2.2.0.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recon-standard-v2.2.0.yaml"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	src = bytes.Replace(src, []byte("version: 2.2.0"), []byte("version: 3.0.0"), 1)
	src = bytes.Replace(src, []byte("battery_reserve_pct: 15"), []byte("battery_reserve_pct: 100"), 1)
	if err := os.WriteFile(filepath.Join(dir, "recon-standard-v3.0.0.yaml"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDoctrineDiff(t *testing.T) {
	mem := eventlog.NewMemLog()
	recordMission(t, mem)
	raw := encodedLog(t, mem)

	cases := []struct {
		name     string
		dir      string
		a, b     string
		code     int
		want     []string
		wantErrs string
	}{
		{"a pack with itself", shippedPacks, "recon-standard@2.2.0", "recon-standard@2.2.0", exitOK,
			[]string{"none diverging", "decide the same"}, ""},
		// 2.2.0 only narrows a rule for vectors returning to base, and none
		// is: the approval naming its pack is not a difference.
		{"packs that never disagree here", shippedPacks, "recon-standard@2.1.0", "recon-standard@2.2.0", exitOK,
			[]string{"first     recon-standard@2.1.0 (", "second    recon-standard@2.2.0 (", "decide the same"}, ""},
		{"packs that disagree", diverging(t), "recon-standard@2.2.0", "recon-standard@3.0.0", exitPacksDiffer,
			[]string{"    0.1 s\n", "  - assignment", "the first at 0.1 s", "decide differently"}, "the packs decide differently"},
		{"an unknown pack", shippedPacks, "recon-standard@2.2.0", "recon-standard@9.9.9", exitError,
			nil, "9.9.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := keelctlIn(t, bytes.NewReader(raw), "doctrine", "diff", "--doctrine-dir", tc.dir, "-", tc.a, tc.b)
			if code != tc.code || !strings.Contains(errOut, tc.wantErrs) {
				t.Fatalf("exit %d, want %d\n%s\n%s", code, tc.code, out, errOut)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}
