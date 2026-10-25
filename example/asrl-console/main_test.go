package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	asrl "github.com/jkbhuan/go-asrl"
)

type consoleFake struct {
	readFn  func(context.Context, []byte) (int, error)
	writeFn func(context.Context, []byte) (int, error)
	closed  chan struct{}
	once    sync.Once
}

func (p *consoleFake) ReadContext(ctx context.Context, b []byte) (int, error) {
	if p.readFn != nil {
		return p.readFn(ctx, b)
	}
	<-ctx.Done()
	return 0, ctx.Err()
}
func (p *consoleFake) WriteContext(ctx context.Context, b []byte) (int, error) {
	if p.writeFn != nil {
		return p.writeFn(ctx, b)
	}
	return len(b), nil
}
func (p *consoleFake) Close() error { p.once.Do(func() { close(p.closed) }); return nil }

func TestOptionsAndHelp(t *testing.T) {
	var help bytes.Buffer
	o, err := parseOptions(nil, &help)
	if err != nil || o.port != "COM2" || o.cfg != asrl.DefaultConfig() || o.interactive {
		t.Fatalf("%+v %v", o, err)
	}
	_, err = parseOptions([]string{"-h"}, &help)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "Receives only") || !strings.Contains(help.String(), "line-ending") {
		t.Fatalf("%s %v", help.String(), err)
	}
	for _, args := range [][]string{{"-line-ending", "auto"}, {"-data-bits", "9"}, {"-baud", "0"}, {"-timeout", "-1s"}, {"extra"}, {"-parity", "typo"}} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestReceivePreservesSegmentsAndContinuesAfterTimeout(t *testing.T) {
	for _, hex := range []bool{false, true} {
		p := &consoleFake{}
		calls := 0
		finished := errors.New("device stopped")
		p.readFn = func(_ context.Context, p []byte) (int, error) {
			calls++
			switch calls {
			case 1:
				return 0, asrl.ErrTimeout
			case 2:
				return copy(p, []byte{' ', 0, '\r'}), asrl.ErrTimeout
			default:
				return copy(p, []byte{0xff, '>'}), finished
			}
		}
		var out bytes.Buffer
		if err := receive(context.Background(), p, &out, hex); !errors.Is(err, finished) {
			t.Fatal(err)
		}
		want := string([]byte{' ', 0, '\r', 0xff, '>'})
		if hex {
			want = "20 00 0D\nFF 3E\n"
		}
		if out.String() != want {
			t.Fatalf("output changed: %q want %q", out.String(), want)
		}
	}
}

func TestInteractiveLineEndingsWithoutExtraEcho(t *testing.T) {
	for _, ending := range []string{"cr", "lf", "crlf", "none"} {
		t.Run(ending, func(t *testing.T) {
			p := &consoleFake{closed: make(chan struct{})}
			var sent [][]byte
			p.writeFn = func(_ context.Context, b []byte) (int, error) {
				sent = append(sent, append([]byte{}, b...))
				return len(b), nil
			}
			var out bytes.Buffer
			err := session(context.Background(), p, strings.NewReader("\nhello \r\nlast"), &out, options{interactive: true, lineEnding: ending})
			if err != nil {
				t.Fatal(err)
			}
			suffix, _ := lineSuffix(ending)
			want := []string{suffix, "hello " + suffix, "last" + suffix}
			if len(sent) != len(want) {
				t.Fatalf("sent %q", sent)
			}
			for i := range want {
				if string(sent[i]) != want[i] {
					t.Fatalf("sent %q want %q", sent, want)
				}
			}
			if out.Len() != 0 {
				t.Fatalf("extra sent-text echo: %q", out.String())
			}
			select {
			case <-p.closed:
			default:
				t.Fatal("port not closed")
			}
		})
	}
}

type forbiddenInput struct{}

func (forbiddenInput) Read([]byte) (int, error) { panic("receive-only mode read stdin") }

func TestReceiveOnlyAndCancellationJoinSerialReader(t *testing.T) {
	p := &consoleFake{closed: make(chan struct{})}
	reading, settled := make(chan struct{}), make(chan struct{})
	p.readFn = func(ctx context.Context, _ []byte) (int, error) {
		close(reading)
		<-ctx.Done()
		close(settled)
		return 0, ctx.Err()
	}
	p.writeFn = func(context.Context, []byte) (int, error) {
		t.Error("receive-only mode wrote serial data")
		return 0, errors.New("unexpected write")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session(ctx, p, forbiddenInput{}, io.Discard, options{lineEnding: "cr"}) }()
	select {
	case <-reading:
	case <-time.After(3 * time.Second):
		t.Fatal("reader not started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit")
	}
	select {
	case <-settled:
	default:
		t.Fatal("serial reader left running")
	}
}

func TestCancelDuringWriteSettlesSession(t *testing.T) {
	p := &consoleFake{closed: make(chan struct{})}
	writing := make(chan struct{})
	p.writeFn = func(ctx context.Context, _ []byte) (int, error) { close(writing); <-ctx.Done(); return 0, ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- session(ctx, p, strings.NewReader("hello\n"), io.Discard, options{interactive: true, lineEnding: "cr"})
	}()
	select {
	case <-writing:
	case <-time.After(3 * time.Second):
		t.Fatal("write not started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled write did not settle")
	}
}
