// Package sched implements the round schedule of the V1.0 solver (design doc
// §14): the pingpong parity, the focus-N epoch and the search-scale escalator.
//
// PingPong alternates two round kinds by parity: an even round is hot (it
// attacks the hottest still-live cell of the epoch's fixed focus list) and an
// odd round is early (it sweeps the time slots with the `early` cursor
// regardless of global heat).  The early sweep is the point: while the single
// hottest cell is stuck, the tail of the lexicographic vector keeps moving
// instead of the loop spending rounds on a cell the pool cannot relieve.
//
// focus-N wraps that alternation in an epoch.  At the start of an epoch the
// hottest N still-live cells are fixed as epoch_focus, and a hot round may only
// attack one of them -- so the search stops chasing a new peak the moment one
// appears (doc §21: never unfreeze to attack the N+1th peak).  The epoch ends
// when no focus member is live any more, i.e. every one of them has been
// soft-frozen or skipped.  That is a set-exhaustion predicate, not a count of
// freezes: soft_frozen only ever holds focus members, so a counter could never
// exceed N and the epoch would never end.
//
// When the epoch ends and search_scale * grow_mult still fits under scale_cap,
// the epoch restarts: the soft freezes are lifted, the scale is multiplied, and
// pingpong restarts from t=0.  With the cap reached the search stops without
// lifting anything.
//
// The scale is the doc's single escalator.  It multiplies n_hot_max (HotK here)
// and the candidate budget.  mip_budget has no exact counterpart in the Go
// targeted generator -- the closest analogue is the per-demand waypoint cap, so
// that is what scales, under an absolute ceiling that keeps one round finite.
// The round's scale is max(search_scale, the seed's retry scale), the latter
// being what a proven-optimal miss bumps before giving up on that seed (§15).
package sched

import (
	"fmt"

	"tasr/internal/snap"
)

// Mode is the round-target schedule.
type Mode int

const (
	// Hot is the legacy schedule: every round attacks the globally hottest
	// still-live cell.  It is the default, so -schedule hot reproduces the
	// pre-pingpong solver bit for bit.
	Hot Mode = iota
	// PingPong is the design doc §14 schedule: hot and early rounds alternate.
	PingPong
)

func (m Mode) String() string {
	switch m {
	case Hot:
		return "hot"
	case PingPong:
		return "pingpong"
	}
	return "unknown"
}

// ParseMode parses the -schedule flag value.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "hot":
		return Hot, nil
	case "pingpong":
		return PingPong, nil
	}
	return Hot, fmt.Errorf("sched: unknown schedule %q (want hot or pingpong)", s)
}

// DefaultWidthStep is how much one scale step adds to each per-demand candidate
// budget; Options.WidthStep overrides it.
const DefaultWidthStep = 10

// The scale widens the round, and a window that is allowed to grow without
// bound would eventually spend the whole wall clock inside one pool build: the
// doc checks the wall clock only at round boundaries (§21), so a round that
// overruns misses the deadline entirely rather than being cut short.  These
// ceilings keep the widest round finite.  They are a backstop -- with the
// additive law below the budgets reach 94/102 at the widest scale, so in
// practice it is WidthStep, not these, that bounds the round.
const (
	maxWidthW1 = 96
	maxWidthW2 = 128
)

// Options configures a Scheduler.
type Options struct {
	Mode Mode
	// T is the instance's time-slot count: the early cursor sweeps 0..T-1.
	T int
	// FocusN > 0 enables the focus-N epoch with an N-cell focus list.
	FocusN int
	// HotK is the base n_hot_max: how many cells a round decomposes together.
	HotK int
	// GrowMult multiplies the scale when an epoch restarts, ScaleCap ceilings it.
	GrowMult int
	ScaleCap int
	// MaxW1 / MaxW2 are the base (scale 1) per-demand candidate budgets; each
	// further scale step adds WidthStep to both (see Widths).
	MaxW1 int
	MaxW2 int
	// WidthStep is the per-scale-step increment of both candidate budgets.  0
	// takes DefaultWidthStep.
	WidthStep int
}

// Decision is one round's instruction.
type Decision struct {
	// Early reports the pingpong parity: true = early sweep, false = hot round.
	Early bool
	// Slot is the anchor time slot (the hot cell's slot, or the early cursor's).
	Slot int
	// Hots is the cell set the round decomposes, hottest first with the anchor
	// slot's cells in front (doc §9's same_t_first).
	Hots []snap.Key
	// Seed is Hots[0]: the cell the round is really about.
	Seed snap.Key
	// Restart is true on the round that opens an epoch: the caller lifts the
	// epoch's soft freezes and forgets the epoch's confidence memory.
	Restart bool
	// Scale is the search scale this round runs at; HotK / MaxW1 / MaxW2 are
	// already scaled by it.
	Scale int
	HotK  int
	MaxW1 int
	MaxW2 int
}

// Scheduler is the stateful round scheduler.
type Scheduler struct {
	opts     Options
	scale    int
	step     int // rounds since the epoch opened: even = hot, odd = early
	early    int // early sweep cursor
	focus    []snap.Key
	focusSet map[snap.Key]bool
	hasFocus bool
}

// New builds a scheduler for the given options, applying the defaults that keep
// a zero value sane.
func New(opts Options) *Scheduler {
	if opts.T < 1 {
		opts.T = 1
	}
	if opts.HotK < 1 {
		opts.HotK = 1
	}
	if opts.GrowMult < 1 {
		opts.GrowMult = 1
	}
	if opts.ScaleCap < 1 {
		opts.ScaleCap = 1
	}
	return &Scheduler{opts: opts, scale: 1}
}

// Mode returns the configured schedule.
func (s *Scheduler) Mode() Mode { return s.opts.Mode }

// Scale returns the current search scale.
func (s *Scheduler) Scale() int { return s.scale }

// InFocus reports whether k is a member of the current epoch's focus list.  The
// caller uses it to tell a soft freeze from a plain skip (doc §15).
func (s *Scheduler) InFocus(k snap.Key) bool { return s.focusSet[k] }

// Reschedule drops the epoch focus and rewinds the early cursor.  It is what a
// first-bit improvement calls: the hot structure changed, so every conclusion
// the current epoch was built on is void (doc §15).  The next Next call opens a
// fresh epoch.
func (s *Scheduler) Reschedule() {
	s.focus, s.focusSet, s.hasFocus = nil, nil, false
	s.step, s.early = 0, 0
}

// Widths returns the round geometry the given scale maps to.
//
// hotK scales multiplicatively: it is a property of the *round* -- how many
// cells the round decomposes -- and a wider round genuinely needs more of them.
//
// The per-demand candidate budgets do not.  They are per *demand*, so the
// round's column count is hotK x MaxW1 (+ MaxW2) and multiplying them
// multiplies that by the same factor a second time: a doubling window
// (24, 48, 96, ...) makes the last epoch's pool build and MIP an order of
// magnitude heavier than the first's.  They grow by a fixed WidthStep per scale
// instead (24, 34, 44, ...), so the round's size stays linear in the epoch
// count.  The ceilings above remain as a backstop.
func (s *Scheduler) Widths(scale int) (hotK, maxW1, maxW2 int) {
	if scale < 1 {
		scale = 1
	}
	step := s.opts.WidthStep
	if step <= 0 {
		step = DefaultWidthStep
	}
	add := step * (scale - 1)
	hotK = s.opts.HotK * scale
	maxW1 = s.opts.MaxW1 + add
	maxW2 = s.opts.MaxW2 + add
	if maxW1 > maxWidthW1 {
		maxW1 = maxWidthW1
	}
	if maxW2 > maxWidthW2 {
		maxW2 = maxWidthW2
	}
	return hotK, maxW1, maxW2
}

// Next advances the schedule by one round.  ranked is the live cell list in
// decreasing saturation order, with cells that carry no load already removed.
// seedScale reports a seed's retry scale (doc §15: the round runs at
// max(search_scale, the seed's retry scale)); it may be nil.
//
// topRetired is the caller's exhaustion test, and it is the one that fires in
// practice.  The focus test alone is weak: the fixed focus list only tracks the
// cells that were hot when the epoch opened, and every accept clears the
// retired flag of the cells it moved, so a focus that keeps yielding small
// moves is never exhausted and the epoch never turns over -- the scale
// escalator, which is the whole point of an epoch, never fires.  topRetired
// asks the direct question instead: the caller ranks the cells *before*
// subtracting the retired set, so a frozen cell still stands where it stands,
// and reports whether the hottest n of them are all retired.  The load vector
// is ordered by that same ranking, so once every cell that could still move its
// first n layers is gone the epoch has nothing left to aim at.  Physical pins
// count as retired: they can never move, so letting one hold an epoch open is
// exactly the mistake this test exists to avoid.
//
// ok == false means the search is over: the epoch is exhausted and the scale
// cannot grow past scale-cap.
func (s *Scheduler) Next(ranked []snap.Key, seedScale func(snap.Key) int, topRetired bool) (Decision, bool) {
	var d Decision
	if len(ranked) == 0 {
		return d, false
	}

	// The legacy schedule has no parity and no epoch: every round the globally
	// hottest cells at scale 1.  Taking this branch before the epoch machinery
	// matters -- letting it run would build a focus and grow the scale, which is
	// exactly the pingpong behaviour the flag exists to keep out of the control
	// arm.
	if s.opts.Mode == Hot {
		d.HotK, d.MaxW1, d.MaxW2 = s.Widths(1)
		d.Scale = 1
		d.Hots = head(ranked, d.HotK)
		d.Seed = d.Hots[0]
		d.Slot = d.Seed.T
		return d, true
	}

	// Epoch transition.  An exhausted epoch restarts at a higher scale while
	// that still fits under the cap; with the cap reached the search stops
	// without lifting anything -- never unfreeze to attack the N+1th peak
	// (doc §21).  It is exhausted when every focus member has left the live
	// list (soft-frozen, skipped, or dropped to zero load) or when the caller
	// reports the top of the vector is retired outright.
	for s.opts.FocusN > 0 && s.hasFocus && (topRetired || len(s.liveFocus(ranked)) == 0) {
		if s.scale*s.opts.GrowMult > s.opts.ScaleCap {
			return d, false
		}
		s.scale *= s.opts.GrowMult
		s.hasFocus = false
	}

	if s.opts.FocusN > 0 && !s.hasFocus {
		n := s.opts.FocusN
		if n > len(ranked) {
			n = len(ranked)
		}
		s.focus = append([]snap.Key(nil), ranked[:n]...)
		s.focusSet = make(map[snap.Key]bool, n)
		for _, k := range s.focus {
			s.focusSet[k] = true
		}
		s.hasFocus = true
		d.Restart = true
	}

	// Pingpong parity: rounds are counted from the epoch's start, even = hot,
	// odd = early (doc §14, "偶数 round 为 hot，奇数 round 为 early").
	d.Early = s.step%2 == 1
	s.step++

	// The round's seed is known before its width: a hot round's seed is the
	// hottest live focus member (or the globally hottest cell with the focus
	// off) and an early round's is the hottest cell of its cursor slot.  The
	// per-seed retry scale then widens *this* round, which is the doc's
	// max(search_scale, seed_scale).
	var src []snap.Key
	if d.Early {
		// Sweep from the cursor to the next slot that still has a live cell: a
		// slot with nothing to relieve cannot carry a round, and skipping it
		// keeps the cursor's rotation intact (the doc advances it by one per
		// early round, which the loop below does for every slot it passes).
		for tries := 0; tries < s.opts.T && len(src) == 0; tries++ {
			src = slotOf(ranked, s.early%s.opts.T)
			s.early++
		}
	} else if s.hasFocus {
		src = s.liveFocus(ranked)
	} else {
		src = ranked
	}
	if len(src) == 0 {
		return d, false
	}
	seed := src[0]
	scale := s.scale
	if seedScale != nil {
		if v := seedScale(seed); v > scale {
			scale = v
		}
	}
	d.Scale = scale
	d.HotK, d.MaxW1, d.MaxW2 = s.Widths(scale)
	if d.Early {
		d.Hots = head(src, d.HotK)
	} else {
		// same_t_first (doc §9): the seed slot's cells come first, the rest of
		// the pool fills the round up to n_hot_max.
		d.Hots = sameTFirst(src, seed.T, d.HotK)
	}
	d.Seed = d.Hots[0]
	d.Slot = d.Seed.T
	return d, true
}

// slotOf returns the live cells of slot t, hottest first.
func slotOf(ranked []snap.Key, t int) []snap.Key {
	var out []snap.Key
	for _, k := range ranked {
		if k.T == t {
			out = append(out, k)
		}
	}
	return out
}

// liveFocus returns the focus members present in ranked, in ranked order.
func (s *Scheduler) liveFocus(ranked []snap.Key) []snap.Key {
	if !s.hasFocus {
		return nil
	}
	out := make([]snap.Key, 0, len(s.focus))
	for _, k := range ranked {
		if s.focusSet[k] {
			out = append(out, k)
		}
	}
	return out
}

// sameTFirst returns up to n cells with slot t's first, both groups in the
// given (decreasing saturation) order.
func sameTFirst(keys []snap.Key, t, n int) []snap.Key {
	out := make([]snap.Key, 0, n)
	for _, k := range keys {
		if k.T != t {
			continue
		}
		out = append(out, k)
		if len(out) == n {
			return out
		}
	}
	for _, k := range keys {
		if k.T == t {
			continue
		}
		out = append(out, k)
		if len(out) == n {
			break
		}
	}
	return out
}

// head returns up to n cells, hottest first.
func head(keys []snap.Key, n int) []snap.Key {
	if n > len(keys) {
		n = len(keys)
	}
	return keys[:n]
}
