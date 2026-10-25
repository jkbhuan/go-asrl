// asrl-console receives raw serial output and optionally sends operator lines.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	asrl "github.com/jkbhuan/go-asrl"
)

type options struct {
	port             string
	cfg              asrl.Config
	interactive, hex bool
	lineEnding       string
}

func parseOptions(args []string, out io.Writer) (options, error) {
	o := options{cfg: asrl.DefaultConfig()}
	fs := flag.NewFlagSet("asrl-console", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "Usage: asrl-console [flags]\nReceives only by default; sends no probes or commands.\n8N1/no flow control and DTR/RTS off are initial suggestions; verify device requirements.")
		fs.PrintDefaults()
	}
	fs.StringVar(&o.port, "port", "COM2", "OS port name (COM2, COM10, or Windows device path)")
	fs.IntVar(&o.cfg.BaudRate, "baud", o.cfg.BaudRate, "baud rate")
	fs.IntVar(&o.cfg.DataBits, "data-bits", o.cfg.DataBits, "data bits (5..8)")
	parity, stop, flow := string(o.cfg.Parity), string(o.cfg.StopBits), string(o.cfg.FlowControl)
	fs.StringVar(&parity, "parity", parity, "none, odd, even, mark or space (driver dependent)")
	fs.StringVar(&stop, "stop-bits", stop, "1, 1.5 (5 data bits only), or 2 (6..8 data bits)")
	fs.StringVar(&flow, "flow-control", flow, "none (rtscts and xonxoff currently unsupported)")
	fs.DurationVar(&o.cfg.Timeout, "timeout", o.cfg.Timeout, "total I/O budget; 0 disables after open")
	fs.BoolVar(&o.cfg.DTR, "dtr", false, "assert initial DTR (verify device requirements)")
	fs.BoolVar(&o.cfg.RTS, "rts", false, "assert initial RTS (verify device requirements)")
	fs.BoolVar(&o.interactive, "interactive", false, "send operator input in line mode")
	fs.StringVar(&o.lineEnding, "line-ending", "cr", "interactive suffix: cr, lf, crlf, none (line mode, not raw keys)")
	fs.BoolVar(&o.hex, "hex", false, "display received bytes as hexadecimal")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	o.cfg.Parity, o.cfg.StopBits, o.cfg.FlowControl = asrl.Parity(strings.ToLower(parity)), asrl.StopBits(stop), asrl.FlowControl(strings.ToLower(flow))
	o.lineEnding = strings.ToLower(o.lineEnding)
	if _, err := lineSuffix(o.lineEnding); err != nil {
		return o, err
	}
	if o.cfg.BaudRate <= 0 || o.cfg.DataBits == 0 {
		return o, fmt.Errorf("baud and data-bits must be positive")
	}
	requestedTimeout := o.cfg.Timeout
	normalized, err := o.cfg.Normalize()
	if err != nil {
		return o, err
	}
	o.cfg = normalized
	o.cfg.Timeout = requestedTimeout
	return o, nil
}

func lineSuffix(name string) (string, error) {
	switch name {
	case "cr":
		return "\r", nil
	case "lf":
		return "\n", nil
	case "crlf":
		return "\r\n", nil
	case "none":
		return "", nil
	default:
		return "", fmt.Errorf("unknown line-ending %q (use cr, lf, crlf, none)", name)
	}
}

type consolePort interface {
	ReadContext(context.Context, []byte) (int, error)
	WriteContext(context.Context, []byte) (int, error)
	Close() error
}

type inputLine struct {
	text string
	err  error
}

func readInput(ctx context.Context, input io.Reader, lines chan<- inputLine) {
	defer close(lines)
	r := bufio.NewReader(input)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			// Only consume the local line delimiter, never trim user text.
			if strings.HasSuffix(line, "\n") {
				line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			}
			select {
			case lines <- inputLine{text: line}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case lines <- inputLine{err: err}:
			case <-ctx.Done():
			}
			return
		}
	}
}

func receive(ctx context.Context, port consolePort, out io.Writer, hex bool) error {
	buf := make([]byte, 4096)
	for {
		n, err := port.ReadContext(ctx, buf)
		if n > 0 {
			var outputErr error
			if hex {
				_, outputErr = fmt.Fprintf(out, "% X\n", buf[:n])
			} else {
				var written int
				written, outputErr = out.Write(buf[:n])
				if outputErr == nil && written != n {
					outputErr = io.ErrShortWrite
				}
			}
			if outputErr != nil {
				return outputErr
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, asrl.ErrTimeout) {
			return err
		}
	}
}

func session(ctx context.Context, port consolePort, input io.Reader, out io.Writer, o options) (result error) {
	ctx, cancel := context.WithCancel(ctx)
	received := make(chan error, 1)
	go func() { received <- receive(ctx, port, out, o.hex) }()
	readerJoined := false
	defer func() {
		cancel()
		closeErr := port.Close()
		if !readerJoined {
			<-received
		}
		result = errors.Join(result, closeErr)
	}()
	var lines chan inputLine
	if o.interactive {
		lines = make(chan inputLine)
		// This goroutine only reads local input. Serial writes run in this
		// function; all serial I/O is joined on exit. A terminal input read
		// may remain blocked until process exit, without owning the port.
		go readInput(ctx, input, lines)
	}
	suffix, _ := lineSuffix(o.lineEnding)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-received:
			readerJoined = true
			if ctx.Err() != nil {
				return nil
			}
			return err
		case line, ok := <-lines:
			if !ok || errors.Is(line.err, io.EOF) {
				return nil
			}
			if line.err != nil {
				return line.err
			}
			data := []byte(line.text + suffix)
			n, err := port.WriteContext(ctx, data)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("write accepted %d/%d bytes: %w", n, len(data), err)
			}
		}
	}
}

func run() error {
	o, err := parseOptions(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	port, err := asrl.Open(o.port, o.cfg)
	if err != nil {
		return err
	}

	port.SetTimeout(o.cfg.Timeout) // explicit CLI zero means unlimited
	fmt.Fprintf(os.Stderr, "Opened %s: baud=%d data-bits=%d parity=%s stop-bits=%s flow-control=%s timeout=%s DTR=%t RTS=%t interactive=%t hex=%t line-ending=%s\n",
		o.port, o.cfg.BaudRate, o.cfg.DataBits, o.cfg.Parity, o.cfg.StopBits, o.cfg.FlowControl, o.cfg.Timeout, o.cfg.DTR, o.cfg.RTS, o.interactive, o.hex, o.lineEnding)
	fmt.Fprintln(os.Stderr, "Initial settings beyond the known 9600 baud still need confirmation from the router manual. Ctrl+C exits. No commands are sent automatically.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return session(ctx, port, os.Stdin, os.Stdout, o)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
