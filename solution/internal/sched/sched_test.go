package sched

import (
	"testing"

	"tasr/internal/snap"
)

// ranked builds a live cell list from (slot, arc) pairs, hottest first.
func ranked(pairs ...[2]int) []snap.Key {
	out := make([]snap.Key, len(pairs))
	for i, p := range pairs {
		out[i] = snap.Key{T: p[0], A: p[1]}
	}
	return out
}

func slots(keys []snap.Key) []int {
	out := make([]int, len(keys))
	for i, k := range keys {
		out[i] = k.T
	}
	return out
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"hot", Hot, true},
		{"pingpong", PingPong, true},
		{"", Hot, false},
		{"ping pong", Hot, false},
	} {
		got, err := ParseMode(tc.in)
		if (err == nil) != tc.ok {
			t.Fatalf("ParseMode(%q) err=%v, want ok=%v", tc.in, err, tc.ok)
		}
		if got != tc.want {
			t.Fatalf("ParseMode(%q)=%v, want %v", tc.in, got, tc.want)
		}
	}
}

// Hot mode is the pre-pingpong behaviour: every round the globally hottest
// cells, no parity, no epoch, scale always 1.
func TestHotModeIsLegacyShape(t *testing.T) {
	s := New(Options{Mode: Hot, T: 2, FocusN: 10, HotK: 2, GrowMult: 2, ScaleCap: 16, MaxW1: 24, MaxW2: 32})
	for r := 1; r <= 4; r++ {
		d, ok := s.Next(ranked([2]int{0, 5}, [2]int{1, 7}, [2]int{0, 3}), nil)
		if !ok {
			t.Fatalf("round %d: unexpectedly stopped", r)
		}
		if d.Early {
			t.Fatalf("round %d: hot mode must never schedule an early round", r)
		}
		if got := slots(d.Hots); len(got) != 2 || got[0] != 0 || got[1] != 1 {
			t.Fatalf("round %d: hots=%v, want the top-2 hot cells in order", r, got)
		}
		if d.Scale != 1 || d.HotK != 2 {
			t.Fatalf("round %d: scale=%d hotK=%d, want 1/2", r, d.Scale, d.HotK)
		}
		// FocusN is set, but hot mode must not build an epoch around it: no
		// restart, no scale growth, no focus membership.
		if d.Restart || s.Scale() != 1 || s.InFocus(snap.Key{T: 0, A: 5}) {
			t.Fatalf("round %d: hot mode ran the epoch machinery (restart=%v scale=%d)",
				r, d.Restart, s.Scale())
		}
	}
}

// PingPong alternates hot and early rounds, and the early cursor sweeps the
// slots in order (doc §14).
func TestPingPongParityAndEarlyCursor(t *testing.T) {
	s := New(Options{Mode: PingPong, T: 2, HotK: 1, GrowMult: 2, ScaleCap: 16})
	live := ranked([2]int{0, 5}, [2]int{1, 7})
	var earlySlots []int
	for r := 1; r <= 6; r++ {
		d, ok := s.Next(live, nil)
		if !ok {
			t.Fatalf("round %d: unexpectedly stopped", r)
		}
		wantEarly := r%2 == 0
		if d.Early != wantEarly {
			t.Fatalf("round %d: early=%v, want %v", r, d.Early, wantEarly)
		}
		if d.Early {
			if len(d.Hots) != 1 {
				t.Fatalf("round %d: early round has %d cells, want 1", r, len(d.Hots))
			}
			earlySlots = append(earlySlots, d.Hots[0].T)
		} else if d.Hots[0].T != 0 {
			t.Fatalf("round %d: hot round anchored at slot %d, want the hottest cell's slot 0", r, d.Hots[0].T)
		}
	}
	want := []int{0, 1, 0}
	if len(earlySlots) != len(want) {
		t.Fatalf("early rounds=%v, want %v", earlySlots, want)
	}
	for i := range want {
		if earlySlots[i] != want[i] {
			t.Fatalf("early cursor=%v, want %v", earlySlots, want)
		}
	}
}

// An early round skips cursor slots with no live cell rather than wasting the
// round on an empty slot.
func TestEarlyCursorSkipsDeadSlots(t *testing.T) {
	s := New(Options{Mode: PingPong, T: 4, HotK: 1, GrowMult: 2, ScaleCap: 16})
	// Slots 1 and 3 carry no live cell at all.
	live := ranked([2]int{0, 5}, [2]int{2, 7})
	d, ok := s.Next(live, nil) // hot
	if !ok || d.Early {
		t.Fatalf("round 1: want a hot round, got early=%v ok=%v", d.Early, ok)
	}
	d, ok = s.Next(live, nil) // early, cursor 0
	if !ok || !d.Early || d.Hots[0].T != 0 {
		t.Fatalf("round 2: early anchor=%d ok=%v, want slot 0", d.Hots[0].T, ok)
	}
	d, ok = s.Next(live, nil) // hot
	if !ok || d.Early {
		t.Fatalf("round 3: want a hot round, got early=%v ok=%v", d.Early, ok)
	}
	d, ok = s.Next(live, nil) // early, cursor 1 is dead -> skip to 2
	if !ok || !d.Early || d.Hots[0].T != 2 {
		t.Fatalf("round 4: early anchor=%d ok=%v, want slot 2", d.Hots[0].T, ok)
	}
}

// A hot round attacks only its epoch's focus list, and the anchor slot's cells
// come first (doc §9 same_t_first).
func TestHotRoundStaysInsideFocus(t *testing.T) {
	s := New(Options{Mode: PingPong, T: 2, FocusN: 2, HotK: 2, GrowMult: 2, ScaleCap: 16})
	// The focus is the top-2 at epoch start: slot 0 only.  The later slot-1
	// cell is hotter than nothing but is not a focus member, so a hot round
	// must not attack it.
	d, ok := s.Next(ranked([2]int{0, 5}, [2]int{0, 6}, [2]int{1, 7}), nil)
	if !ok || !d.Restart {
		t.Fatalf("round 1: want the epoch to open (restart), got %+v ok=%v", d, ok)
	}
	if got := len(d.Hots); got != 2 || d.Hots[0].T != 0 || d.Hots[1].T != 0 {
		t.Fatalf("hot round attacked %v, want the two focus cells of slot 0", d.Hots)
	}
}

// The epoch ends when its focus list is exhausted -- not on a freeze count --
// and restarts at a higher scale with the cursor rewound (doc §14/§21).
func TestEpochExhaustionRestartsAtHigherScale(t *testing.T) {
	s := New(Options{Mode: PingPong, T: 2, FocusN: 1, HotK: 1, GrowMult: 2, ScaleCap: 2})
	a := snap.Key{T: 0, A: 5}
	b := snap.Key{T: 1, A: 7}
	c := snap.Key{T: 0, A: 3}

	d, ok := s.Next([]snap.Key{a, b, c}, nil)
	if !ok || !d.Restart || d.Scale != 1 {
		t.Fatalf("round 1: want epoch 1 at scale 1, got %+v ok=%v", d, ok)
	}
	if !s.InFocus(a) || s.InFocus(b) {
		t.Fatalf("focus should hold {a} only: a=%v b=%v", s.InFocus(a), s.InFocus(b))
	}
	// a is retired; the epoch is now exhausted.
	d, ok = s.Next([]snap.Key{b, c}, nil)
	if !ok {
		t.Fatal("round 2: the epoch should restart, not stop")
	}
	if !d.Restart || d.Scale != 2 {
		t.Fatalf("round 2: want a restart at scale 2, got restart=%v scale=%d", d.Restart, d.Scale)
	}
	if !s.InFocus(b) {
		t.Fatal("round 2: the new epoch's focus should hold the hottest live cell b")
	}
	// b is retired too; the scale is at the cap, so the search stops without
	// lifting anything (doc §21: scale to cap means stop).
	if _, ok := s.Next([]snap.Key{c}, nil); ok {
		t.Fatal("round 3: an exhausted epoch at the scale cap must stop the search")
	}
}

// Reschedule rewinds the early cursor and rebuilds the focus on the next round.
func TestRescheduleRebuildsFocus(t *testing.T) {
	s := New(Options{Mode: PingPong, T: 2, FocusN: 1, HotK: 1, GrowMult: 2, ScaleCap: 16})
	a := snap.Key{T: 0, A: 5}
	b := snap.Key{T: 1, A: 7}
	if d, ok := s.Next([]snap.Key{a, b}, nil); !ok || !s.InFocus(a) {
		t.Fatalf("round 1: want focus {a}, got %+v", d)
	}
	s.Reschedule()
	if s.InFocus(a) {
		t.Fatal("Reschedule must drop the old focus")
	}
	d, ok := s.Next([]snap.Key{b}, nil)
	if !ok || !s.InFocus(b) {
		t.Fatalf("round 2: want a fresh focus {b}, got %+v ok=%v", d, ok)
	}
	if d.Early {
		t.Fatal("Reschedule must rewind the pingpong parity to a hot round")
	}
}

// The round's scale is max(search_scale, the seed's retry scale) and it widens
// the round geometry: hotK multiplicatively, the candidate budgets additively.
func TestSeedRetryScaleWidensTheRound(t *testing.T) {
	s := New(Options{Mode: PingPong, T: 2, HotK: 2, GrowMult: 2, ScaleCap: 16, MaxW1: 24, MaxW2: 32})
	live := ranked([2]int{0, 5}, [2]int{0, 6}, [2]int{1, 7})
	seedScale := func(k snap.Key) int {
		if k.A == 5 {
			return 4
		}
		return 1
	}
	d, ok := s.Next(live, seedScale)
	if !ok {
		t.Fatal("round 1 stopped unexpectedly")
	}
	// scale 4: hotK doubles twice over (2 -> 8); the budgets take three
	// WidthStep-10 steps (24+30, 32+30), not a factor of four.
	if d.Scale != 4 || d.HotK != 8 || d.MaxW1 != 54 || d.MaxW2 != 62 {
		t.Fatalf("scaled round = scale %d hotK %d w1 %d w2 %d, want 4/8/54/62",
			d.Scale, d.HotK, d.MaxW1, d.MaxW2)
	}
	// The other seed has no retry bump: the base geometry.
	d, ok = s.Next(live, func(snap.Key) int { return 1 })
	if !ok {
		t.Fatal("round 2 stopped unexpectedly")
	}
	if d.Scale != 1 || d.HotK != 2 {
		t.Fatalf("unbumped round = scale %d hotK %d, want 1/2", d.Scale, d.HotK)
	}
}

// The width ceilings hold even at the largest scale.
func TestWidthCeilings(t *testing.T) {
	s := New(Options{Mode: Hot, T: 2, HotK: 8, GrowMult: 2, ScaleCap: 16, MaxW1: 24, MaxW2: 32})
	if _, w1, w2 := s.Widths(16); w1 != maxWidthW1 || w2 != maxWidthW2 {
		t.Fatalf("Widths(16) = %d/%d, want the ceilings %d/%d", w1, w2, maxWidthW1, maxWidthW2)
	}
}

// The candidate budgets grow by a fixed step per scale, not by a factor: the
// round's column count is hotK x (MaxW1+MaxW2), so a multiplicative window
// compounds the growth.  WidthStep is the knob that keeps it linear.
func TestWidthsGrowAdditively(t *testing.T) {
	s := New(Options{Mode: PingPong, T: 2, HotK: 6, GrowMult: 2, ScaleCap: 8,
		MaxW1: 24, MaxW2: 32, WidthStep: 10})
	for scale := 1; scale <= 8; scale++ {
		hotK, w1, w2 := s.Widths(scale)
		if hotK != 6*scale {
			t.Fatalf("Widths(%d) hotK = %d, want %d", scale, hotK, 6*scale)
		}
		if w1 != 24+10*(scale-1) {
			t.Fatalf("Widths(%d) w1 = %d, want %d", scale, w1, 24+10*(scale-1))
		}
		if w2 != 32+10*(scale-1) {
			t.Fatalf("Widths(%d) w2 = %d, want %d", scale, w2, 32+10*(scale-1))
		}
	}
	// A zero WidthStep means the default, not "no growth".
	s0 := New(Options{Mode: Hot, T: 2, HotK: 6, MaxW1: 24, MaxW2: 32})
	if _, w1, _ := s0.Widths(3); w1 != 24+2*DefaultWidthStep {
		t.Fatalf("zero WidthStep: w1 at scale 3 = %d, want the default step", w1)
	}
}
