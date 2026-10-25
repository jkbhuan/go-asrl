package asrl

import (
	"fmt"
	"math"
	"time"
)

// Parity is the hardware parity setting, without any software byte conversion.
type Parity string

const (
	ParityNone  Parity = "none"
	ParityOdd   Parity = "odd"
	ParityEven  Parity = "even"
	ParityMark  Parity = "mark"
	ParitySpace Parity = "space"
)

type StopBits string

const (
	OneStopBit           StopBits = "1"
	OnePointFiveStopBits StopBits = "1.5"
	TwoStopBits          StopBits = "2"
)

type FlowControl string

const (
	FlowControlNone    FlowControl = "none"
	FlowControlRTSCTS  FlowControl = "rtscts"
	FlowControlXONXOFF FlowControl = "xonxoff"
)

// Config describes a port at open time. Zero fields use DefaultConfig values.
// Timeout == 0 means the default (5s); use Port.SetTimeout(0) after Open for
// unlimited I/O. Negative timeouts are invalid at open time.
// DTR and RTS select static initial line levels (false by default). Drivers may
// change these lines transiently during open/close; consult the device manual.
type Config struct {
	BaudRate    int
	DataBits    int
	Parity      Parity
	StopBits    StopBits
	FlowControl FlowControl
	Timeout     time.Duration
	DTR         bool
	RTS         bool
}

// DefaultConfig is a starting suggestion, not a confirmation of device settings.
func DefaultConfig() Config {
	return Config{BaudRate: 9600, DataBits: 8, Parity: ParityNone,
		StopBits: OneStopBit, FlowControl: FlowControlNone, Timeout: 5 * time.Second}
}

// Normalize fills unspecified fields and rejects invalid values. Known flow
// control modes are valid configurations, but Open currently supports only none.
func (c Config) Normalize() (Config, error) {
	d := DefaultConfig()
	if c.BaudRate == 0 {
		c.BaudRate = d.BaudRate
	}
	if c.DataBits == 0 {
		c.DataBits = d.DataBits
	}
	if c.Parity == "" {
		c.Parity = d.Parity
	}
	if c.StopBits == "" {
		c.StopBits = d.StopBits
	}
	if c.FlowControl == "" {
		c.FlowControl = d.FlowControl
	}
	if c.Timeout == 0 {
		c.Timeout = d.Timeout
	}
	invalid := func(detail string) (Config, error) {
		return Config{}, fmt.Errorf("%w: %s", ErrInvalidConfig, detail)
	}
	if c.BaudRate < 1 || uint64(c.BaudRate) > math.MaxUint32 {
		return invalid("baud rate must fit a positive uint32")
	}
	if c.DataBits < 5 || c.DataBits > 8 {
		return invalid("data bits must be 5..8")
	}
	switch c.Parity {
	case ParityNone, ParityOdd, ParityEven, ParityMark, ParitySpace:
	default:
		return invalid("unknown parity")
	}
	switch c.StopBits {
	case OneStopBit:
	case OnePointFiveStopBits:
		if c.DataBits != 5 {
			return invalid("1.5 stop bits requires 5 data bits")
		}
	case TwoStopBits:
		if c.DataBits == 5 {
			return invalid("2 stop bits requires 6..8 data bits")
		}
	default:
		return invalid("unknown stop bits")
	}
	switch c.FlowControl {
	case FlowControlNone, FlowControlRTSCTS, FlowControlXONXOFF:
	default:
		return invalid("unknown flow control")
	}
	if c.Timeout < 0 {
		return invalid("negative open timeout; use SetTimeout(0) for unlimited I/O")
	}
	return c, nil
}
