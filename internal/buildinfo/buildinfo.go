// Package buildinfo carries the version stamped in at link time.
//
// The values are overridden by -ldflags -X from the Makefile and the
// Dockerfiles. A binary built without them reports "dev", which is honest
// rather than misleading.
package buildinfo

// Set through -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
)

// String renders the stamp for --version output and the /api/v1 banner.
func String() string {
	return Version + " (" + Commit + ")"
}
