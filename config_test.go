package asrl

import (
	"errors"
	"testing"
	"time"
)

func TestConfigValidationBeforeOpen(t *testing.T) {
	invalid := []Config{
		{BaudRate: -1}, {DataBits: 4}, {DataBits: 9}, {Parity: "typo"}, {StopBits: "3"},
		{FlowControl: "auto"}, {Timeout: -time.Second}, {DataBits: 8, StopBits: OnePointFiveStopBits},
		{DataBits: 5, StopBits: TwoStopBits},
	}
	for _, cfg := range invalid {
		p, err := Open("not-a-real-port", cfg)
		if p != nil || !errors.Is(err, ErrInvalidConfig) || !isUnstarted(err) {
			t.Fatalf("cfg=%+v: %v %v", cfg, p, err)
		}
	}
	got, err := (Config{}).Normalize()
	if err != nil || got != DefaultConfig() {
		t.Fatalf("zero normalization = %+v %v", got, err)
	}
	for _, cfg := range []Config{{DataBits: 5, StopBits: OnePointFiveStopBits}, {DataBits: 7, Parity: ParityEven, StopBits: TwoStopBits}, {FlowControl: FlowControlRTSCTS}} {
		if _, err := cfg.Normalize(); err != nil {
			t.Fatalf("legal cfg: %+v %v", cfg, err)
		}
	}
	for _, name := range []string{"", " ", "COM2\x00extra"} {
		if _, err := Open(name, Config{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid port %q: %v", name, err)
		}
	}
}

func TestWindowsPortNames(t *testing.T) {
	for input, want := range map[string]string{"COM2": `\\.\COM2`, "COM10": `\\.\COM10`, "com1234": `\\.\COM1234`, `\\.\COM10`: `\\.\COM10`, `\\?\COM10`: `\\?\COM10`, `\\?\usb#vendor#serial`: `\\?\usb#vendor#serial`} {
		got, err := windowsPortName(input)
		if err != nil || got != want {
			t.Fatalf("%q -> %q %v (want %q)", input, got, err, want)
		}
	}
	for _, input := range []string{"2", "COM", "COM0", "COM01", "COM2:", "COM-1", "COM2foo", " COM2", "ASRL2::INSTR", "/dev/ttyS2", `\\.\`, "COM2\x00extra"} {
		if _, err := windowsPortName(input); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("accepted %q: %v", input, err)
		}
	}
}
