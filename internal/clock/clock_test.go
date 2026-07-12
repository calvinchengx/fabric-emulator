package clock

import "testing"

func fixed(at int64) *Clock {
	return &Clock{realNow: func() int64 { return at }}
}

func TestOffsetAndAdvance(t *testing.T) {
	c := fixed(1000)
	if c.Now() != 1000 {
		t.Fatalf("Now() = %d; want 1000", c.Now())
	}
	c.Advance(50)
	if c.Now() != 1050 {
		t.Fatalf("after Advance(50): %d; want 1050", c.Now())
	}
	c.SetOffset(-100)
	if c.Now() != 900 {
		t.Fatalf("after SetOffset(-100): %d; want 900", c.Now())
	}
}

func TestFreezeUnfreezeContinuity(t *testing.T) {
	real := int64(1000)
	c := &Clock{realNow: func() int64 { return real }}
	c.Freeze()
	real = 2000 // real time races ahead
	if c.Now() != 1000 {
		t.Fatalf("frozen Now() = %d; want 1000", c.Now())
	}
	c.Advance(5)
	if c.Now() != 1005 {
		t.Fatalf("frozen+advanced Now() = %d; want 1005", c.Now())
	}
	c.Unfreeze()
	if c.Now() != 1005 {
		t.Fatalf("unfrozen Now() = %d; want continuity at 1005", c.Now())
	}
	real = 2010
	if c.Now() != 1015 {
		t.Fatalf("after real +10: %d; want 1015", c.Now())
	}
}

func TestState(t *testing.T) {
	c := fixed(500)
	c.Advance(25)
	offset, frozen, now := c.State()
	if offset != 25 || frozen || now != 525 {
		t.Fatalf("State() = (%d,%v,%d); want (25,false,525)", offset, frozen, now)
	}
	c.Freeze()
	_, frozen, _ = c.State()
	if !frozen {
		t.Fatal("State() frozen = false after Freeze")
	}
}
