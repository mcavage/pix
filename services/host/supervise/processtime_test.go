// processtime_test.go — encodeStartTime is the OS-independent identity
// packing at the core of darwin's process-start-time source; it carries no
// build tag so it is provable on every platform, including the Linux box
// that runs CI, without ever touching a real Darwin kernel.
package supervise

import "testing"

func TestEncodeStartTimeStableAndDistinct(t *testing.T) {
	v1, ok := encodeStartTime(1000, 500)
	if !ok {
		t.Fatal("expected ok for a valid (sec, usec) pair")
	}
	v2, ok := encodeStartTime(1000, 500)
	if !ok || v2 != v1 {
		t.Fatalf("encodeStartTime is not stable for the same input: %d vs %d", v1, v2)
	}

	// Any change to either component — a later process, or the same second
	// at a different microsecond — must produce a DIFFERENT identity value:
	// this is exactly what lets revalidateOrphan detect pid reuse.
	if laterSec, ok := encodeStartTime(1001, 500); !ok || laterSec == v1 {
		t.Fatalf("a later second must encode to a distinct value, got %d == %d", laterSec, v1)
	}
	if laterUsec, ok := encodeStartTime(1000, 501); !ok || laterUsec == v1 {
		t.Fatalf("a later microsecond must encode to a distinct value, got %d == %d", laterUsec, v1)
	}
}

func TestEncodeStartTimeRefusesOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		sec  int64
		usec int32
	}{
		{"negative seconds", -1, 0},
		{"negative microseconds", 0, -1},
		{"microseconds at 1e6", 0, 1_000_000},
		{"microseconds well past 1e6", 0, 2_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := encodeStartTime(c.sec, c.usec); ok {
				t.Fatalf("expected refusal for %s", c.name)
			}
		})
	}
}

func TestEncodeStartTimeZeroIsValid(t *testing.T) {
	// The epoch itself (sec=0, usec=0) is a legitimate — if implausible —
	// start time and must not be conflated with the refusal case; only the
	// returned ok bool signals refusal, never a zero value on its own.
	if v, ok := encodeStartTime(0, 0); !ok || v != 0 {
		t.Fatalf("expected ok=true, v=0 for the epoch, got v=%d ok=%v", v, ok)
	}
}
