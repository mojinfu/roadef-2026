package cand

import (
	"fmt"
	"testing"

	"tasr/internal/graph"
	"tasr/internal/model"
)

// serialLadderInstance is the residual strategy's test bed.  The demand runs
// 0 -> 4 over three parallel two-hop tracks, and one three-hop way round exists
// solely so that banning *two* arcs still leaves a route:
//
//	0 -> 1 -> 4   arcs 0,1   the A track
//	0 -> 2 -> 4   arcs 2,3   the B track
//	0 -> 3 -> 2   arcs 4,5   the detour to 2 that avoids B, through node 3
//	0 -> 5 -> 4   arcs 6,7   a track pressing neither hot arc
//
// Node 3 exists only on that detour, so it can be discovered by the residual
// walk and by nothing else -- which is what makes it a witness for the
// strategy's central property.
func serialLadderInstance() *model.Instance {
	arcs := []model.Arc{
		{ID: 0, From: 0, To: 1, Metric: 1, Capacity: 100},
		{ID: 1, From: 1, To: 4, Metric: 1, Capacity: 100},
		{ID: 2, From: 0, To: 2, Metric: 1, Capacity: 100},
		{ID: 3, From: 2, To: 4, Metric: 1, Capacity: 100},
		{ID: 4, From: 0, To: 3, Metric: 1, Capacity: 100},
		{ID: 5, From: 3, To: 2, Metric: 1, Capacity: 100},
		{ID: 6, From: 0, To: 5, Metric: 1, Capacity: 100},
		{ID: 7, From: 5, To: 4, Metric: 1, Capacity: 100},
	}
	names := []string{"n0", "n1", "n2", "n3", "n4", "n5"}
	ids := []int{0, 1, 2, 3, 4, 5}
	return &model.Instance{
		Name:      "serial-ladder",
		NodeNames: names,
		NodeIDs:   ids,
		Arcs:      arcs,
		NSlots:    1,
		Demands:   []model.Demand{{Source: 0, Target: 4, Volume: []float64{1}}},
		Scenario: model.Scenario{
			MaxSegments: 2,
			Budget:      []int{0},
			Blocked:     [][]bool{make([]bool, len(arcs))},
		},
	}
}

type residualFixture struct {
	inst *model.Instance
	g    *graph.Graph
	ix   *graph.Index
}

func newResidualFixture(inst *model.Instance) residualFixture {
	g := graph.New(inst)
	return residualFixture{inst: inst, g: g, ix: graph.NewIndex(g, inst.Scenario.Blocked, 0)}
}

// pool runs the residual strategy alone, exactly the way Build calls it.
// IndexHops keeps the fixture free of the hop cache, which the strategy only
// uses for ranking.
func (f residualFixture) pool(fam Family, opts Options) stratPool {
	memo := newRouteMemo(&fakeRouter{n: f.g.N}, fam.D)
	return residual(memo, f.ix, IndexHops{IX: f.ix}, f.g, f.inst, fam, opts.withDefaults())
}

// serialFamily is the ladder's hot pair: arc 0 (the A track) hottest, arc 2
// (the B track) second.  ArcRelief is one minus the unit's split on that arc --
// "the fraction of the cell this candidate leaves unloaded" -- so a candidate
// clears step 4's gate iff it does not load the arc at all.
func serialFamily() Family {
	return Family{
		D:      0,
		Slots:  []int{0},
		Hots:   map[int][]int{0: {0, 2}},
		Relief: alwaysRelief,
		ArcRelief: func(_ int, arc int, unit []float64) float64 {
			return 1.0 - unit[arc]
		},
	}
}

func pairStrings(ps [][2]int) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = fmt.Sprintf("%d,%d", p[0], p[1])
	}
	return out
}

// The whole point of the strategy: V is taken from the path the *first* point
// already produced, not from an independent detour crossed with it.
//
// Banning arc 0 leaves the U's {2,5}.  Only U=2's A-free path presses another
// hot arc (arc 2), so only U=2 gets a partner; the partner is node 3, which the
// 0->4 graph reaches only via 0->3->2 once arc 2 is banned too -- i.e. V exists
// only because U is already in the route.  U=5's new path presses no hot arc,
// so it is emitted bare, exactly as "one point is enough" requires.
func TestResidualPairIsSerialAndEmittedInBothOrders(t *testing.T) {
	f := newResidualFixture(serialLadderInstance())
	got := f.pool(serialFamily(), Options{Mode: ModeResidual})

	// Pairs lead the pool (see the ordering note in residual): U then V, then
	// the bare singleton U=5, which every other strategy could have found too.
	if want := []int{2, 3, 5}; !sameInts(ids(got.nodes), want) {
		t.Fatalf("pool nodes = %v, want %v", ids(got.nodes), want)
	}
	wantPairs := []string{"2,3", "3,2"}
	gotPairs := pairStrings(got.pairs)
	if len(gotPairs) != len(wantPairs) {
		t.Fatalf("pool pairs = %v, want %v", gotPairs, wantPairs)
	}
	for i := range wantPairs {
		if gotPairs[i] != wantPairs[i] {
			t.Fatalf("pool pairs = %v, want %v", gotPairs, wantPairs)
		}
	}
	// Node 1 sits on the A track only: once A is banned it is unreachable, so
	// nothing may propose it.  This is the negative half of "the candidates are
	// built against a ban", and a strategy that ranked the raw hop ball instead
	// of the A-free DAG would put it in the pool.
	for _, n := range ids(got.nodes) {
		if n == 1 {
			t.Fatal("pool contains node 1, which is unreachable once arc 0 is banned")
		}
	}
}

// The engagement gate: a demand pressing only one of the round's hot arcs is
// the singleton strategies' business.  Turning the gate down to 1 keeps the
// strategy alive on such a demand, but then no second hot arc can be found on
// the new path, so every candidate it produces is bare.
func TestResidualEngagesOnlyOnTwoCrossedHotArcs(t *testing.T) {
	inst := serialLadderInstance()
	oneHot := serialFamily()
	oneHot.Hots = map[int][]int{0: {0}}

	if got := newResidualFixture(inst).pool(oneHot, Options{Mode: ModeResidual}); len(got.nodes) != 0 || len(got.pairs) != 0 {
		t.Fatalf("one crossed hot arc produced %v / %v, want nothing at the default gate",
			ids(got.nodes), pairStrings(got.pairs))
	}

	got := newResidualFixture(inst).pool(oneHot, Options{Mode: ModeResidual, ResidualMinHot: 1})
	if want := []int{2, 5}; !sameInts(ids(got.nodes), want) {
		t.Fatalf("nodes = %v, want %v (bare U's, in DAG-rank order)", ids(got.nodes), want)
	}
	if len(got.pairs) != 0 {
		t.Fatalf("pairs = %v, want none: with one hot arc there is no B to pair around",
			pairStrings(got.pairs))
	}
}

// Step 4: a candidate that does not unload A is not this strategy's material,
// however it was discovered.
func TestResidualDropsCandidatesThatDoNotUnloadA(t *testing.T) {
	fam := serialFamily()
	fam.ArcRelief = func(int, int, []float64) float64 { return 0 }
	got := newResidualFixture(serialLadderInstance()).pool(fam, Options{Mode: ModeResidual})
	if len(got.nodes) != 0 || len(got.pairs) != 0 {
		t.Fatalf("unloading nothing produced %v / %v, want nothing",
			ids(got.nodes), pairStrings(got.pairs))
	}
}

// Without ArcRelief the caller cannot name a cell, so the family-wide test
// stands in -- the strategy still runs, it just cannot insist on A.
func TestResidualFallsBackToFamilyReliefWithoutArcRelief(t *testing.T) {
	fam := serialFamily()
	fam.ArcRelief = nil
	got := newResidualFixture(serialLadderInstance()).pool(fam, Options{Mode: ModeResidual})
	if want := []int{2, 3, 5}; !sameInts(ids(got.nodes), want) {
		t.Fatalf("nodes = %v, want %v", ids(got.nodes), want)
	}
}

// Banning B can cut U off entirely.  On the plain diamond there is no third
// route, so the strategy must find nothing rather than pair a V that no ban
// context supports.
func TestResidualSkipsWhenBanningBCutsUOff(t *testing.T) {
	inst := ladderInstance() // diamond: 0->1->2->5 / 0->3->4->5
	fam := Family{
		D:      0,
		Slots:  []int{0},
		Hots:   map[int][]int{0: {0, 3}},
		Relief: alwaysRelief,
	}
	got := newResidualFixture(inst).pool(fam, Options{Mode: ModeResidual})
	if len(got.nodes) != 0 || len(got.pairs) != 0 {
		t.Fatalf("produced %v / %v on a graph with no residual, want nothing",
			ids(got.nodes), pairStrings(got.pairs))
	}
}

// Same determinism contract as the other strategies: the DAG walk, the V walk
// and the per-B grouping all iterate slices, never maps.
func TestResidualBuildIsRepeatable(t *testing.T) {
	inst := serialLadderInstance()
	g := graph.New(inst)
	ix := graph.NewIndex(g, inst.Scenario.Blocked, 0)
	rt := &fakeRouter{n: g.N}
	fam := serialFamily()
	opts := Options{Mode: ModeResidual}

	first := ""
	for i := 0; i < 64; i++ {
		alts, err := Build(rt, ix, IndexHops{IX: ix}, g, inst, fam, opts)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		s := ""
		for _, a := range alts {
			s += fmt.Sprint(a.Wps) + "|"
		}
		if i == 0 {
			first = s
			if first == "" {
				t.Fatal("Build returned no candidates; the test would pass vacuously")
			}
			continue
		}
		if s != first {
			t.Fatalf("Build call %d differs from call 1:\n got %q\nwant %q", i, s, first)
		}
	}
}

// "residual" is an ordinary mode, and any subset composes with any other: the
// list is the round-robin priority, so order is preserved and repeats collapse.
func TestModesOfComposition(t *testing.T) {
	if got := modesOf(ModeResidual); len(got) != 1 || got[0] != ModeResidual {
		t.Fatalf("modesOf(residual) = %v, want exactly itself", got)
	}
	if got := modesOf("hot_center+residual"); len(got) != 2 || got[0] != ModeHotCenter || got[1] != ModeResidual {
		t.Fatalf("modesOf(hot_center+residual) = %v, want the pair in order", got)
	}
	// The mix already runs residual, so naming it again is a no-op and must not
	// disturb the order.
	got := modesOf("mix+residual")
	if len(got) != 3 || got[2] != ModeResidual {
		t.Fatalf("modesOf(mix+residual) = %v, want the mix unchanged", got)
	}
	// bottleneck is the one strategy the mix does *not* run, so it is the one
	// that composes with it.
	got = modesOf("mix+bottleneck")
	if len(got) != 4 || got[3] != ModeBottleneck {
		t.Fatalf("modesOf(mix+bottleneck) = %v, want the mix then bottleneck", got)
	}
	// A repeat of an already-running strategy adds nothing.
	if got := modesOf("mix+od_scan"); len(got) != 3 {
		t.Fatalf("modesOf(mix+od_scan) = %v, want 3 (od_scan already in the mix)", got)
	}
	// Order is the caller's, not a canonical order.
	if got := modesOf("residual+bottleneck"); len(got) != 2 || got[0] != ModeResidual {
		t.Fatalf("modesOf(residual+bottleneck) = %v, want residual first", got)
	}
	if got := modesOf("residual+nonsense"); got != nil {
		t.Fatalf("modesOf(residual+nonsense) = %v, want nil", got)
	}
}

func TestResidualOptionDefaults(t *testing.T) {
	d := Options{}.withDefaults()
	if d.ResidualMinHot != 2 || d.ResidualU != 8 || d.ResidualV != 8 {
		t.Fatalf("residual defaults = %d/%d/%d, want 2/8/8",
			d.ResidualMinHot, d.ResidualU, d.ResidualV)
	}
}
