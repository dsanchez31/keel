package doctrine

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/dsanchez31/keel/internal/domain"
)

// ErrInvalidVersion reports a version that is not strict SemVer 2.0.0.
var ErrInvalidVersion = errors.New("doctrine: invalid version")

// ErrInvalidRef reports a doctrine reference that is not "<name>@<semver>".
var ErrInvalidRef = errors.New("doctrine: invalid doctrine reference")

// identPattern is lowercase kebab-case: pack names and the ids of roles,
// constraints and rules.
var identPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidIdent reports whether s is lowercase kebab-case.
func ValidIdent(s string) bool { return identPattern.MatchString(s) }

// ValidateVersion accepts strict SemVer 2.0.0 with no "v" prefix and no build
// metadata: MAJOR.MINOR.PATCH with an optional pre-release.
//
// golang.org/x/mod/semver implements the precedence rules but expects a "v"
// prefix, and it also accepts shorthands ("v1", "v1.2") and build metadata.
// A pack version is part of what is hashed and compared, so exactly one
// spelling of each version is accepted: the one semver.Canonical returns.
func ValidateVersion(s string) error {
	if s == "" || strings.HasPrefix(s, "v") {
		return fmt.Errorf("%w: %q is not MAJOR.MINOR.PATCH[-PRERELEASE]", ErrInvalidVersion, s)
	}
	v := "v" + s
	if !semver.IsValid(v) || semver.Canonical(v) != v {
		return fmt.Errorf("%w: %q is not MAJOR.MINOR.PATCH[-PRERELEASE]", ErrInvalidVersion, s)
	}
	return nil
}

// CompareVersions orders two versions by SemVer precedence, returning -1, 0
// or +1. An invalid version orders before every valid one, and all invalid
// versions compare equal, as in golang.org/x/mod/semver.
func CompareVersions(a, b string) int { return semver.Compare("v"+a, "v"+b) }

// ParseRef parses the canonical "<name>@<semver>" form of a doctrine
// reference, the form Plan IR carries in its doctrine field.
func ParseRef(s string) (domain.DoctrineRef, error) {
	name, version, ok := strings.Cut(s, "@")
	if !ok {
		return domain.DoctrineRef{}, fmt.Errorf("%w: %q has no '@'", ErrInvalidRef, s)
	}
	ref := domain.DoctrineRef{Name: name, Version: version}
	if err := ValidateRef(ref); err != nil {
		return domain.DoctrineRef{}, err
	}
	return ref, nil
}

// ValidateRef checks both halves of a reference.
func ValidateRef(ref domain.DoctrineRef) error {
	if !ValidIdent(ref.Name) {
		return fmt.Errorf("%w: name %q is not lowercase kebab-case", ErrInvalidRef, ref.Name)
	}
	if err := ValidateVersion(ref.Version); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRef, err)
	}
	return nil
}
