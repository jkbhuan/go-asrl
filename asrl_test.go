package asrl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	readFn  func(context.Context, []byte) (int, error)
	writeFn func(context.Context, []byte) (int, error)
	closeFn func() error
	closed  chan struct{}
	once    sync.Once
}

func newFake() *fakeBackend { return &fakeBackend{closed: make(chan struct{})} }
func (b *fakeBackend) read(ctx context.Context, p []byte) (int, error) {
	if b.readFn != nil {
		return b.readFn(ctx, p)
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-b.closed:
		return 0, ErrClosed
	}
}
func (b *fakeBackend) write(ctx context.Context, p []byte) (int, error) {
	if b.writeFn != nil {
		return b.writeFn(ctx, p)
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-b.closed:
		return 0, ErrClosed
	}
}
func (b *fakeBackend) close() error {
	b.once.Do(func() { close(b.closed) })
	if b.closeFn != nil {
		return b.closeFn()
	}
	return nil
}

type ioResult struct {
	n   int
	err error
}

func resultOf(fn func() (int, error)) <-chan ioResult {
	ch := make(chan ioResult, 1)
	go func() { n, err := fn(); ch <- ioResult{n, err} }()
	return ch
}
func awaitResult(t *testing.T, ch <-chan ioResult) ioResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not settle")
		return ioResult{}
	}
}
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not reach synchronization point")
	}
}
func isUnstarted(err error) bool {
	var marker interface{ OperationNotStarted() bool }
	return errors.As(err, &marker) && marker.OperationNotStarted()
}
func testPort(t *testing.T, b backend, timeout time.Duration) *Port {
	t.Helper()
	p := newPort("fake-port", timeout, b)
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p
}

func TestRawBytesAndSegmentedReads(t *testing.T) {
	b := newFake()
	chunks := [][]byte{{'\r', '\n'}, {0, 0x80, 0xff, '>'}}
	b.readFn = func(_ context.Context, p []byte) (int, error) {
		if len(chunks) == 0 {
			return 0, io.EOF
		}
		n := copy(p, chunks[0])
		chunks = chunks[1:]
		return n, nil
	}
	p := testPort(t, b, time.Second)
	buf := make([]byte, 128)
	var all []byte
	for _, wantN := range []int{2, 4} {
		n, err := p.Read(buf)
		if n != wantN || err != nil {
			t.Fatalf("Read = %d, %v", n, err)
		}
		all = append(all, buf[:n]...)
	}
	if !bytes.Equal(all, []byte{'\r', '\n', 0, 0x80, 0xff, '>'}) {
		t.Fatalf("changed raw bytes: %x", all)
	}
}

func TestShortWritesAndPartialErrors(t *testing.T) {
	for _, failAfter := range []int{-1, 0, 2} {
		t.Run(string(rune('b'+failAfter)), func(t *testing.T) {
			b := newFake()
			var accepted []byte
			cause := errors.New("driver rejected remainder")
			b.writeFn = func(_ context.Context, data []byte) (int, error) {
				n := min(2, len(data))
				if len(accepted) == failAfter {
					accepted = append(accepted, data[:1]...)
					return 1, cause
				}
				accepted = append(accepted, data[:n]...)
				return n, nil
			}
			p := testPort(t, b, time.Second)
			data := []byte{'\r', '\n', 0, 0x80, 0xff, 'x', 'y'}
			n, err := p.Write(data)
			want := len(data)
			if failAfter >= 0 {
				want = failAfter + 1
				if !errors.Is(err, cause) {
					t.Fatalf("lost cause: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if n != want || !bytes.Equal(accepted, data[:want]) {
				t.Fatalf("Write = %d %v, data=%x", n, err, accepted)
			}
			if err != nil && isUnstarted(err) {
				t.Fatal("partial I/O marked unstarted")
			}
		})
	}
}

func TestReadPreservesDataWithError(t *testing.T) {
	cause := errors.New("read device failure")
	b := newFake()
	b.readFn = func(_ context.Context, p []byte) (int, error) { return copy(p, []byte{0, 0xff}), cause }
	p := testPort(t, b, 0)
	buf := make([]byte, 8)
	n, err := p.Read(buf)
	if n != 2 || !bytes.Equal(buf[:n], []byte{0, 0xff}) || !errors.Is(err, cause) {
		t.Fatalf("%d %x %v", n, buf, err)
	}
	var op *OpError
	if !errors.As(err, &op) || op.Op != "read" || op.Port != "fake-port" {
		t.Fatalf("missing diagnostic context: %v", err)
	}
}

func TestZeroLengthIsAlwaysNoOp(t *testing.T) {
	b := newFake()
	b.readFn = func(context.Context, []byte) (int, error) { t.Fatal("unexpected read"); return 0, nil }
	b.writeFn = func(context.Context, []byte) (int, error) { t.Fatal("unexpected write"); return 0, nil }
	p := testPort(t, b, 0)
	p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, fn := range []func() (int, error){func() (int, error) { return p.Read(nil) }, func() (int, error) { return p.Write([]byte{}) }, func() (int, error) { return p.ReadContext(ctx, nil) }, func() (int, error) { return p.WriteContext(ctx, nil) }, func() (int, error) { return p.ReadWithDeadline(ctx, nil, time.Now().Add(-time.Second)) }} {
		if n, err := fn(); n != 0 || err != nil {
			t.Fatalf("empty operation = %d %v", n, err)
		}
	}
}

func TestNoProgressDoesNotSpinOrReturnEmptySuccess(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[write], func(t *testing.T) {
			b := newFake()
			var calls atomic.Int32
			fn := func(context.Context, []byte) (int, error) { calls.Add(1); return 0, nil }
			b.readFn, b.writeFn = fn, fn
			p := testPort(t, b, 60*time.Millisecond)
			var n int
			var err error
			if write {
				n, err = p.Write([]byte{1})
			} else {
				n, err = p.Read(make([]byte, 1))
			}
			if n != 0 || !errors.Is(err, ErrTimeout) || calls.Load() > 100 {
				t.Fatalf("n=%d err=%v calls=%d", n, err, calls.Load())
			}
			if isUnstarted(err) {
				t.Fatal("a backend call occurred")
			}
		})
	}
}

func TestCancelledAndExpiredEntryDoesNotTouchBackend(t *testing.T) {
	b := newFake()
	var calls atomic.Int32
	fn := func(context.Context, []byte) (int, error) { calls.Add(1); return 0, errors.New("unexpected I/O") }
	b.readFn, b.writeFn = fn, fn
	p := testPort(t, b, 0)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, ctx := range []context.Context{cancelled, expired} {
		for _, fn := range []func() (int, error){func() (int, error) { return p.ReadContext(ctx, make([]byte, 1)) }, func() (int, error) { return p.WriteContext(ctx, []byte{1}) }} {
			n, err := fn()
			if n != 0 || !errors.Is(err, ctx.Err()) || !isUnstarted(err) {
				t.Fatalf("%d %v", n, err)
			}
		}
	}
	_, err := p.ReadWithDeadline(context.Background(), make([]byte, 1), time.Now().Add(-time.Second))
	if !errors.Is(err, ErrTimeout) || !isUnstarted(err) || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestBudgetUsesEarlierDeadline(t *testing.T) {
	for _, defaultEarlier := range []bool{false, true} {
		t.Run(map[bool]string{true: "default", false: "context"}[defaultEarlier], func(t *testing.T) {
			b := newFake()
			var captured time.Time
			b.readFn = func(ctx context.Context, _ []byte) (int, error) {
				captured, _ = ctx.Deadline()
				<-ctx.Done()
				return 0, ctx.Err()
			}
			budget, ctxBudget := 80*time.Millisecond, 2*time.Second
			if !defaultEarlier {
				budget, ctxBudget = ctxBudget, budget
			}
			p := testPort(t, b, budget)
			ctx, cancel := context.WithTimeout(context.Background(), ctxBudget)
			defer cancel()
			start := time.Now()
			_, err := p.ReadContext(ctx, make([]byte, 1))
			if captured.Sub(start) > 150*time.Millisecond || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrTimeout) || !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("deadline=%v err=%v", captured.Sub(start), err)
			}
			var timeout interface{ Timeout() bool }
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatal("not a timeout error")
			}
		})
	}
}

func TestQueuedBudgetAndCancellationAreUnstarted(t *testing.T) {
	for _, write := range []bool{false, true} {
		for _, cancelQueued := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "write", false: "read"}[write], map[bool]string{true: "cancel", false: "timeout"}[cancelQueued]}, "/"), func(t *testing.T) {
				b := newFake()
				entered := make(chan struct{})
				var calls atomic.Int32
				fn := func(ctx context.Context, _ []byte) (int, error) {
					if calls.Add(1) == 1 {
						close(entered)
					}
					<-ctx.Done()
					return 0, ctx.Err()
				}
				b.readFn, b.writeFn = fn, fn
				p := testPort(t, b, 0)
				firstCtx, cancelFirst := context.WithCancel(context.Background())
				defer cancelFirst()
				call := func(ctx context.Context) (int, error) {
					if write {
						return p.WriteContext(ctx, []byte{1})
					}
					return p.ReadContext(ctx, make([]byte, 1))
				}
				first := resultOf(func() (int, error) { return call(firstCtx) })
				awaitSignal(t, entered)
				p.SetTimeout(80 * time.Millisecond)
				queuedCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				second := resultOf(func() (int, error) { return call(queuedCtx) })
				if cancelQueued {
					cancel()
				}
				r := awaitResult(t, second)
				want := ErrTimeout
				if cancelQueued {
					want = context.Canceled
				}
				if r.n != 0 || !errors.Is(r.err, want) || !isUnstarted(r.err) || calls.Load() != 1 {
					t.Fatalf("result=%+v calls=%d", r, calls.Load())
				}
				cancelFirst()
				awaitResult(t, first)
			})
		}
	}
}

func TestSetTimeoutDoesNotWaitOrChangeActiveBudget(t *testing.T) {
	b := newFake()
	deadlines := make(chan time.Time, 2)
	b.readFn = func(ctx context.Context, _ []byte) (int, error) {
		d, _ := ctx.Deadline()
		deadlines <- d
		<-ctx.Done()
		return 0, ctx.Err()
	}
	p := testPort(t, b, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := resultOf(func() (int, error) { return p.ReadContext(ctx, make([]byte, 1)) })
	if d := <-deadlines; !d.IsZero() {
		t.Fatal("expected unlimited original budget")
	}
	setterDone := make(chan struct{})
	go func() { p.SetTimeout(50 * time.Millisecond); close(setterDone) }()
	awaitSignal(t, setterDone)
	cancel()
	awaitResult(t, first)
	_, err := p.Read(make([]byte, 1))
	if !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	if d := <-deadlines; d.IsZero() {
		t.Fatal("new budget not applied")
	}
	for _, d := range []time.Duration{0, -time.Second} {
		p.SetTimeout(d)
		ctx, cancel := context.WithCancel(context.Background())
		next := resultOf(func() (int, error) { return p.ReadContext(ctx, make([]byte, 1)) })
		if got := <-deadlines; !got.IsZero() {
			t.Fatal("default not disabled")
		}
		cancel()
		awaitResult(t, next)
	}
}

func TestAbsoluteDeadlineOverridesDefault(t *testing.T) {
	for _, zero := range []bool{true, false} {
		t.Run(map[bool]string{true: "zero", false: "absolute"}[zero], func(t *testing.T) {
			b := newFake()
			deadlineSeen := make(chan time.Time, 1)
			release := make(chan struct{})
			b.readFn = func(ctx context.Context, p []byte) (int, error) {
				d, _ := ctx.Deadline()
				deadlineSeen <- d
				select {
				case <-release:
					p[0] = 42
					return 1, nil
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			}
			p := testPort(t, b, time.Nanosecond)
			deadline := time.Time{}
			if !zero {
				deadline = time.Now().Add(time.Hour)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := resultOf(func() (int, error) { return p.ReadWithDeadline(ctx, make([]byte, 1), deadline) })
			if seen := <-deadlineSeen; !seen.Equal(deadline) {
				t.Fatalf("got %v want %v", seen, deadline)
			}
			close(release)
			if got := awaitResult(t, r); got.n != 1 || got.err != nil {
				t.Fatalf("%+v", got)
			}
		})
	}
	b := newFake()
	p := testPort(t, b, time.Nanosecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.ReadWithDeadline(ctx, make([]byte, 1), time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTimeoutReadCanBeUsedAgain(t *testing.T) {
	b := newFake()
	calls := 0
	b.readFn = func(ctx context.Context, p []byte) (int, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		p[0] = '>'
		return 1, nil
	}
	p := testPort(t, b, 30*time.Millisecond)
	if _, err := p.Read(make([]byte, 1)); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if n, err := p.Read(buf); n != 1 || err != nil || buf[0] != '>' {
		t.Fatalf("%d %v %x", n, err, buf)
	}
}

func TestFullDuplexAndCloseInterruptsBothDirections(t *testing.T) {
	b := newFake()
	readStarted, writeStarted := make(chan struct{}), make(chan struct{})
	b.readFn = func(context.Context, []byte) (int, error) { close(readStarted); <-b.closed; return 0, ErrClosed }
	b.writeFn = func(context.Context, []byte) (int, error) { close(writeStarted); <-b.closed; return 0, ErrClosed }
	p := testPort(t, b, 0)
	r := resultOf(func() (int, error) { return p.Read(make([]byte, 1)) })
	awaitSignal(t, readStarted)
	w := resultOf(func() (int, error) { return p.Write([]byte{1}) })
	awaitSignal(t, writeStarted)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, ch := range []<-chan ioResult{r, w} {
		if got := awaitResult(t, ch); !errors.Is(got.err, ErrClosed) {
			t.Fatalf("%+v", got)
		}
	}
	for _, fn := range []func() (int, error){func() (int, error) { return p.Read(make([]byte, 1)) }, func() (int, error) { return p.Write([]byte{1}) }} {
		_, err := fn()
		if !errors.Is(err, ErrClosed) || !isUnstarted(err) {
			t.Fatal(err)
		}
	}
}

func TestWritesDoNotInterleaveAcrossShortWrites(t *testing.T) {
	b := newFake()
	first := make(chan struct{})
	release := make(chan struct{})
	var all []byte
	b.writeFn = func(_ context.Context, p []byte) (int, error) {
		if len(all) == 0 {
			close(first)
			<-release
		}
		all = append(all, p[0])
		return 1, nil
	}
	p := testPort(t, b, time.Second)
	a := resultOf(func() (int, error) { return p.Write([]byte("AAA")) })
	awaitSignal(t, first)
	z := resultOf(func() (int, error) { return p.Write([]byte("ZZZ")) })
	close(release)
	for _, ch := range []<-chan ioResult{a, z} {
		if r := awaitResult(t, ch); r.n != 3 || r.err != nil {
			t.Fatalf("%+v", r)
		}
	}
	if string(all) != "AAAZZZ" {
		t.Fatalf("interleaved writes: %q", all)
	}
}

func TestShortWriteBudgetIsNotReset(t *testing.T) {
	b := newFake()
	var original time.Time
	calls := 0
	b.writeFn = func(ctx context.Context, p []byte) (int, error) {
		d, _ := ctx.Deadline()
		calls++
		if calls == 1 {
			original = d
			return 1, nil
		}
		if !d.Equal(original) {
			return 0, errors.New("budget reset")
		}
		<-ctx.Done()
		return 1, ctx.Err()
	}
	p := testPort(t, b, 40*time.Millisecond)
	n, err := p.Write([]byte("abc"))
	if n != 2 || !errors.Is(err, ErrTimeout) || isUnstarted(err) {
		t.Fatalf("%d %v", n, err)
	}
}

func TestLaterUnstartedErrorCannotMarkPartialWriteUnstarted(t *testing.T) {
	b := newFake()
	calls := 0
	b.writeFn = func(context.Context, []byte) (int, error) {
		calls++
		if calls == 1 {
			return 1, nil
		}
		return 0, notStarted(context.Canceled)
	}
	p := testPort(t, b, 0)
	n, err := p.Write([]byte("ab"))
	if n != 1 || !errors.Is(err, context.Canceled) || isUnstarted(err) {
		t.Fatalf("%d %v", n, err)
	}
}

func TestCloseErrorIsStableAndKeepsCause(t *testing.T) {
	b := newFake()
	cause := errors.New("OS close failure")
	var count atomic.Int32
	b.closeFn = func() error { count.Add(1); return cause }
	p := newPort("test", 0, b)
	for range 3 {
		if !errors.Is(p.Close(), cause) {
			t.Fatal("lost close cause")
		}
	}
	if count.Load() != 1 {
		t.Fatalf("close count %d", count.Load())
	}
}

func TestConcurrentSetTimeout(t *testing.T) {
	b := newFake()
	b.writeFn = func(_ context.Context, p []byte) (int, error) { return len(p), nil }
	p := testPort(t, b, 0)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				if i%2 == 0 {
					p.SetTimeout(time.Duration(j) * time.Second)
				} else {
					p.Write([]byte{1})
				}
			}
		}()
	}
	wg.Wait()
}
