package cand

import (
	"fmt"
	"math"
	"testing"

	"tasr/internal/graph"
	"tasr/internal/model"
)

// ringInstance builds a directed ring 0->1->...->(n-1)->0, all metrics 1, one
// slot, and one demand from 0 to n/2.  On a ring the hop counts are closed-form,
// which is what lets the fake Hops below be exact rather than approximated.
func ringInstance(n int) *model.Instance {
	arcs := make([]model.Arc, n)
	for i := 0; i < n; i++ {
		arcs[i] = model.Arc{ID: i, From: i, To: (i + 1) % n, Metric: 1, Capacity: 100}
	}
	names := make([]string, n)
	ids := make([]int, n)
	for i := range names {
		names[i] = string(rune('a' + i))
		ids[i] = i
	}
	return &model.Instance{
		Name:      "ring",
		NodeNames: names,
		NodeIDs:   ids,
		Arcs:      arcs,
		NSlots:    1,
		Demands:   []model.Demand{{Source: 0, Target: n / 2, Volume: []float64{1}}},
		Scenario: model.Scenario{
			MaxSegments: 2,
			Budget:      []int{0},
			Blocked:     [][]bool{make([]bool, n)},
		},
	}
}

// fakeHops is the ring's exact hop structure (arcs 0..n-1, no arc ever down):
//
//	forward  src -> w   (w - src) mod n
//	reverse  w -> dst   (dst - w) mod n
//	undirected v <-> w  min(|w-v|, n-|w-v|)
type fakeHops struct{ n int }

func (f fakeHops) ForwardAt(_, src int) []int {
	h := make([]int, f.n)
	for w := range h {
		h[w] = ((w-src)%f.n + f.n) % f.n
	}
	return h
}

func (f fakeHops) ReverseAt(_, dst int) []int {
	h := make([]int, f.n)
	for w := range h {
		h[w] = ((dst-w)%f.n + f.n) % f.n
	}
	return h
}

func (f fakeHops) UndirectedAt(_, v int) []int {
	h := make([]int, f.n)
	for w := range h {
		d := w - v
		if d < 0 {
			d = -d
		}
		if f.n-d < d {
			d = f.n - d
		}
		h[w] = d
	}
	return h
}

// fakeRouter turns a waypoint list into a load signature that is injective on
// the waypoint list: a singleton loads 1.0 on its own arc, a pair loads 0.5 on
// each of its two arcs, the empty list loads 1.0 on arc 0.  It counts calls so
// the memoisation can be checked.
type fakeRouter struct {
	n     int
	calls int
}

func (f *fakeRouter) UnitRoute(_, _ int, wps []int) ([]float64, error) {
	f.calls++
	u := make([]float64, f.n)
	switch len(wps) {
	case 0:
		u[0] = 1
	case 1:
		u[wps[0]] = 1
	default:
		u[wps[0]] = 0.5
		u[wps[1]] = 0.5
	}
	return u, nil
}

func (f *fakeRouter) GetWaypoints(_, _ int) []int { return nil }

// alwaysRelief makes every routed candidate look like a strict improvement, so
// the tests observe the pool and budget logic rather than the relief filter.
func alwaysRelief(int, []float64) float64 { return 1.0 }

func testGraph(t *testing.T, n int) (*model.Instance, *graph.Graph, *graph.Index) {
	t.Helper()
	inst := ringInstance(n)
	g := graph.New(inst)
	return inst, g, graph.NewIndex(g, inst.Scenario.Blocked, 0)
}

// ringBallNodes is what hopBall must return for the n=12, 0->6 demand with
// maxExtra 4: the ring nodes closer to the target side.  w=7..11 give hf+ht=18
// (the long way round) and are excluded; the endpoints 0 and 6 are always
// excluded.  These are exactly 1..5.
var ringBallNodes = []int{1, 2, 3, 4, 5}

func TestHopBallIsTheExtraRadiusNeighbourhood(t *testing.T) {
	inst, _, _ := testGraph(t, 12)
	hp := fakeHops{n: 12}
	dem := &inst.Demands[0]

	ball := hopBall(hp, dem, 0, 24, 4)
	var got []int
	for _, n := range ball {
		got = append(got, n.ID)
	}
	if len(got) != len(ringBallNodes) {
		t.Fatalf("ball = %v, want %v", got, ringBallNodes)
	}
	for i := range got {
		if got[i] != ringBallNodes[i] {
			t.Fatalf("ball = %v, want %v", got, ringBallNodes)
		}
	}
	// Every node in the ball must be an actual detour: its hf+ht is the direct
	// hop count (all of 1..5 sit on a shortest 0->6 path here), never less.
	for _, n := range ball {
		if n.Hops < 6 {
			t.Fatalf("node %d: Hops %d < direct hop count 6", n.ID, n.Hops)
		}
	}
	// The endpoints must never be candidates in their own right.
	for _, n := range ball {
		if n.ID == dem.Source || n.ID == dem.Target {
			t.Fatalf("ball contains demand endpoint %d", n.ID)
		}
	}
}

func TestHopBallCapsAndOrdersByExtraCost(t *testing.T) {
	inst, _, _ := testGraph(t, 12)
	hp := fakeHops{n: 12}
	dem := &inst.Demands[0]

	// A tighter radius must be a subset of the looser one, ordered by cost.
	small := hopBall(hp, dem, 0, 3, 4)
	if len(small) != 3 {
		t.Fatalf("capped ball has %d nodes, want 3", len(small))
	}
	for i := 1; i < len(small); i++ {
		if small[i-1].Hops > small[i].Hops {
			t.Fatalf("ball not ordered by Hops: %v", small)
		}
	}
	// maxExtra 0 keeps only the nodes on a *shortest* path (same hf+ht).
	tight := hopBall(hp, dem, 0, 24, 0)
	for _, n := range tight {
		if n.Hops != 6 {
			t.Fatalf("maxExtra 0 admitted node %d with Hops %d, want 6", n.ID, n.Hops)
		}
	}
}

// The two caps must be applied to the 1-waypoint and 2-waypoint lists
// independently.  A single merged cap of 2 (what the legacy generator did)
// would spend both slots on singletons and return no pair at all.
//
// The ladder is used because the 2-waypoint side now comes only from residual,
// and this is the smallest graph where it produces one.  Its two orders (2,3)
// and (3,2) route identically under the fake router -- which is injective on
// the waypoint *set*, not the sequence -- so they collapse to a single pair.
func TestBuildAppliesTwoIndependentBudgets(t *testing.T) {
	inst := serialLadderInstance()
	g := graph.New(inst)
	ix := graph.NewIndex(g, inst.Scenario.Blocked, 0)
	fam := serialFamily()

	count := func(opts Options) (int, int) {
		t.Helper()
		alts, err := Build(&fakeRouter{n: g.N}, ix, IndexHops{IX: ix}, g, inst, fam, opts)
		if err != nil {
			t.Fatal(err)
		}
		var w1, w2 int
		for _, a := range alts {
			switch len(a.Wps) {
			case 1:
				w1++
			case 2:
				w2++
			default:
				t.Fatalf("unexpected arity %d in %v", len(a.Wps), a.Wps)
			}
		}
		return w1, w2
	}

	// Pool is {2,3,5}: three singletons for two slots, plus the one pair.
	if w1, w2 := count(Options{Mode: ModeResidual, MaxW1: 2, MaxW2: 2}); w1 != 2 || w2 != 1 {
		t.Fatalf("MaxW1 2 / MaxW2 2: got %d singletons + %d pairs, want 2 + 1", w1, w2)
	}
	// Shrinking MaxW1 must not touch the pair list: the pair is not paid for
	// out of the singleton budget.
	if w1, w2 := count(Options{Mode: ModeResidual, MaxW1: 1, MaxW2: 2}); w1 != 1 || w2 != 1 {
		t.Fatalf("MaxW1 1 / MaxW2 2: got %d singletons + %d pairs, want 1 + 1", w1, w2)
	}
}

// The budgets must be *caps*, not targets: a pool with fewer nodes than MaxW1
// must not be padded, and must not silently borrow from the other budget.
func TestBuildNeverExceedsThePool(t *testing.T) {
	inst, g, ix := testGraph(t, 12)
	rt := &fakeRouter{n: 12}
	fam := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{}, Relief: alwaysRelief}

	alts, err := Build(rt, ix, fakeHops{n: 12}, g, inst, fam, Options{
		Mode: ModeODScan, MaxW1: 100, MaxW2: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// MaxW1 is generous, so every singleton the pool offers is returned: 5 ball
	// nodes + 1 off-hot top-up (round(0.05*24)=1).  od_scan stocks the singleton
	// side only, so there are no pairs to return regardless of MaxW2.
	var w1, w2 int
	for _, a := range alts {
		if len(a.Wps) == 1 {
			w1++
		} else {
			w2++
		}
	}
	if w1 != 6 {
		t.Fatalf("got %d singletons, want 6 (5 ball nodes + 1 off-hot)", w1)
	}
	if w2 != 0 {
		t.Fatalf("got %d pairs, want 0 (od_scan emits no 2-waypoint candidates)", w2)
	}
}

// Nothing that fails to strictly relieve a target may reach the MIP.
func TestBuildFiltersZeroRelief(t *testing.T) {
	inst, g, ix := testGraph(t, 12)
	rt := &fakeRouter{n: 12}

	none := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{},
		Relief: func(int, []float64) float64 { return 0 }}
	if alts, err := Build(rt, ix, fakeHops{n: 12}, g, inst, none, Options{Mode: ModeODScan}); err != nil {
		t.Fatal(err)
	} else if len(alts) != 0 {
		t.Fatalf("zero relief produced %d candidates", len(alts))
	}

	// A negative relief (the family has no target on this slot) must also filter.
	neg := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{},
		Relief: func(int, []float64) float64 { return -1 }}
	if alts, err := Build(rt, ix, fakeHops{n: 12}, g, inst, neg, Options{Mode: ModeODScan}); err != nil {
		t.Fatal(err)
	} else if len(alts) != 0 {
		t.Fatalf("negative relief produced %d candidates", len(alts))
	}

	// Relief must be read per candidate, not once for the family: only node 3's
	// signature may survive.
	only3 := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{},
		Relief: func(_ int, u []float64) float64 {
			if u[3] == 1.0 {
				return 0.5
			}
			return 0
		}}
	alts, err := Build(rt, ix, fakeHops{n: 12}, g, inst, only3, Options{Mode: ModeODScan})
	if err != nil {
		t.Fatal(err)
	}
	if len(alts) != 1 || alts[0].Wps[0] != 3 || alts[0].Rel != 0.5 {
		t.Fatalf("got %v, want the single singleton [3] with rel 0.5", alts)
	}
}

// ModeOff is the legacy escape hatch and must never be silently accepted here.
func TestBuildRejectsOffAndUnknownModes(t *testing.T) {
	inst, g, ix := testGraph(t, 12)
	rt := &fakeRouter{n: 12}
	fam := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{}, Relief: alwaysRelief}

	if _, err := Build(rt, ix, fakeHops{n: 12}, g, inst, fam, Options{Mode: ModeOff}); err == nil {
		t.Fatal("ModeOff was accepted; the caller is supposed to branch on it")
	}
	if _, err := Build(rt, ix, fakeHops{n: 12}, g, inst, fam, Options{Mode: "nonsense"}); err == nil {
		t.Fatal("unknown mode was accepted")
	}
	if _, err := Build(rt, ix, fakeHops{n: 12}, g, inst,
		Family{D: 0, Relief: alwaysRelief}, Options{}); err == nil {
		t.Fatal("empty family slot list was accepted")
	}
	if _, err := Build(rt, ix, fakeHops{n: 12}, g, inst,
		Family{D: 0, Slots: []int{0}}, Options{}); err == nil {
		t.Fatal("nil Relief was accepted")
	}
}

// The relief reported for a family is the best across its slots, and a slot with
// no target (negative relief) must not win the max.
func TestReliefOfTakesBestSlotOnly(t *testing.T) {
	fam := Family{D: 0, Slots: []int{0, 1}, Relief: func(slot int, _ []float64) float64 {
		if slot == 0 {
			return -1 // no target on the first slot
		}
		return 0.25
	}}
	units := map[int][]float64{0: {1}, 1: {1}}
	if got := reliefOf(fam, units); got != 0.25 {
		t.Fatalf("reliefOf = %v, want 0.25", got)
	}
	// A slot missing from units is skipped, not treated as zero relief.
	if got := reliefOf(fam, map[int][]float64{0: {1}}); got != -1 {
		t.Fatalf("reliefOf with only the targetless slot = %v, want -1", got)
	}
}

func TestModesOf(t *testing.T) {
	// The mix is the two singleton strategies plus the only pair strategy.
	// bottleneck is out of it: its crossed pair is the 2-waypoint generation the
	// mix retired, so including it would put the retired shape back in the pool.
	got := modesOf(ModeMix)
	want := []string{ModeHotCenter, ModeODScan, ModeResidual}
	if !sameStrings(got, want) {
		t.Fatalf("modesOf(mix) = %v, want %v", got, want)
	}
	for _, m := range []string{ModeHotCenter, ModeODScan, ModeBottleneck, ModeResidual} {
		if got := modesOf(m); len(got) != 1 || got[0] != m {
			t.Fatalf("modesOf(%s) = %v, want exactly itself", m, got)
		}
	}
	if got := modesOf(ModeOff); got != nil {
		t.Fatalf("modesOf(off) = %v, want nil", got)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// interleave must round-robin so a capped pool still represents every strategy.
// Plain concatenate-then-truncate would return [1 2 3 4].
func TestInterleaveIsRoundRobinAndDedups(t *testing.T) {
	lists := [][]Node{
		{{ID: 1}, {ID: 2}, {ID: 3}},
		{{ID: 4}, {ID: 5}},
		{{ID: 6}},
	}
	got := ids(interleave(lists, 4))
	want := []int{1, 4, 6, 2}
	if !sameInts(got, want) {
		t.Fatalf("interleave = %v, want %v", got, want)
	}

	// Duplicates must be dropped, and dropping one must not stall the rotation.
	dup := [][]Node{{{ID: 1}, {ID: 2}}, {{ID: 1}, {ID: 3}}}
	if got := ids(interleave(dup, 10)); !sameInts(got, []int{1, 2, 3}) {
		t.Fatalf("interleave with duplicates = %v, want [1 2 3]", got)
	}

	// An empty list contributes nothing and must not spin the loop forever.
	if got := ids(interleave([][]Node{{}, {{ID: 9}}}, 5)); !sameInts(got, []int{9}) {
		t.Fatalf("interleave with an empty list = %v, want [9]", got)
	}
	if got := interleave(nil, 5); len(got) != 0 {
		t.Fatalf("interleave(nil) = %v, want empty", got)
	}
}

// The signature key is the load, not the waypoint list: two different waypoint
// lists producing the same load are the same MIP column and must collide.
func TestSigOfIsKeyedOnLoadNotWaypoints(t *testing.T) {
	a := map[int][]float64{0: {0, 1.0, 0}}
	b := map[int][]float64{0: {0, 1.0, 0}}
	if sigOf([]int{0}, a) != sigOf([]int{0}, b) {
		t.Fatal("identical loads produced different signatures")
	}
	// A zero entry is not part of the signature: {1.0,0} and {1.0} are one column.
	if sigOf([]int{0}, map[int][]float64{0: {1, 0}}) != sigOf([]int{0}, map[int][]float64{0: {1}}) {
		t.Fatal("a trailing zero changed the signature")
	}
	// Different loads must differ, including in the last ulp only.
	c := map[int][]float64{0: {0, math.Nextafter(1.0, 2.0), 0}}
	if sigOf([]int{0}, a) == sigOf([]int{0}, c) {
		t.Fatal("a one-ulp difference did not change the signature")
	}
	// Per-slot: the same load on a different slot is a different column.
	if sigOf([]int{0}, a) == sigOf([]int{1}, a) {
		t.Fatal("slot identity is missing from the signature")
	}
}

// The memo routes each distinct (slot, waypoint list) exactly once -- this is
// the fix for the legacy generator's double routing.
func TestRouteMemoRoutesOncePerKey(t *testing.T) {
	rt := &fakeRouter{n: 12}
	m := newRouteMemo(rt, 0)

	for i := 0; i < 4; i++ {
		if _, ok := m.at(0, []int{3}); !ok {
			t.Fatal("routing [3] failed")
		}
	}
	if rt.calls != 1 {
		t.Fatalf("4 identical queries caused %d routings, want 1", rt.calls)
	}
	// A different slot is a different key (the blocked sets differ).
	m.at(1, []int{3})
	// A different waypoint list is a different key.
	m.at(0, []int{3, 4})
	if rt.calls != 3 {
		t.Fatalf("calls = %d, want 3", rt.calls)
	}
	// all() routes every family slot and memoises them the same way.
	m2 := newRouteMemo(rt, 0)
	before := rt.calls
	if _, ok := m2.all([]int{0, 1}, []int{5}); !ok {
		t.Fatal("all() failed")
	}
	if rt.calls != before+2 {
		t.Fatalf("all() cost %d routings for 2 slots, want 2", rt.calls-before)
	}
}

// offhotSample must be deterministic for a given seed, must exclude the pool and
// the demand's own endpoints, and must not scale with the node count.
func TestOffhotSampleShapeAndDeterminism(t *testing.T) {
	inst, _, _ := testGraph(t, 40)
	fam := Family{D: 0, Slots: []int{0}}
	inPool := map[int]bool{}
	for w := 1; w <= 10; w++ {
		inPool[w] = true
	}
	opts := Options{PoolCap: 24, OffHotPct: 0.05, Seed: 7}.withDefaults()
	if got := len(offhotSample(inst, fam, inPool, opts)); got != 1 {
		t.Fatalf("sample size %d, want round(0.05*24)=1", got)
	}

	a := offhotSample(inst, fam, inPool, opts)
	b := offhotSample(inst, fam, inPool, opts)
	if !sameInts(a, b) {
		t.Fatalf("same seed gave %v then %v", a, b)
	}
	for _, w := range a {
		if inPool[w] {
			t.Fatalf("sample drew %d, which is already in the pool", w)
		}
		if w == inst.Demands[0].Source || w == inst.Demands[0].Target {
			t.Fatalf("sample drew demand endpoint %d", w)
		}
	}

	opts.Seed = 8
	if sameInts(a, offhotSample(inst, fam, inPool, opts)) && len(a) > 0 {
		// Not a correctness requirement, but a seed that never matters makes the
		// flag useless; on a 28-node domain a different seed should differ.
		t.Logf("seed 7 and 8 happened to draw the same node %v", a)
	}

	// When the domain is exhausted the sample must shrink, not panic.
	full := map[int]bool{}
	for w := 0; w < 40; w++ {
		full[w] = true
	}
	if got := offhotSample(inst, fam, full, opts); len(got) != 0 {
		t.Fatalf("exhausted domain returned %v, want empty", got)
	}
}

// hot_center is a singleton strategy: it stocks the pool and the pool is all it
// contributes.  It used to spend one real routing per pooled node proving which
// of them could serve as pairing material, and pair up the survivors; that
// filter never removed a node from the pool, so the two halves of this test are
// the same claim as before -- the pool is the whole ball, and the emitted
// singletons are exactly the ones that relieve -- with the pair half now
// asserting the 2-waypoint side is residual's and nobody else's.
func TestHotCenterEmitsSingletonsOnly(t *testing.T) {
	inst, g, ix := testGraph(t, 12)
	rt := &fakeRouter{n: 12}
	// Only node 2's signature counts as offloading.
	rel := func(_ int, u []float64) float64 {
		if u[2] == 1.0 {
			return 1.0
		}
		return -1
	}
	fam := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{0: {0}}, Relief: rel}

	alts, err := Build(rt, ix, fakeHops{n: 12}, g, inst, fam,
		Options{Mode: ModeHotCenter, MaxW1: 100, MaxW2: 100})
	if err != nil {
		t.Fatal(err)
	}
	// The pool still holds the whole ball -- the offload filter never removed a
	// node from it -- but only node 2 relieves, so it is the only emitted
	// singleton and MaxW2 caps an empty list.
	var w1, w2 int
	for _, a := range alts {
		if len(a.Wps) == 1 {
			w1++
		} else {
			w2++
		}
	}
	if w1 != 1 {
		t.Fatalf("got %d singletons, want 1 (only node 2 relieves)", w1)
	}
	if w2 != 0 {
		t.Fatalf("got %d pairs, want 0 (hot_center must not emit 2-waypoint candidates)", w2)
	}

	// Widen the relief to two nodes: two singletons, still no pair.  The test is
	// "loads these arcs at all", not "loads them fully", because a 2-waypoint
	// candidate would split its volume across both waypoints' arcs (0.5/0.5
	// here) -- the real relief closure is a difference of loads and admits that.
	rel2 := func(_ int, u []float64) float64 {
		if u[2] > 0 || u[3] > 0 {
			return 1.0
		}
		return -1
	}
	fam.Relief = rel2
	alts, err = Build(rt, ix, fakeHops{n: 12}, g, inst, fam,
		Options{Mode: ModeHotCenter, MaxW1: 100, MaxW2: 100})
	if err != nil {
		t.Fatal(err)
	}
	w1, w2 = 0, 0
	for _, a := range alts {
		if len(a.Wps) == 1 {
			w1++
		} else {
			w2++
		}
	}
	if w1 != 2 || w2 != 0 {
		t.Fatalf("got %d singletons + %d pairs, want 2 + 0", w1, w2)
	}
}

// TestGlobalReliefRecoversAnOutOfBallNode is the regression test for the recall
// loss the microbenchmark measured: the best singleton by relief can sit outside
// the hop ball, and no amount of ranking *inside* the ball can bring it back.
// Only the unbounded scan can.
func TestGlobalReliefRecoversAnOutOfBallNode(t *testing.T) {
	const n = 12
	inst, g, ix := testGraph(t, n)
	// Relief that values only node 9.  The 0->6 demand's maxExtra=4 ball is
	// exactly ringBallNodes (1..5), so 9 can only arrive through the net.
	onlyNine := func(_ int, u []float64) float64 { return u[9] }
	fam := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{0: {0}}, Relief: onlyNine}
	// PoolCap 4 also zeroes the off-hot top-up (round(0.05*4) = 0).  At the
	// shipping PoolCap of 24 that top-up contributes one *random* node, which is
	// itself sometimes node 9 -- it would make this test pass for the wrong
	// reason and flakily fail for no reason.
	opts := Options{Mode: ModeMix, PoolCap: 4}

	base, err := Build(&fakeRouter{n: n}, ix, fakeHops{n}, g, inst, fam, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 0 {
		t.Fatalf("net off: got %d candidates %v, want 0 -- node 9 is outside the ball",
			len(base), idsOfWps(base))
	}

	netOpts := opts
	netOpts.GlobalK = 2
	got, err := Build(&fakeRouter{n: n}, ix, fakeHops{n}, g, inst, fam, netOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("net on: got %d candidates %v, want exactly node 9", len(got), idsOfWps(got))
	}
	if len(got[0].Wps) != 1 || got[0].Wps[0] != 9 {
		t.Fatalf("net on: wps %v, want [9]", got[0].Wps)
	}
	if got[0].Tag != TagGlobal {
		t.Fatalf("net on: tag %q, want %q", got[0].Tag, TagGlobal)
	}
}

// TestGlobalReliefCapsAndEmitsSingletonsOnly pins the two shape guarantees:
// GlobalK bounds how many nodes the net contributes, and every node it
// contributes is a singleton (the 2-waypoint side is what makes the MIP
// expensive and is not where the recall loss was measured).
func TestGlobalReliefCapsAndEmitsSingletonsOnly(t *testing.T) {
	const n = 12
	inst, g, ix := testGraph(t, n)
	// Relief that values exactly the out-of-ball tail 7..11, so the strategy
	// pools contribute nothing and every emitted candidate comes from the net.
	tail := func(_ int, u []float64) float64 {
		s := 0.0
		for _, w := range []int{7, 8, 9, 10, 11} {
			s += u[w]
		}
		return s
	}
	fam := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{}, Relief: tail}
	// PoolCap 4 zeroes the off-hot top-up (see above), so every emitted candidate
	// really does come from the net.
	got, err := Build(&fakeRouter{n: n}, ix, fakeHops{n}, g, inst, fam,
		Options{Mode: ModeMix, PoolCap: 4, GlobalK: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates %v, want 3 (GlobalK)", len(got), idsOfWps(got))
	}
	// Equal relief throughout, so the stable sort keeps ascending node order.
	want := []int{7, 8, 9}
	for i, a := range got {
		if len(a.Wps) != 1 {
			t.Fatalf("candidate %d has %d waypoints, want 1: the net must not emit pairs", i, len(a.Wps))
		}
		if a.Wps[0] != want[i] {
			t.Fatalf("candidate %d is node %d, want %d", i, a.Wps[0], want[i])
		}
		if a.Tag != TagGlobal {
			t.Fatalf("candidate %d has tag %q, want %q", i, a.Tag, TagGlobal)
		}
	}
}

// TestGlobalReliefIsAdditive: switching the net on must not remove anything the
// mode alone produced, or an A/B that flips GlobalK would be uninterpretable.
// The net is appended after the round-robin merge and is not offered as pairing
// material, so the strategy pools keep their exact composition.
func TestGlobalReliefIsAdditive(t *testing.T) {
	const n = 12
	inst, g, ix := testGraph(t, n)
	fam := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{}, Relief: alwaysRelief}
	// PoolCap 3 caps the ball at {1,2,3}; OffHotPct is left at its default, which
	// contributes 0 nodes at this pool size, so the two builds differ only by the
	// net.  MaxW1 is roomy enough that no appended singleton is truncated away.
	opts := Options{Mode: ModeMix, PoolCap: 3, MaxW1: 10, MaxW2: 10}

	off, err := Build(&fakeRouter{n: n}, ix, fakeHops{n}, g, inst, fam, opts)
	if err != nil {
		t.Fatal(err)
	}
	onOpts := opts
	onOpts.GlobalK = 4
	on, err := Build(&fakeRouter{n: n}, ix, fakeHops{n}, g, inst, fam, onOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(off) == 0 {
		t.Fatal("net off: no candidates at all; the test setup is wrong")
	}

	have := map[string]bool{}
	for _, a := range on {
		have[wpsKey(a.Wps)] = true
	}
	added := 0
	for _, a := range on {
		if a.Tag == TagGlobal {
			added++
		}
	}
	for _, a := range off {
		if !have[wpsKey(a.Wps)] {
			t.Fatalf("net on dropped %v, which the mode alone produced", a.Wps)
		}
	}
	// The net's own top-4 by relief are nodes 1..4; 1..3 are already pooled, so
	// exactly node 4 is new.  GlobalK must not add more than it promises.
	if added != 1 {
		t.Fatalf("net added %d nodes, want 1 (node 4; 1..3 were already pooled)", added)
	}
	if !have[wpsKey([]int{4})] {
		t.Fatalf("net on did not add node 4: got %v", idsOfWps(on))
	}
}

// TestGlobalReliefDoesNotDisplacePoolSingletons is the regression test for the
// setA-04 tail loss.  The net exists to fix the FIRST component; the pool's
// long detours are what tie the layers after it.  When a net node outranks a
// pool node by relief and both compete for the same MaxW1 slots, the net
// silently evicts the pool's tail-oriented candidates -- measured as tie layers
// 10 -> 1 at an unchanged first bit.  The net therefore gets its own budget.
func TestGlobalReliefDoesNotDisplacePoolSingletons(t *testing.T) {
	const n = 12
	inst, g, ix := testGraph(t, n)
	// Nodes 1..5 (the ball) relieve 1.0 each; node 9 -- outside the ball, so it
	// can only come from the net -- outranks them all at 2.0.
	outranks := func(_ int, u []float64) float64 {
		s := 2 * u[9]
		for _, w := range []int{1, 2, 3, 4, 5} {
			s += u[w]
		}
		return s
	}
	fam := Family{D: 0, Slots: []int{0}, Hots: map[int][]int{}, Relief: outranks}
	// PoolCap 6 keeps the whole ball; MaxW1 3 forces the truncation that used to
	// let the net's higher-relief node evict a pool node.
	opts := Options{Mode: ModeMix, PoolCap: 6, MaxW1: 3, GlobalK: 2}

	got, err := Build(&fakeRouter{n: n}, ix, fakeHops{n}, g, inst, fam, opts)
	if err != nil {
		t.Fatal(err)
	}
	var singles []int
	hasNine := false
	for _, a := range got {
		if len(a.Wps) != 1 {
			continue
		}
		singles = append(singles, a.Wps[0])
		if a.Wps[0] == 9 {
			hasNine = true
		}
	}
	if !hasNine {
		t.Fatalf("net node 9 missing from %v", singles)
	}
	// The pool's top MaxW1 singletons must survive alongside it, not underneath it.
	for _, w := range []int{1, 2, 3} {
		found := false
		for _, s := range singles {
			found = found || s == w
		}
		if !found {
			t.Fatalf("net displaced pool singleton %d: singles are %v, want 1,2,3 plus 9", w, singles)
		}
	}
	if len(singles) != 4 {
		t.Fatalf("got %d singletons %v, want 4 (MaxW1 = 3 pool + 1 net)", len(singles), singles)
	}
}

func wpsKey(w []int) string { return fmt.Sprint(w) }

func idsOfWps(alts []Alt) [][]int {
	out := make([][]int, len(alts))
	for i, a := range alts {
		out[i] = a.Wps
	}
	return out
}

func ids(ns []Node) []int {
	out := make([]int, len(ns))
	for i, n := range ns {
		out[i] = n.ID
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ladderInstance builds a diamond with two disjoint 3-arc routes from 0 to 5:
//
//	0 -> 1 -> 2 -> 5
//	0 -> 3 -> 4 -> 5
//
// Every arc lies on some shortest 0->5 walk, so all six are "tight" and
// crossedHotArcs returns all six.  With MaxBans below that, which arcs get
// banned decides the whole detour pool -- which is exactly the property this
// file needs to pin down.
func ladderInstance() *model.Instance {
	arcs := []model.Arc{
		{ID: 0, From: 0, To: 1, Metric: 1, Capacity: 100},
		{ID: 1, From: 1, To: 2, Metric: 1, Capacity: 100},
		{ID: 2, From: 2, To: 5, Metric: 1, Capacity: 100},
		{ID: 3, From: 0, To: 3, Metric: 1, Capacity: 100},
		{ID: 4, From: 3, To: 4, Metric: 1, Capacity: 100},
		{ID: 5, From: 4, To: 5, Metric: 1, Capacity: 100},
	}
	names := []string{"n0", "n1", "n2", "n3", "n4", "n5"}
	ids := []int{0, 1, 2, 3, 4, 5}
	return &model.Instance{
		Name:      "ladder",
		NodeNames: names,
		NodeIDs:   ids,
		Arcs:      arcs,
		NSlots:    1,
		Demands:   []model.Demand{{Source: 0, Target: 5, Volume: []float64{1}}},
		Scenario: model.Scenario{
			MaxSegments: 2,
			Budget:      []int{0},
			Blocked:     [][]bool{make([]bool, len(arcs))},
		},
	}
}

// Go randomises map iteration order on every pass, so a generator that ranged a
// map built from the hot arcs would ban a different subset each call -- and the
// banned arcs are what the detour pool is made of.  Build must be a pure
// function of its inputs; this is the regression test for the crossedHotArcs
// ordering fix, where the cross-arc loop ranged a set instead of the arcs slice.
//
// The failure mode is silent and looks like a tuning result: two runs of the
// same configuration disagree, so an A/B comparison measures the RNG rather
// than the change.
func TestBuildIsRepeatableUnderMapOrderRandomisation(t *testing.T) {
	inst := ladderInstance()
	g := graph.New(inst)
	ix := graph.NewIndex(g, inst.Scenario.Blocked, 0)
	rt := &fakeRouter{n: len(inst.NodeNames)}
	fam := Family{
		D:      0,
		Slots:  []int{0},
		Hots:   map[int][]int{0: {0, 1, 2, 3, 4, 5}},
		Relief: alwaysRelief,
	}
	opts := Options{
		Mode:    ModeBottleneck,
		PoolCap: 24,
		MaxW1:   64,
		MaxW2:   64,
		MaxBans: 2, // strictly fewer than the six crossed arcs
	}
	sig := func(alts []Alt) string {
		s := ""
		for _, a := range alts {
			for _, w := range a.Wps {
				s += string(rune('a' + w))
			}
			s += "|"
		}
		return s
	}

	first := ""
	for i := 0; i < 64; i++ {
		alts, err := Build(rt, ix, IndexHops{IX: ix}, g, inst, fam, opts)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if i == 0 {
			first = sig(alts)
			if first == "" {
				t.Fatalf("Build returned no candidates; the test would pass vacuously")
			}
			continue
		}
		if got := sig(alts); got != first {
			t.Fatalf("Build call %d differs from call 1:\n got %q\nwant %q", i, got, first)
		}
	}
}
