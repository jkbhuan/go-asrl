// Package asrl provides cancellable, full-duplex raw serial byte streams.
// Windows has a pure Go backend. Other platforms currently return ErrNotSupported.
// The library does not implement message boundaries, terminal emulation or SCPI.
package asrl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A backend must settle all I/O and cancellation callbacks before returning from
// read/write. close must interrupt both directions without taking their gates.
type backend interface {
	read(context.Context, []byte) (int, error)
	write(context.Context, []byte) (int, error)
	close() error
}

// Port allows one read and one write concurrently. Same-direction operations
// queue with cancellation and share one total budget per API call. Do not copy
// a Port; obtain it with Open. Its zero value is not an open port.
type Port struct {
	name                string
	dev                 backend
	timeout             atomic.Int64
	readGate, writeGate chan struct{}
	closed              chan struct{}
	closeOnce           sync.Once
	closeErr            error
}

var _ io.ReadWriteCloser = (*Port)(nil)

// Open opens an OS port name (COM2, COM10 or a Windows device path). It does not
// accept VISA resources. Configuration is validated before opening the device.
// The native open/configuration calls are synchronous and have no context API.
func Open(portName string, cfg Config) (*Port, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return nil, &OpError{Op: "open", Port: portName, Err: notStarted(err)}
	}
	if strings.TrimSpace(portName) == "" || strings.ContainsRune(portName, 0) {
		return nil, &OpError{Op: "open", Port: portName, Err: notStarted(fmt.Errorf("%w: empty port or embedded NUL", ErrInvalidConfig))}
	}
	dev, err := openBackend(portName, cfg)
	if err != nil {
		return nil, &OpError{Op: "open", Port: portName, Err: err}
	}
	return newPort(portName, cfg.Timeout, dev), nil
}

func newPort(name string, timeout time.Duration, dev backend) *Port {
	p := &Port{name: name, dev: dev, closed: make(chan struct{}), readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1)}
	p.timeout.Store(int64(timeout))
	p.readGate <- struct{}{}
	p.writeGate <- struct{}{}
	return p
}

// SetTimeout changes only subsequent operations. d <= 0 removes the default
// budget. It neither waits for I/O nor changes budgets already captured by calls.
func (p *Port) SetTimeout(d time.Duration) { p.timeout.Store(int64(d)) }

func (p *Port) Read(buf []byte) (int, error)  { return p.ReadContext(context.Background(), buf) }
func (p *Port) Write(buf []byte) (int, error) { return p.WriteContext(context.Background(), buf) }

// ReadContext returns available raw bytes without requiring a full buffer.
// ctx must be non-nil. Quiet ports wait, time out or cancel; they are not EOF.
func (p *Port) ReadContext(ctx context.Context, buf []byte) (int, error) {
	return p.operate(ctx, buf, false, time.Time{}, false)
}

// WriteContext continues short writes until all bytes are accepted or an error
// occurs. A returned prefix may already have reached the device. Success does
// not imply physical transmission or device execution. ctx must be non-nil.
func (p *Port) WriteContext(ctx context.Context, buf []byte) (int, error) {
	return p.operate(ctx, buf, true, time.Time{}, false)
}

// ReadWithDeadline replaces the default timeout with an absolute deadline.
// A zero deadline removes that budget. Context cancellation/deadlines still apply.
func (p *Port) ReadWithDeadline(ctx context.Context, buf []byte, deadline time.Time) (int, error) {
	return p.operate(ctx, buf, false, deadline, true)
}

// Close is idempotent and interrupts active I/O before waiting for its completion.
// Concurrent callers receive the same close result. No reconnection is attempted.
func (p *Port) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		if err := p.dev.close(); err != nil {
			p.closeErr = &OpError{Op: "close", Port: p.name, Err: err}
		}
	})
	return p.closeErr
}

func (p *Port) stateError(ctx context.Context) error {
	select {
	case <-p.closed:
		return ErrClosed
	default:
		return ctx.Err()
	}
}

func (p *Port) opError(op string, err error, unstarted bool) error {
	if errors.Is(err, context.DeadlineExceeded) {
		err = &timeoutError{err}
	}
	if unstarted {
		err = notStarted(err)
	}
	return &OpError{Op: op, Port: p.name, Err: err}
}

func (p *Port) operate(ctx context.Context, buf []byte, write bool, deadline time.Time, override bool) (int, error) {
	// Even a closed port or cancelled context permits an empty no-op.
	if len(buf) == 0 {
		return 0, nil
	}
	var cancel context.CancelFunc
	if override {
		if !deadline.IsZero() {
			ctx, cancel = context.WithDeadline(ctx, deadline)
		}
	} else if d := time.Duration(p.timeout.Load()); d > 0 {
		ctx, cancel = context.WithTimeout(ctx, d)
	}
	if cancel != nil {
		defer cancel()
	}
	op, gate := "read", p.readGate
	if write {
		op, gate = "write", p.writeGate
	}
	if err := p.stateError(ctx); err != nil {
		return 0, p.opError(op, err, true)
	}
	select {
	case <-p.closed:
		return 0, p.opError(op, ErrClosed, true)
	case <-ctx.Done():
		return 0, p.opError(op, p.stateError(ctx), true)
	case <-gate:
	}
	defer func() { gate <- struct{}{} }()
	started, total := false, 0
	for {
		if err := p.stateError(ctx); err != nil {
			return total, p.opError(op, err, !started)
		}
		var n int
		var err error
		previouslyStarted := started
		if write {
			n, err = p.dev.write(ctx, buf[total:])
		} else {
			n, err = p.dev.read(ctx, buf)
		}
		started = true // n == 0 does not prove that no I/O occurred.
		remaining := len(buf) - total
		if n < 0 || n > remaining {
			return total, p.opError(op, fmt.Errorf("invalid backend byte count %d (buffer %d)", n, remaining), false)
		}
		total += n
		if err != nil {
			if previouslyStarted || n > 0 {
				err = &startedError{err}
			}
			// Explicit close has priority for an interrupted operation. Genuine
			// device errors and their OS causes are otherwise retained unchanged.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				select {
				case <-p.closed:
					err = errors.Join(ErrClosed, err)
				default:
				}
			}
			return total, p.opError(op, err, false)
		}
		if (!write && n > 0) || (write && total == len(buf)) {
			return total, nil
		}
		if n == 0 {
			// A defective/quiet backend must not create empty successes or spin.
			timer := time.NewTimer(2 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
			case <-p.closed:
			}
			timer.Stop()
		}
	}
}
