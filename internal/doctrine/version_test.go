package doctrine

import (
	"errors"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

func TestValidateVersion(t *testing.T) {
	valid := []string{"0.0.1", "2.1.0", "10.20.30", "1.0.0-rc.1", "1.0.0-alpha.beta.1", "1.0.0-0"}
	for _, v := range valid {
		if err := ValidateVersion(v); err != nil {
			t.Errorf("ValidateVersion(%q) = %v, want nil", v, err)
		}
	}
	invalid := []string{
		"",
		"v1.0.0",       // prefix
		"1",            // shorthand
		"1.2",          // shorthand
		"01.0.0",       // leading zero
		"1.0.0+build",  // build metadata
		"1.0.0-01",     // leading zero in a numeric pre-release identifier
		" 1.0.0",       // whitespace
		"1.0.0-",       // empty pre-release
		"latest",       // not a version
		"1.0.0-rc..1",  // empty identifier
		"1.0.0-rc.1+x", // build metadata after a pre-release
	}
	for _, v := range invalid {
		if err := ValidateVersion(v); !errors.Is(err, ErrInvalidVersion) {
			t.Errorf("ValidateVersion(%q) = %v, want ErrInvalidVersion", v, err)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.0.0", "2.1.0", -1},
		{"2.1.0", "2.1.0", 0},
		{"2.1.0", "10.0.0", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"3.0.0", "2.9.9", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseRef(t *testing.T) {
	ref, err := ParseRef("recon-standard@2.1.0")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if want := (domain.DoctrineRef{Name: "recon-standard", Version: "2.1.0"}); ref != want {
		t.Fatalf("ParseRef = %+v, want %+v", ref, want)
	}
	if ref.String() != "recon-standard@2.1.0" {
		t.Fatalf("round trip = %q", ref.String())
	}

	for _, s := range []string{"recon-standard", "recon-standard@", "@2.1.0", "Recon@2.1.0", "recon_standard@2.1.0", "recon@v2.1.0", "recon@2.1"} {
		if _, err := ParseRef(s); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("ParseRef(%q) = %v, want ErrInvalidRef", s, err)
		}
	}
}

func TestValidIdent(t *testing.T) {
	for _, s := range []string{"a", "battery-reserve", "recon-standard", "rule-2"} {
		if !ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = false", s)
		}
	}
	for _, s := range []string{"", "-a", "a-", "a--b", "A", "a_b", "a b", "a.b"} {
		if ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = true", s)
		}
	}
}
