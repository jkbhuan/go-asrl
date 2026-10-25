package asrl

import (
	"errors"
	"fmt"
	"os"
)

var (
	ErrInvalidConfig = errors.New("asrl: invalid configuration")
	ErrNotSupported  = errors.New("asrl: not supported")
	ErrTimeout       = errors.New("asrl: timeout")
	ErrClosed        = errors.New("asrl: port closed")
	ErrDisconnected  = errors.New("asrl: device or handle unavailable")
)

// OpError retains the operation, original OS port name and underlying error.
type OpError struct {
	Op   string
	Port string
	Err  error
}

func (e *OpError) Error() string { return fmt.Sprintf("asrl %s %q: %v", e.Op, e.Port, e.Err) }
func (e *OpError) Unwrap() error { return e.Err }
func (e *OpError) Timeout() bool {
	return errors.Is(e.Err, ErrTimeout) || errors.Is(e.Err, os.ErrDeadlineExceeded)
}

type timeoutError struct{ error }

func (e *timeoutError) Unwrap() error { return e.error }
func (e *timeoutError) Is(target error) bool {
	return target == ErrTimeout || target == os.ErrDeadlineExceeded
}
func (e *timeoutError) Timeout() bool { return true }

type unstartedError struct{ error }

func (e *unstartedError) Unwrap() error             { return e.error }
func (e *unstartedError) OperationNotStarted() bool { return true }

func notStarted(err error) error { return &unstartedError{err} }

// A later short-write iteration may fail before submitting that iteration, but
// the complete API call already touched I/O. Hide any inner positive marker.
type startedError struct{ error }

func (e *startedError) Unwrap() error             { return e.error }
func (e *startedError) OperationNotStarted() bool { return false }
