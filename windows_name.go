package asrl

import (
	"fmt"
	"strings"
)

// Kept platform independent so name handling is also checked by Unix CI.
func windowsPortName(name string) (string, error) {
	invalid := func() (string, error) {
		return "", fmt.Errorf("%w: use COM<number> or a Windows device path", ErrInvalidConfig)
	}
	if strings.ContainsRune(name, 0) {
		return invalid()
	}
	if strings.HasPrefix(name, `\\.\`) || strings.HasPrefix(name, `\\?\`) {
		if len(name) == 4 {
			return invalid()
		}
		return name, nil
	}
	upper := strings.ToUpper(name)
	if !strings.HasPrefix(upper, "COM") || len(upper) < 4 {
		return invalid()
	}
	digits := upper[3:]
	if digits[0] == '0' {
		return invalid()
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return invalid()
		}
	}
	return `\\.\` + upper, nil
}
