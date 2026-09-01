package delivery

import (
	"math"
	"testing"
	"time"
)

func TestBackoffCeilingDoubles(t *testing.T) {
	b := NewBackoff(time.Second, time.Hour, 6)

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 64 * time.Second},
	}
	for _, tt := range tests {
		if got := b.Ceiling(tt.attempt); got != tt.want {
			t.Errorf("Ceiling(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestBackoffCeilingIsClamped(t *testing.T) {
	b := NewBackoff(time.Second, time.Hour, 100)

	t.Run("saturates at Max", func(t *testing.T) {
		// 2^12 seconds is already over an hour.
		for _, attempt := range []int{12, 20, 40, 61} {
			if got := b.Ceiling(attempt); got != time.Hour {
				t.Errorf("Ceiling(%d) = %v, want the 1h cap", attempt, got)
			}
		}
	})

	// A shift wide enough to wrap the int64 must not produce a zero or
	// negative delay, which would turn backoff into a hot loop.
	t.Run("does not overflow into a negative delay", func(t *testing.T) {
		for _, attempt := range []int{62, 63, 64, 1000, math.MaxInt32} {
			got := b.Ceiling(attempt)
			if got <= 0 {
				t.Errorf("Ceiling(%d) = %v, want a positive duration", attempt, got)
			}
			if got > time.Hour {
				t.Errorf("Ceiling(%d) = %v, want at most the cap", attempt, got)
			}
		}
	})

	t.Run("an attempt below 1 is treated as 1", func(t *testing.T) {
		if b.Ceiling(0) != b.Ceiling(1) || b.Ceiling(-5) != b.Ceiling(1) {
			t.Error("Ceiling() does not clamp attempt to >= 1")
		}
	})
}

func TestBackoffDelayStaysInsideItsWindow(t *testing.T) {
	b := NewBackoff(time.Second, time.Hour, 6)

	for attempt := 1; attempt <= 6; attempt++ {
		ceiling := b.Ceiling(attempt)
		for i := 0; i < 500; i++ {
			d := b.Delay(attempt)
			if d < 0 {
				t.Fatalf("Delay(%d) = %v, negative", attempt, d)
			}
			if d >= ceiling {
				t.Fatalf("Delay(%d) = %v, outside [0, %v)", attempt, d, ceiling)
			}
		}
	}
}

func TestBackoffDefaults(t *testing.T) {
	b := NewBackoff(0, 0, 0)
	if b.Base != DefaultBaseDelay || b.Max != DefaultMaxDelay || b.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("NewBackoff(0,0,0) = %+v, want the documented defaults", b)
	}
}

func TestShouldRetryRespectsTheAttemptBudget(t *testing.T) {
	b := NewBackoff(time.Second, time.Hour, 6)
	for attempt := 1; attempt <= 5; attempt++ {
		if !b.ShouldRetry(attempt) {
			t.Errorf("ShouldRetry(%d) = false, want true", attempt)
		}
	}
	// The sixth attempt is the last one; after it the event is dead-lettered.
	for _, attempt := range []int{6, 7, 100} {
		if b.ShouldRetry(attempt) {
			t.Errorf("ShouldRetry(%d) = true, want false", attempt)
		}
	}
}

// The property that justifies full jitter over plain exponential backoff.
//
// An endpoint goes down; a thousand events fail at the same instant. Without
// jitter every one of them retries at the same instant too, so the recovering
// endpoint takes the whole backlog as a single spike. With full jitter the
// retries are spread across the window.
//
// This asserts the spread directly: bucket a thousand delays and require that
// no single bucket holds a large share, and that every bucket is occupied.
func TestFullJitterSpreadsAThunderingHerd(t *testing.T) {
	b := NewBackoff(time.Second, time.Hour, 6)

	const (
		herd    = 1000
		attempt = 5 // ceiling 32s
		buckets = 10
	)
	ceiling := b.Ceiling(attempt)

	counts := make([]int, buckets)
	for i := 0; i < herd; i++ {
		d := b.Delay(attempt)
		idx := int(int64(d) * buckets / int64(ceiling))
		if idx >= buckets {
			idx = buckets - 1
		}
		counts[idx]++
	}

	// Uniform would be 100 per bucket. Without jitter, one bucket would hold
	// all 1000. Allow generous slack so this cannot flake.
	const maxPerBucket = herd / buckets * 2 // 200
	for i, c := range counts {
		if c == 0 {
			t.Errorf("bucket %d is empty — the herd is not spread across the window", i)
		}
		if c > maxPerBucket {
			t.Errorf("bucket %d holds %d of %d delays; the herd is still clustered", i, c, herd)
		}
	}
}

// A deterministic source proves Delay is a straight draw from [0, ceiling)
// and not, say, silently returning the ceiling.
func TestDelayUsesTheInjectedSource(t *testing.T) {
	b := NewBackoff(time.Second, time.Hour, 6)

	t.Run("a source that always returns 0 yields no delay", func(t *testing.T) {
		b.rand = func(int64) int64 { return 0 }
		if got := b.Delay(3); got != 0 {
			t.Errorf("Delay() = %v, want 0", got)
		}
	})

	t.Run("a source at the top of the range yields just under the ceiling", func(t *testing.T) {
		b.rand = func(n int64) int64 { return n - 1 }
		want := b.Ceiling(3) - 1
		if got := b.Delay(3); got != want {
			t.Errorf("Delay() = %v, want %v", got, want)
		}
	})
}
