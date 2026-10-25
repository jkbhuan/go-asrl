//go:build !windows

package asrl

import "fmt"

func openBackend(name string, cfg Config) (backend, error) {
	return nil, notStarted(fmt.Errorf("%w: serial backend is currently Windows-only (OS port %q)", ErrNotSupported, name))
}
