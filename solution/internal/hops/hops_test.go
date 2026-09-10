package hops

import (
	"testing"

	"tasr/internal/graph"
	"tasr/internal/model"
)

// lineTestGraph builds a tiny two-slot instance:
//
//	0 -> 1 -> 2 -> 3     (arcs 0,1,2)
//	3 -> 2 -> 1          (arcs 3,4, the way back)
//
// so hops are asymmetric and an undirected BFS differs from both directions.
func lineTestGraph() *graph.Graph {
	arcs := []model.Arc{
		{ID: 0, From: 0, To: 1, Metric: 1, Capacity: 10},
		{ID: 1, From: 1, To: 2, Metric: 1, Capacity: 10},
		{ID: 2, From: 2, To: 3, Metric: 1, Capacity: 10},
		{ID: 3, From: 3, To: 2, Metric: 1, Capacity: 10},
		{ID: 4, From: 2, To: 1, Metric: 1, Capacity: 10},
	}
	inst := &model.Instance{
		Name:      "line",
		NodeNames: []string{"n0", "n1", "n2", "n3"},
		NodeIDs:   []int{0, 1, 2, 3},
		Arcs:      arcs,
		NSlots:    2,
		Scenario: model.Scenario{
			MaxSegments: 2,
			Budget:      []int{0, 0},
			// Slot 0 has nothing down, slot 1 takes arc 1 (1->2) down.  Slot 2
			// repeats slot 0's set exactly, which must intern to the same id.
			Blocked: [][]bool{
				make([]bool, len(arcs)),
				{false, true, false, false, false},
			},
		},
	}
	return graph.New(inst)
}

func TestInternsIdenticalBlockedSets(t *testing.T) {
	blocked := [][]bool{
		{false, false, false},
		{false, true, false},
		{false, false, false}, // same content as slot 0
	}
	c := New(lineTestGraph(), blocked)
	if c.Sets() != 2 {
		t.Fatalf("interned %d sets, want 2 (the two distinct contents)", c.Sets())
	}
	if c.slotOf[0] != c.slotOf[2] {
		t.Fatalf("identical blocked sets interned to %d and %d, want equal", c.slotOf[0], c.slotOf[2])
	}
	if c.slotOf[0] == c.slotOf[1] {
		t.Fatalf("different blocked sets interned to the same id %d", c.slotOf[0])
	}
}

func TestForwardReverseMatchUncachedBFS(t *testing.T) {
	g := lineTestGraph()
	c := New(g, [][]bool{nil, {false, true, false, false, false}})

	for _, tc := range []struct{ slot, src int }{{0, 0}, {0, 2}, {1, 0}, {1, 3}} {
		got := c.ForwardAt(tc.slot, tc.src)
		want := graph.HopCounts(g, tc.src, blockedAt(g, tc.slot), false)
		assertEqual(t, "Forward", tc.slot, tc.src, got, want)

		rev := c.ReverseAt(tc.slot, tc.src)
		wantRev := graph.HopCounts(g, tc.src, blockedAt(g, tc.slot), true)
		assertEqual(t, "Reverse", tc.slot, tc.src, rev, wantRev)
	}
}

// blockedAt mirrors what the cache holds for a slot; the tests keep their own
// copy so a bug in the interning cannot hide behind it.
func blockedAt(g *graph.Graph, slot int) []bool {
	if slot == 1 {
		return []bool{false, true, false, false, false}
	}
	return nil
}

func assertEqual(t *testing.T, what string, slot, src int, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s(slot=%d,src=%d): length %d != %d", what, slot, src, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s(slot=%d,src=%d): node %d = %d, want %d", what, slot, src, i, got[i], want[i])
		}
	}
}

func TestUndirectedIsSymmetricAndNotDirected(t *testing.T) {
	g := lineTestGraph()
	c := New(g, [][]bool{nil})

	// Arc 3 goes 3->2 only, so from node 3 the directed forward BFS cannot
	// reach 0, while the undirected one can (3->2->1->0 using arcs 3,4,0).
	fwd := c.ForwardAt(0, 3)
	if fwd[0] != -1 {
		t.Fatalf("forward hops 3->0 = %d, want -1 (no directed path)", fwd[0])
	}
	und := c.UndirectedAt(0, 3)
	if und[0] != 3 {
		t.Fatalf("undirected hops 3<->0 = %d, want 3", und[0])
	}
	// Symmetry: the undirected distance must not depend on which end is the root.
	back := c.UndirectedAt(0, 0)
	if back[3] != und[0] {
		t.Fatalf("undirected distance asymmetric: 3<->0 = %d but 0<->3 = %d", und[0], back[3])
	}
}

func TestCacheHitsAndUncachedControl(t *testing.T) {
	g := lineTestGraph()
	c := New(g, [][]bool{nil, {false, true, false, false, false}})

	// Two slots with identical content, six queries each: the interned set
	// means the second slot's queries are hits too.
	for i := 0; i < 6; i++ {
		c.ForwardAt(0, 0)
		c.ReverseAt(0, 3)
	}
	hits, misses, computed := c.Stats()
	if computed != 2 {
		t.Fatalf("computed %d BFS, want 2 (one forward, one reverse)", computed)
	}
	if hits != 10 || misses != 2 {
		t.Fatalf("hits=%d misses=%d, want 10/2", hits, misses)
	}

	// The uncached control must recompute every time and retain nothing.
	c.SetEnabled(false)
	before := computed
	for i := 0; i < 5; i++ {
		c.ForwardAt(0, 0)
	}
	_, _, after := c.Stats()
	if after-before != 5 {
		t.Fatalf("uncached mode computed %d BFS for 5 queries, want 5", after-before)
	}
}
