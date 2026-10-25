//go:build windows

package asrl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsOpenFailureReleasesHandle(t *testing.T) {
	for _, stage := range []string{"create", "get", "set", "verify", "coercion", "timeouts"} {
		t.Run(stage, func(t *testing.T) {
			cause := windows.ERROR_ACCESS_DENIED
			closed, gets, created := 0, 0, 0
			var configured windows.DCB
			calls := windowsOpenCalls{
				create: func(*uint16) (windows.Handle, error) {
					created++
					if stage == "create" {
						return windows.InvalidHandle, cause
					}
					return 42, nil
				},
				getState: func(_ windows.Handle, s *windows.DCB) error {
					gets++
					if (stage == "get" && gets == 1) || (stage == "verify" && gets == 2) {
						return cause
					}
					if gets == 1 {
						s.Flags = ^uint32(0)
					} else {
						*s = configured
						if stage == "coercion" {
							s.BaudRate = 123
						}
					}
					return nil
				},
				setState: func(_ windows.Handle, s *windows.DCB) error {
					configured = *s
					if stage == "set" {
						return windows.ERROR_INVALID_PARAMETER
					}
					return nil
				},
				setTimeouts: func(windows.Handle, *windows.CommTimeouts) error { return cause },
				close: func(h windows.Handle) error {
					if h != 42 {
						t.Errorf("closed wrong handle: %d", h)
					}
					closed++
					return nil
				},
			}
			dev, err := openWindows("COM10", DefaultConfig(), calls)
			if dev != nil || err == nil {
				t.Fatalf("%v %v", dev, err)
			}
			if stage == "set" {
				if !errors.Is(err, ErrNotSupported) || !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
					t.Fatal(err)
				}
			} else if stage == "coercion" {
				if !errors.Is(err, ErrNotSupported) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, cause) {
				t.Fatal(err)
			}
			wantClosed := 1
			if stage == "create" {
				wantClosed = 0
			}
			if created != 1 || closed != wantClosed {
				t.Fatalf("created=%d closed=%d", created, closed)
			}
			if gets > 1 && (configured.Flags != dcbBinary || configured.ByteSize != 8 || configured.BaudRate != 9600 || configured.DCBlength != uint32(unsafe.Sizeof(windows.DCB{}))) {
				t.Fatalf("not raw 9600/8N1: %+v", configured)
			}
		})
	}
}

func TestWindowsConfigurationMapping(t *testing.T) {
	for _, parity := range []Parity{ParityNone, ParityOdd, ParityEven, ParityMark, ParitySpace} {
		for _, bits := range []int{5, 6, 7, 8} {
			for _, stop := range []StopBits{OneStopBit, OnePointFiveStopBits, TwoStopBits} {
				cfg := DefaultConfig()
				cfg.Parity, cfg.DataBits, cfg.StopBits = parity, bits, stop
				cfg.DTR, cfg.RTS = true, true
				cfg, err := cfg.Normalize()
				if err != nil {
					continue
				}
				var state windows.DCB
				var timeouts windows.CommTimeouts
				gets := 0
				calls := windowsOpenCalls{
					create: func(*uint16) (windows.Handle, error) { return 42, nil },
					getState: func(_ windows.Handle, s *windows.DCB) error {
						gets++
						if gets == 2 {
							*s = state
						}
						return nil
					},
					setState:    func(_ windows.Handle, s *windows.DCB) error { state = *s; return nil },
					setTimeouts: func(_ windows.Handle, t *windows.CommTimeouts) error { timeouts = *t; return nil },
					close:       func(windows.Handle) error { t.Fatal("unexpected cleanup of successful open"); return nil },
				}
				_, err = openWindows("COM10", cfg, calls)
				if err != nil {
					t.Fatalf("%+v: %v", cfg, err)
				}
				wantFlags := dcbBinary | dcbDTR | dcbRTS
				if parity != ParityNone {
					wantFlags |= dcbParity
				}
				wantParity := map[Parity]byte{ParityNone: windows.NOPARITY, ParityOdd: windows.ODDPARITY, ParityEven: windows.EVENPARITY, ParityMark: windows.MARKPARITY, ParitySpace: windows.SPACEPARITY}[parity]
				wantStop := map[StopBits]byte{OneStopBit: windows.ONESTOPBIT, OnePointFiveStopBits: windows.ONE5STOPBITS, TwoStopBits: windows.TWOSTOPBITS}[stop]
				if state.Flags != wantFlags || state.ByteSize != byte(bits) || state.Parity != wantParity || state.StopBits != wantStop {
					t.Fatalf("incorrect mapping: %+v", state)
				}
				if timeouts.ReadIntervalTimeout != ^uint32(0) || timeouts.ReadTotalTimeoutMultiplier != ^uint32(0) || timeouts.ReadTotalTimeoutConstant != 50 || timeouts.WriteTotalTimeoutConstant != 0 || timeouts.WriteTotalTimeoutMultiplier != 0 {
					t.Fatalf("incorrect timeout mode: %+v", timeouts)
				}
			}
		}
	}
}

func TestWindowsRejectsUnsupportedBeforeOpen(t *testing.T) {
	calls := windowsOpenCalls{create: func(*uint16) (windows.Handle, error) { t.Fatal("touched device"); return 0, nil }}
	for _, flow := range []FlowControl{FlowControlRTSCTS, FlowControlXONXOFF} {
		cfg := DefaultConfig()
		cfg.FlowControl = flow
		if _, err := openWindows("COM2", cfg, calls); !errors.Is(err, ErrNotSupported) || !isUnstarted(err) {
			t.Fatal(err)
		}
	}
}

func TestWindowsErrorClassificationKeepsOSCause(t *testing.T) {
	for _, cause := range []error{windows.ERROR_INVALID_HANDLE, windows.ERROR_DEVICE_NOT_CONNECTED, windows.ERROR_NO_SUCH_DEVICE, windows.ERROR_DEV_NOT_EXIST, windows.ERROR_BROKEN_PIPE} {
		err := classifyWindowsIOError(cause)
		if !errors.Is(err, ErrDisconnected) || !errors.Is(err, cause) {
			t.Fatal(err)
		}
	}
	for _, cause := range []error{windows.ERROR_SEM_TIMEOUT, windows.ERROR_TIMEOUT} {
		err := classifyWindowsIOError(cause)
		if !errors.Is(err, ErrTimeout) || !errors.Is(err, cause) {
			t.Fatal(err)
		}
	}
	for _, cause := range []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_FILE_NOT_FOUND, windows.ERROR_OPERATION_ABORTED} {
		err := classifyWindowsIOError(cause)
		if !errors.Is(err, cause) || errors.Is(err, ErrDisconnected) || errors.Is(err, ErrTimeout) {
			t.Fatal(err)
		}
	}
}

var pipeSequence atomic.Uint64

// Native Windows kernel integration, no COM ports: both endpoints use exactly
// the production transfer/CancelIoEx/close path. This does NOT test serial DCB,
// UART drivers, COMMTIMEOUTS, electrical line levels or a physical router.
func nativePipePair(t *testing.T) (*Port, *Port) {
	t.Helper()
	name := fmt.Sprintf(`\\.\pipe\go-asrl-test-%d-%d`, os.Getpid(), pipeSequence.Add(1))
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	server, err := windows.CreateNamedPipe(path, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 4096, 4096, 0, nil)
	if err != nil {
		nativePipeError(t, "CreateNamedPipe", err)
	}
	t.Cleanup(func() {
		if server != windows.InvalidHandle {
			windows.CloseHandle(server)
		}
	})
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	overlap := &windows.Overlapped{HEvent: event}
	err = windows.ConnectNamedPipe(server, overlap)
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		t.Fatalf("ConnectNamedPipe: %v", err)
	}
	client, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		windows.CancelIoEx(server, overlap)
		var n uint32
		windows.GetOverlappedResult(server, overlap, &n, true)
		nativePipeError(t, "CreateFile (pipe client)", err)
	}
	var n uint32
	if err = windows.GetOverlappedResult(server, overlap, &n, true); err != nil {
		windows.CloseHandle(client)
		t.Fatal(err)
	}
	a := newPort("native-pipe-server", 0, newWindowsBackend(server))
	b := newPort("native-pipe-client", time.Second, newWindowsBackend(client))
	// Only the owning backends close handles after construction.
	// The earlier cleanup is guarded by invalidating its captured local value.
	server = windows.InvalidHandle
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func nativePipeError(t *testing.T, op string, err error) {
	t.Helper()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) && os.Getenv("ASRL_REQUIRE_NATIVE_IO") != "1" {
		t.Skipf("%s denied in restricted environment: %v; set ASRL_REQUIRE_NATIVE_IO=1 to require native I/O", op, err)
	}
	t.Fatalf("%s: %v", op, err)
}

// Observe registration of cancellation AFTER a real pending kernel request.
// Hiding Value prevents context's internal cancelCtx optimization from bypassing
// the test's AfterFunc observer; cancellation still belongs to the real context.
type pendingContext struct {
	context.Context
	registered chan struct{}
	once       sync.Once
}

func (c *pendingContext) Value(any) any { return nil }
func (c *pendingContext) AfterFunc(fn func()) func() bool {
	stop := context.AfterFunc(c.Context, fn)
	c.once.Do(func() { close(c.registered) })
	return stop
}
func observePending(ctx context.Context) *pendingContext {
	return &pendingContext{Context: ctx, registered: make(chan struct{})}
}

func TestWindowsNativeCancelReadAndReuse(t *testing.T) {
	p, peer := nativePipePair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := observePending(ctx)
	buf := make([]byte, 64)
	result := resultOf(func() (int, error) { return p.ReadContext(observed, buf) })
	awaitSignal(t, observed.registered)
	cancel()
	r := awaitResult(t, result)
	if !errors.Is(r.err, context.Canceled) || isUnstarted(r.err) {
		t.Fatalf("%+v", r)
	}
	// Buffer ownership is returned, and the same open handle remains useful.
	for i := range buf {
		buf[i] = 0xcc
	}
	data := []byte{0, '\r', '\n', 0x80, 0xff}
	if n, err := peer.Write(data); n != len(data) || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	p.SetTimeout(time.Second)
	if n, err := p.Read(buf); n != len(data) || err != nil || !bytes.Equal(buf[:n], data) {
		t.Fatalf("%d %x %v", n, buf, err)
	}
}

func TestWindowsNativeDeadlineAndNoStaleCallback(t *testing.T) {
	p, peer := nativePipePair(t)
	p.SetTimeout(40 * time.Millisecond)
	_, err := p.Read(make([]byte, 1))
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	p.SetTimeout(0)
	for range 10 {
		ctx, cancel := context.WithCancel(context.Background())
		observed := observePending(ctx)
		buf := make([]byte, 8)
		r := resultOf(func() (int, error) { return p.ReadContext(observed, buf) })
		awaitSignal(t, observed.registered)
		if _, err := peer.Write([]byte{42}); err != nil {
			t.Fatal(err)
		}
		if got := awaitResult(t, r); got.n != 1 || got.err != nil {
			t.Fatalf("%+v", got)
		}
		cancel() // cancellation of a completed call must not reach the next one
	}
}

func TestWindowsNativeReadAndWriteAreIndependent(t *testing.T) {
	p, peer := nativePipePair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := observePending(ctx)
	r := resultOf(func() (int, error) { return p.ReadContext(observed, make([]byte, 8)) })
	awaitSignal(t, observed.registered)
	if n, err := p.WriteContext(context.Background(), []byte("input")); n != 5 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	buf := make([]byte, 8)
	if n, err := peer.Read(buf); n != 5 || err != nil || string(buf[:n]) != "input" {
		t.Fatalf("%d %q %v", n, buf, err)
	}
	cancel()
	if got := awaitResult(t, r); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("%+v", got)
	}
}

func TestWindowsNativeCancelBlockedWrite(t *testing.T) {
	p, _ := nativePipePair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := observePending(ctx)
	buf := make([]byte, 2<<20)
	w := resultOf(func() (int, error) { return p.WriteContext(observed, buf) })
	awaitSignal(t, observed.registered)
	cancel()
	if r := awaitResult(t, w); !errors.Is(r.err, context.Canceled) || isUnstarted(r.err) || r.n >= len(buf) {
		t.Fatalf("%+v", r)
	}
	for i := range buf {
		buf[i] = 0xaa
	} // safe to reuse caller storage on return
}

func TestWindowsNativeCloseInterruptsReadAndWrite(t *testing.T) {
	p, _ := nativePipePair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readCtx, writeCtx := observePending(ctx), observePending(ctx)
	r := resultOf(func() (int, error) { return p.ReadContext(readCtx, make([]byte, 8)) })
	awaitSignal(t, readCtx.registered)
	w := resultOf(func() (int, error) { return p.WriteContext(writeCtx, make([]byte, 2<<20)) })
	awaitSignal(t, writeCtx.registered)
	closed := resultOf(func() (int, error) { return 0, p.Close() })
	if got := awaitResult(t, closed); got.err != nil {
		t.Fatal(got.err)
	}
	for _, ch := range []<-chan ioResult{r, w} {
		if got := awaitResult(t, ch); !errors.Is(got.err, ErrClosed) {
			t.Fatalf("%+v", got)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
