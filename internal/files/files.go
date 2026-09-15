// Package files reads the files KEEL's commands share: the world file and the
// directory of doctrine packs.
//
// Parsing belongs to the packages that own the formats (planir, doctrine);
// this package adds the file system around them, once, so that keelctl,
// keelsim and keeld refuse the same inputs with the same message. It sits
// outside the decision path, whose packages read no file.
package files

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/planir"
)

// ReadWorld parses a world file.
func ReadWorld(path string) (*planir.World, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the world: %w", err)
	}
	w, err := planir.ParseWorld(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return w, nil
}

// ReadPacks parses every pack of dir, the files matching *.yaml, into a
// registry. A pack that fails to parse fails the whole read: skipping it
// would let a plan or a hot swap name a doctrine the operator believes is
// loaded. A directory holding no pack is refused for the same reason.
func ReadPacks(dir string) (*doctrine.Registry, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no doctrine pack in %s", dir)
	}
	packs := make([]*doctrine.Pack, 0, len(paths))
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading a doctrine pack: %w", err)
		}
		p, err := doctrine.Parse(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		packs = append(packs, p)
	}
	return doctrine.NewRegistry(packs...)
}
