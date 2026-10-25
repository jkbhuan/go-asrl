//go:build windows

package asrl

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows DCB bit positions (the SDK exposes these as C bitfields).
const (
	dcbBinary        = uint32(1 << 0)
	dcbParity        = uint32(1 << 1)
	dcbDTR           = uint32(1 << 4)
	dcbRTS           = uint32(1 << 12)
	dcbRelevantFlags = uint32(0x7fff)
)

type windowsBackend struct {
	handle    windows.Handle // immutable until active I/O and callbacks have settled
	calls     windowsIOCalls
	mu        sync.Mutex // protects closing and submission vs. close
	closing   bool
	active    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

type windowsIOCalls struct {
	createEvent func() (windows.Handle, error)
	read        func(windows.Handle, []byte, *uint32, *windows.Overlapped) error
	write       func(windows.Handle, []byte, *uint32, *windows.Overlapped) error
	result      func(windows.Handle, *windows.Overlapped, *uint32, bool) error
	cancel      func(windows.Handle, *windows.Overlapped) error
	close       func(windows.Handle) error
}

func newWindowsBackend(handle windows.Handle) *windowsBackend {
	return &windowsBackend{handle: handle, calls: windowsIOCalls{
		createEvent: func() (windows.Handle, error) { return windows.CreateEvent(nil, 1, 0, nil) },
		read:        windows.ReadFile, write: windows.WriteFile, result: windows.GetOverlappedResult,
		cancel: windows.CancelIoEx, close: windows.CloseHandle,
	}}
}

// Opening calls can be substituted to verify cleanup without a serial device.
type windowsOpenCalls struct {
	create      func(*uint16) (windows.Handle, error)
	getState    func(windows.Handle, *windows.DCB) error
	setState    func(windows.Handle, *windows.DCB) error
	setTimeouts func(windows.Handle, *windows.CommTimeouts) error
	close       func(windows.Handle) error
}

func openBackend(name string, cfg Config) (backend, error) {
	return openWindows(name, cfg, windowsOpenCalls{
		create: func(path *uint16) (windows.Handle, error) {
			return windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE,
				0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
		},
		getState: windows.GetCommState, setState: windows.SetCommState,
		setTimeouts: windows.SetCommTimeouts, close: windows.CloseHandle,
	})
}

func openWindows(name string, cfg Config, calls windowsOpenCalls) (_ backend, err error) {
	name, err = windowsPortName(name)
	if err != nil {
		return nil, notStarted(err)
	}
	if cfg.FlowControl != FlowControlNone {
		return nil, notStarted(fmt.Errorf("%w: flow control %s", ErrNotSupported, cfg.FlowControl))
	}
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, notStarted(err)
	}
	handle, err := calls.create(path)
	if err != nil {
		return nil, fmt.Errorf("CreateFile: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, calls.close(handle))
		}
	}()
	state := windows.DCB{DCBlength: uint32(unsafe.Sizeof(windows.DCB{}))}
	if err = calls.getState(handle, &state); err != nil {
		return nil, fmt.Errorf("GetCommState: %w", err)
	}
	// Explicit binary mode, no NUL/error-character substitution, no inherited
	// XON/XOFF, CTS/DSR gating, abort-on-error or dynamic modem handshakes.
	state.Flags = dcbBinary
	if cfg.Parity != ParityNone {
		state.Flags |= dcbParity
	}
	if cfg.DTR {
		state.Flags |= dcbDTR
	}
	if cfg.RTS {
		state.Flags |= dcbRTS
	}
	state.BaudRate, state.ByteSize = uint32(cfg.BaudRate), byte(cfg.DataBits)
	switch cfg.Parity {
	case ParityNone:
		state.Parity = windows.NOPARITY
	case ParityOdd:
		state.Parity = windows.ODDPARITY
	case ParityEven:
		state.Parity = windows.EVENPARITY
	case ParityMark:
		state.Parity = windows.MARKPARITY
	case ParitySpace:
		state.Parity = windows.SPACEPARITY
	}
	switch cfg.StopBits {
	case OneStopBit:
		state.StopBits = windows.ONESTOPBIT
	case OnePointFiveStopBits:
		state.StopBits = windows.ONE5STOPBITS
	case TwoStopBits:
		state.StopBits = windows.TWOSTOPBITS
	}
	wanted := state
	if err = calls.setState(handle, &state); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			err = errors.Join(ErrNotSupported, err)
		}
		return nil, fmt.Errorf("SetCommState: %w", err)
	}
	state = windows.DCB{DCBlength: uint32(unsafe.Sizeof(windows.DCB{}))}
	if err = calls.getState(handle, &state); err != nil {
		return nil, fmt.Errorf("GetCommState (verify): %w", err)
	}
	if state.BaudRate != wanted.BaudRate || state.ByteSize != wanted.ByteSize ||
		state.Parity != wanted.Parity || state.StopBits != wanted.StopBits ||
		state.Flags&dcbRelevantFlags != wanted.Flags&dcbRelevantFlags {
		return nil, fmt.Errorf("%w: driver did not retain requested DCB settings", ErrNotSupported)
	}
	// Special first-byte mode: return buffered bytes immediately, otherwise
	// return when a byte arrives. The 50ms heartbeat is NOT the API budget;
	// empty completions are retried by Port. Writes have no driver timeout.
	timeouts := windows.CommTimeouts{
		ReadIntervalTimeout: ^uint32(0), ReadTotalTimeoutMultiplier: ^uint32(0),
		ReadTotalTimeoutConstant: 50,
	}
	if err = calls.setTimeouts(handle, &timeouts); err != nil {
		return nil, fmt.Errorf("SetCommTimeouts: %w", err)
	}
	return newWindowsBackend(handle), nil
}

func (b *windowsBackend) read(ctx context.Context, buf []byte) (int, error) {
	return b.transfer(ctx, buf, false)
}
func (b *windowsBackend) write(ctx context.Context, buf []byte) (int, error) {
	return b.transfer(ctx, buf, true)
}

func (b *windowsBackend) transfer(ctx context.Context, buf []byte, write bool) (int, error) {
	// ReadFile/WriteFile take a DWORD length. Bounded requests also avoid
	// unusually large driver allocations; Port accumulates short writes.
	if len(buf) > 1<<20 {
		buf = buf[:1<<20]
	}
	event, err := b.calls.createEvent()
	if err != nil {
		return 0, notStarted(fmt.Errorf("CreateEvent: %w", err))
	}
	overlap := &windows.Overlapped{HEvent: event}
	// Keep buffers and OVERLAPPED alive until the kernel has completed the
	// request. Returning on context alone would violate caller buffer ownership.
	defer runtime.KeepAlive(buf)
	defer runtime.KeepAlive(overlap)
	var transferred uint32
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		b.calls.close(event)
		return 0, notStarted(ErrClosed)
	}
	if err = ctx.Err(); err != nil {
		b.mu.Unlock()
		b.calls.close(event)
		return 0, notStarted(err)
	}
	b.active.Add(1)
	// Add and issue under the same lock used by Close. This prevents a new
	// request from being submitted after Close's CancelIoEx(handle, nil).
	if write {
		err = b.calls.write(b.handle, buf, &transferred, overlap)
	} else {
		err = b.calls.read(b.handle, buf, &transferred, overlap)
	}
	b.mu.Unlock()
	defer b.active.Done()
	defer b.calls.close(event)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		callbackDone := make(chan struct{})
		var cancelErr error
		// Register AFTER submission: a cancellation before submission cannot
		// miss a request. Each callback targets this unique OVERLAPPED only.
		stop := context.AfterFunc(ctx, func() {
			cancelErr = b.calls.cancel(b.handle, overlap)
			close(callbackDone)
		})
		err = b.calls.result(b.handle, overlap, &transferred, true)
		if !stop() {
			<-callbackDone
		}
		if cancelErr != nil && !errors.Is(cancelErr, windows.ERROR_NOT_FOUND) && err != nil {
			err = errors.Join(err, fmt.Errorf("CancelIoEx: %w", cancelErr))
		}
	}
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		b.mu.Lock()
		closing := b.closing
		b.mu.Unlock()
		if closing {
			err = errors.Join(ErrClosed, err)
		} else if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
	}
	return int(transferred), classifyWindowsIOError(err)
}

func (b *windowsBackend) close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		err := b.calls.cancel(b.handle, nil)
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			err = nil
		}
		b.mu.Unlock()
		// Cancellation is requested before waiting; no read/write direction
		// gate is needed. The handle is retained until all callbacks are joined.
		b.active.Wait()
		b.closeErr = errors.Join(err, b.calls.close(b.handle))
	})
	return b.closeErr
}

func classifyWindowsIOError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_INVALID_HANDLE), errors.Is(err, windows.ERROR_DEVICE_NOT_CONNECTED),
		errors.Is(err, windows.ERROR_NO_SUCH_DEVICE), errors.Is(err, windows.ERROR_DEV_NOT_EXIST), errors.Is(err, windows.ERROR_BROKEN_PIPE):
		return errors.Join(ErrDisconnected, err)
	case errors.Is(err, windows.ERROR_SEM_TIMEOUT), errors.Is(err, windows.ERROR_TIMEOUT):
		return &timeoutError{err}
	default:
		return err
	}
}
