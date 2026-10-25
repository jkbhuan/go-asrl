//go:build serialintegration

package asrl

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

// Explicit opt-in only. This never sends bytes, probes, login commands or Break.
// Opening itself may affect DTR/RTS; verify the equipment's requirements first.
func TestHardwareOpenReadCloseReopen(t *testing.T) {
	name := os.Getenv("ASRL_TEST_PORT")
	if name == "" {
		t.Skip("not executed: set ASRL_TEST_PORT and verify all device parameters")
	}
	cfg := DefaultConfig()
	for key, target := range map[string]*int{"ASRL_TEST_BAUD": &cfg.BaudRate, "ASRL_TEST_DATA_BITS": &cfg.DataBits} {
		if value := os.Getenv(key); value != "" {
			v, err := strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}
			*target = v
		}
	}
	if v := os.Getenv("ASRL_TEST_PARITY"); v != "" {
		cfg.Parity = Parity(v)
	}
	if v := os.Getenv("ASRL_TEST_STOP_BITS"); v != "" {
		cfg.StopBits = StopBits(v)
	}
	if v := os.Getenv("ASRL_TEST_FLOW_CONTROL"); v != "" {
		cfg.FlowControl = FlowControl(v)
	}
	for key, target := range map[string]*bool{"ASRL_TEST_DTR": &cfg.DTR, "ASRL_TEST_RTS": &cfg.RTS} {
		if value := os.Getenv(key); value != "" {
			v, err := strconv.ParseBool(value)
			if err != nil {
				t.Fatal(err)
			}
			*target = v
		}
	}
	cfg.Timeout = 250 * time.Millisecond
	p, err := Open(name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	n, err := p.ReadContext(context.Background(), make([]byte, 4096))
	if err != nil && !errors.Is(err, ErrTimeout) {
		t.Fatalf("read n=%d: %v", n, err)
	}
	if n > 0 {
		t.Logf("read %d bytes (contents deliberately omitted); quiet-port timeout not verified", n)
	} else {
		t.Logf("read n=%d err=%v", n, err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("opened/read/closed/reopened %s with %+v; no transmitted bytes; no claim about router commands or electrical timing", name, cfg)
}
