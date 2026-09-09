package eval

import "testing"

func TestRankIntTruncatesNotRounds(t *testing.T) {
	cases := []struct {
		x    float64
		want int64
	}{
		{0.0, 0},
		{0.123456, 123456},        // exact
		{0.1234565, 123456},       // truncation, not rounding
		{0.1234564999, 123456},    // hair below boundary stays below
		{0.123457, 123457},        // next decimal
		{1.0, 1000000},            // MLU at 1.0
		{1.000000686106, 1000000}, // >1.0 truncates to 1.000000 (observed setA-01)
	}
	for _, c := range cases {
		if got := RankInt(c.x); got != c.want {
			t.Errorf("RankInt(%v) = %d, want %d", c.x, got, c.want)
		}
	}
}

func TestRankIntEpsAbsorbsBoundaryNoise(t *testing.T) {
	// x*1e6 lands a hair below 123456 in IEEE but is mathematically exactly on
	// the decimal boundary; the +eps must lift it to the boundary.
	x := 0.123456 - 5e-13 // still representable below scaled 123456
	if got := RankInt(x); got != 123456 {
		t.Errorf("RankInt near-boundary = %d, want 123456", got)
	}
}

func TestLexCompareAndFirstDivergence(t *testing.T) {
	a := SortedDesc([]float64{0.9, 0.5, 0.4, 0.1})
	b := SortedDesc([]float64{0.9, 0.5, 0.35, 0.2})
	if got := LexCompare(a, b); got != 1 {
		t.Errorf("a (0.4 @layer3) should lose to b (0.35): got %d", got)
	}
	if got := LexCompare(b, a); got != -1 {
		t.Errorf("b should win: got %d", got)
	}
	layer, qa, qb := FirstDivergence(a, b)
	if layer != 3 || qa != 400000 || qb != 350000 {
		t.Errorf("FirstDivergence = (%d, %d, %d), want (3, 400000, 350000)", layer, qa, qb)
	}

	c := append([]float64(nil), a...)
	if got := LexCompare(a, c); got != 0 {
		t.Errorf("equal vectors should tie: got %d", got)
	}
}
