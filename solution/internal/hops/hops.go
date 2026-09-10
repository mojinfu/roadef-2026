// Package hops is the hop-count cache the candidate layer runs on.
//
// The candidate strategies (internal/cand) issue hop queries by the hundred per
// round: hot_center and od_scan both build a hop ball around the demand's
// shortest path, which needs "hops from the origin to every node" and "hops
// from every node to the destination", and hot_center additionally needs an
// undirected distance from every pooled node to the endpoints of a hot arc.
// Across the demands of one round those queries overlap massively -- every
// demand sharing a source repeats the same forward BFS, every demand sharing a
// target repeats the same reverse BFS, and every slot whose maintenance set is
// identical repeats all of them.
//
// So the cache is keyed on two things, in this order:
//
//	banned-set id  an interned identity of the arc set that is down.  Slots
//	               rarely differ (most carry the same, usually empty, set), so
//	               interning by *content* collapses them to a single id and the
//	               same BFS serves every slot with the same maintenance.
//	origin node    the BFS source, which is what actually varies.
//
// Two maps are maintained, exactly as the hop primitive needs them:
//
//	forward[set][src][w]  hops src -> w   (BFS over Outs)
//	reverse[set][dst][w]  hops w -> dst   (BFS over Ins)
//
// plus a third for the undirected variant hot_center ranks with.  Lookups are
// O(1) per node afterwards, instead of a BFS per waypoint candidate.
//
// Entries are never evicted.  That is deliberate and is the difference from
// graph.Index, whose LRU is shared with the ECMP atom cache and can therefore
// evict a hop array that the next round needs again; here the working set is
// bounded by (distinct maintenance sets) x (nodes), which is small -- at n=400
// with 4 distinct sets it is a few MB across all three maps -- and paying it
// once beats re-running BFS on every round.
//
// SetEnabled(false) turns every lookup into a miss that recomputes and stores
// nothing, which is how the "with cache / without cache" benchmark comparison
// is run.  Hits, Misses and Computed count the queries so the effect can be
// reported quantitatively rather than asserted.
package hops

import (
	"hash/fnv"

	"tasr/internal/graph"
)

// key identifies one cached BFS result: which banned set, and which origin.
type key struct {
	set  int
	node int
}

// Cache memoises hop-count BFS results over a fixed graph and a per-slot
// banned-arc set list.  Not safe for concurrent use.
type Cache struct {
	g      *graph.Graph
	sets   [][]bool // interned banned sets; sets[id] is the canonical slice
	slotOf []int    // slot -> interned set id (len == len(blocked))

	enabled bool

	fwd map[key][]int
	rev map[key][]int
	und map[key][]int

	// counters (queries, not BFS calls: Hits+Misses is the query count)
	Hits     int
	Misses   int
	Computed int
}

// New builds the cache for g with the scenario's per-slot banned sets.  blocked
// may be nil (nothing is ever down); a slot beyond its length behaves as an
// empty set.
func New(g *graph.Graph, blocked [][]bool) *Cache {
	c := &Cache{
		g:       g,
		enabled: true,
		fwd:     map[key][]int{},
		rev:     map[key][]int{},
		und:     map[key][]int{},
	}
	c.slotOf = make([]int, len(blocked))
	byFingerprint := map[uint64][]int{}
	for t, bl := range blocked {
		fp := fingerprint(bl)
		id := -1
		for _, cand := range byFingerprint[fp] {
			if sameSet(c.sets[cand], bl) {
				id = cand
				break
			}
		}
		if id < 0 {
			id = len(c.sets)
			c.sets = append(c.sets, bl)
			byFingerprint[fp] = append(byFingerprint[fp], id)
		}
		c.slotOf[t] = id
	}
	return c
}

// SetEnabled toggles storing.  With it off every query recomputes and nothing
// is retained, which is the uncached control arm of the benchmark.
func (c *Cache) SetEnabled(on bool) { c.enabled = on }

// Enabled reports whether results are being retained.
func (c *Cache) Enabled() bool { return c.enabled }

// Sets returns how many distinct banned sets the scenario has.  Slots sharing
// a maintenance set share one BFS.
func (c *Cache) Sets() int { return len(c.sets) }

// Stats returns hits, misses and the number of BFS computations performed.
func (c *Cache) Stats() (hits, misses, computed int) { return c.Hits, c.Misses, c.Computed }

// Clear drops every cached array but keeps the interned sets.
func (c *Cache) Clear() {
	c.fwd = map[key][]int{}
	c.rev = map[key][]int{}
	c.und = map[key][]int{}
}

func (c *Cache) setAt(t int) int {
	if t < 0 || t >= len(c.slotOf) {
		return -1 // no maintenance anywhere: an empty set
	}
	return c.slotOf[t]
}

func (c *Cache) bannedSet(id int) []bool {
	if id < 0 || id >= len(c.sets) {
		return nil
	}
	return c.sets[id]
}

// Forward returns hops[w] = fewest arcs src -> w under the slot's banned set,
// or -1 when w is unreachable.  The slice is shared with the cache: do not
// mutate it, and do not retain it across a Clear.
func (c *Cache) Forward(set, src int) []int {
	k := key{set: set, node: src}
	if c.enabled {
		if h, ok := c.fwd[k]; ok {
			c.Hits++
			return h
		}
	}
	c.Misses++
	h := graph.HopCounts(c.g, src, c.bannedSet(set), false)
	c.Computed++
	if c.enabled {
		c.fwd[k] = h
	}
	return h
}

// Reverse returns hops[w] = fewest arcs w -> dst under the slot's banned set,
// or -1 when w cannot reach dst.
func (c *Cache) Reverse(set, dst int) []int {
	k := key{set: set, node: dst}
	if c.enabled {
		if h, ok := c.rev[k]; ok {
			c.Hits++
			return h
		}
	}
	c.Misses++
	h := graph.HopCounts(c.g, dst, c.bannedSet(set), true)
	c.Computed++
	if c.enabled {
		c.rev[k] = h
	}
	return h
}

// Undirected returns hops[w] = fewest arcs on a walk v <-> w when arcs may be
// traversed either way.  Ranking-only primitive: it feeds hot_center's "how
// close is this node to the hot arc" ordering and must never decide anything.
func (c *Cache) Undirected(set, v int) []int {
	k := key{set: set, node: v}
	if c.enabled {
		if h, ok := c.und[k]; ok {
			c.Hits++
			return h
		}
	}
	c.Misses++
	h := graph.HopCountsUndirected(c.g, v, c.bannedSet(set))
	c.Computed++
	if c.enabled {
		c.und[k] = h
	}
	return h
}

// ForwardAt / ReverseAt / UndirectedAt are the slot-addressed conveniences:
// they resolve the slot's interned banned set and delegate.  Callers inside the
// solver always know the slot, never the interned id.
func (c *Cache) ForwardAt(t, src int) []int  { return c.Forward(c.setAt(t), src) }
func (c *Cache) ReverseAt(t, dst int) []int  { return c.Reverse(c.setAt(t), dst) }
func (c *Cache) UndirectedAt(t, v int) []int { return c.Undirected(c.setAt(t), v) }

// fingerprint hashes a banned set (one bit per arc) so interning only compares
// slices that already agree on the hash.  Distinct sets are few and long, so
// building the packed bytes once per slot is cheaper than n comparisons.
func fingerprint(bl []bool) uint64 {
	packed := make([]byte, (len(bl)+7)/8)
	for i, b := range bl {
		if b {
			packed[i/8] |= 1 << uint(i%8)
		}
	}
	h := fnv.New64a()
	h.Write(packed)
	return h.Sum64()
}

func sameSet(a, b []bool) bool {
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
