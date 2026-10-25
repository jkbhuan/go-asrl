//go:build !windows

package asrl

import (
	"errors"
	"testing"
)

func TestUnsupportedPlatform(t *testing.T) {
	for _, name := range []string{"/dev/ttyS2", "2", "COM2"} {
		p, err := Open(name, DefaultConfig())
		if p != nil || !errors.Is(err, ErrNotSupported) || !isUnstarted(err) {
			t.Fatalf("%q: %v %v", name, p, err)
		}
	}
}
