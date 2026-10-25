//go:build windows

package asrl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A deterministic kernel model checks ownership and ordering even in a sandbox
// that denies named-pipe clients. These are simulated system calls, not native
// Windows I/O evidence.
type modeledRequest struct {
	overlap *windows.Overlapped
	buf     []byte
	write   bool
	done    chan struct{}
	n       uint32
	err     error
	settled bool
}
type modeledWindows struct {
	t              *testing.T
	mu             sync.Mutex
	requests       map[*windows.Overlapped]*modeledRequest
	submitted      chan *modeledRequest
	cancelEntered  chan struct{}
	cancelRelease  <-chan struct{}
	submitEntered  chan struct{}
	submitRelease  <-chan struct{}
	resultReturned chan struct{}
	callbacks      int
	callbackEvents map[windows.Handle]int
	eventSequence  windows.Handle
	portClosed     bool
	cancelCount    int
}

func newModeledWindows(t *testing.T) *modeledWindows {
	return &modeledWindows{t: t, requests: make(map[*windows.Overlapped]*modeledRequest), submitted: make(chan *modeledRequest, 8), callbackEvents: make(map[windows.Handle]int)}
}

func (m *modeledWindows) calls() windowsIOCalls {
	submit := func(write bool) func(windows.Handle, []byte, *uint32, *windows.Overlapped) error {
		return func(_ windows.Handle, p []byte, _ *uint32, o *windows.Overlapped) error {
			if m.submitEntered != nil {
				close(m.submitEntered)
				<-m.submitRelease
			}
			m.mu.Lock()
			if m.portClosed {
				m.t.Error("submitted I/O against a closed handle")
			}
			r := &modeledRequest{overlap: o, buf: p, write: write, done: make(chan struct{})}
			m.requests[o] = r
			m.mu.Unlock()
			m.submitted <- r
			return windows.ERROR_IO_PENDING
		}
	}
	return windowsIOCalls{
		createEvent: func() (windows.Handle, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.eventSequence++
			return 100 + m.eventSequence, nil
		},
		read: submit(false), write: submit(true),
		result: func(_ windows.Handle, o *windows.Overlapped, n *uint32, _ bool) error {
			m.mu.Lock()
			r := m.requests[o]
			m.mu.Unlock()
			<-r.done
			m.mu.Lock()
			*n = r.n
			err := r.err
			m.mu.Unlock()
			if m.resultReturned != nil {
				close(m.resultReturned)
			}
			return err
		},
		cancel: func(_ windows.Handle, o *windows.Overlapped) error {
			m.mu.Lock()
			found := false
			if o != nil {
				m.callbacks++
				m.callbackEvents[o.HEvent]++
				m.cancelCount++
			}
			for key, r := range m.requests {
				if (o == nil || o == key) && !r.settled {
					r.settled = true
					r.err = windows.ERROR_OPERATION_ABORTED
					close(r.done)
					found = true
				}
			}
			m.mu.Unlock()
			if o != nil {
				if m.cancelEntered != nil {
					close(m.cancelEntered)
				}
				if m.cancelRelease != nil {
					<-m.cancelRelease
				}
				m.mu.Lock()
				m.callbacks--
				m.callbackEvents[o.HEvent]--
				m.mu.Unlock()
			}
			if !found {
				return windows.ERROR_NOT_FOUND
			}
			return nil
		},
		close: func(h windows.Handle) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			if (h == 42 && m.callbacks != 0) || m.callbackEvents[h] != 0 {
				m.t.Error("released event/handle while its cancellation callback was running")
			}
			for _, r := range m.requests {
				if (h == 42 || r.overlap.HEvent == h) && !r.settled {
					m.t.Error("released event/handle with pending I/O")
				}
			}
			if h == 42 {
				m.portClosed = true
			}
			return nil
		},
	}
}

func (m *modeledWindows) complete(r *modeledRequest, data []byte, n uint32, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.settled {
		m.t.Fatal("attempted late completion against returned buffer")
	}
	if !r.write {
		copy(r.buf, data)
	}
	r.n, r.err, r.settled = n, err, true
	close(r.done)
}

func modeledPort(t *testing.T, m *modeledWindows) *Port {
	return testPort(t, &windowsBackend{handle: 42, calls: m.calls()}, 0)
}

func awaitSubmission(t *testing.T, m *modeledWindows) *modeledRequest {
	t.Helper()
	select {
	case r := <-m.submitted:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no kernel request submitted")
		return nil
	}
}

func TestWindowsModelJoinsCancellationBeforeReleasingResources(t *testing.T) {
	m := newModeledWindows(t)
	release := make(chan struct{})
	m.cancelEntered = make(chan struct{})
	m.cancelRelease = release
	m.resultReturned = make(chan struct{})
	p := modeledPort(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := resultOf(func() (int, error) { return p.ReadContext(ctx, make([]byte, 8)) })
	awaitSubmission(t, m)
	cancel()
	awaitSignal(t, m.cancelEntered)
	awaitSignal(t, m.resultReturned)
	// The kernel result has settled, but the callback still owns OVERLAPPED.
	select {
	case got := <-r:
		t.Fatalf("returned before callback completed: %+v", got)
	default:
	}
	closed := resultOf(func() (int, error) { return 0, p.Close() })
	close(release)
	got := awaitResult(t, r)
	if !errors.Is(got.err, context.Canceled) && !errors.Is(got.err, ErrClosed) {
		t.Fatalf("%+v", got)
	}
	if result := awaitResult(t, closed); result.err != nil {
		t.Fatal(result.err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.portClosed || m.callbacks != 0 {
		t.Fatal("resources not settled")
	}
}

func TestWindowsModelCloseCannotMissSubmission(t *testing.T) {
	m := newModeledWindows(t)
	release := make(chan struct{})
	m.submitEntered = make(chan struct{})
	m.submitRelease = release
	p := modeledPort(t, m)
	r := resultOf(func() (int, error) { return p.Read(make([]byte, 8)) })
	awaitSignal(t, m.submitEntered)
	closeStarted := make(chan struct{})
	closed := resultOf(func() (int, error) { close(closeStarted); return 0, p.Close() })
	awaitSignal(t, closeStarted)
	close(release)
	if got := awaitResult(t, r); !errors.Is(got.err, ErrClosed) {
		t.Fatalf("%+v", got)
	}
	if got := awaitResult(t, closed); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestWindowsModelOldCancelCannotReachNextRead(t *testing.T) {
	m := newModeledWindows(t)
	p := modeledPort(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := resultOf(func() (int, error) { return p.ReadContext(ctx, make([]byte, 8)) })
	r := awaitSubmission(t, m)
	m.complete(r, []byte{1}, 1, nil)
	if got := awaitResult(t, first); got.n != 1 || got.err != nil {
		t.Fatalf("%+v", got)
	}
	// Start the next call before cancelling the previous call's context.
	second := resultOf(func() (int, error) { return p.Read(make([]byte, 8)) })
	r = awaitSubmission(t, m)
	cancel()
	m.complete(r, []byte{2}, 1, nil)
	if got := awaitResult(t, second); got.n != 1 || got.err != nil {
		t.Fatalf("%+v", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelCount != 0 {
		t.Fatalf("obsolete callback ran %d times", m.cancelCount)
	}
}

func TestWindowsModelCancelledWriteKeepsTransferredPrefix(t *testing.T) {
	m := newModeledWindows(t)
	p := modeledPort(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := resultOf(func() (int, error) { return p.WriteContext(ctx, []byte("abcdef")) })
	r := awaitSubmission(t, m)
	m.mu.Lock()
	r.n = 2
	m.mu.Unlock()
	cancel()
	got := awaitResult(t, w)
	if got.n != 2 || !errors.Is(got.err, context.Canceled) || isUnstarted(got.err) {
		t.Fatalf("%+v", got)
	}
}

func TestWindowsModelCompletedSuccessWinsCancellationRace(t *testing.T) {
	m := newModeledWindows(t)
	p := modeledPort(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := resultOf(func() (int, error) { return p.WriteContext(ctx, []byte{42}) })
	r := awaitSubmission(t, m)
	m.complete(r, nil, 1, nil)
	cancel()
	if got := awaitResult(t, w); got.n != 1 || got.err != nil {
		t.Fatalf("completed write misreported: %+v", got)
	}
}

func TestWindowsModelReadCancellationLeavesWriteActive(t *testing.T) {
	m := newModeledWindows(t)
	p := modeledPort(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := resultOf(func() (int, error) { return p.ReadContext(ctx, make([]byte, 8)) })
	awaitSubmission(t, m)
	w := resultOf(func() (int, error) { return p.Write([]byte("out")) })
	writeRequest := awaitSubmission(t, m)
	cancel()
	if got := awaitResult(t, r); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("%+v", got)
	}
	m.complete(writeRequest, nil, 3, nil)
	if got := awaitResult(t, w); got.n != 3 || got.err != nil {
		t.Fatalf("unrelated write affected: %+v", got)
	}
}
