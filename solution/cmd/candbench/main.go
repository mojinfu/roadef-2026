// Command candbench is a microbenchmark of the candidate generator alone.
//
// The outer loop is a bad instrument for tuning cand: one round costs a Gurobi
// solve whose own run-to-run spread (measured at ~8% on setA-10 firstbit, with
// -presolve off, so it is Gurobi itself and not presolve) is far larger than
// the candidate-layer effect being measured.  This command removes the MIP from
// the loop entirely and asks the only question the generator is responsible
// for answering:
//
//	given a task (an instance, or a mid-solve snapshot of one) and a hot arc,
//	what candidate set comes out, how long did it take, and how much of the
//	brute-force-optimal node set did it keep?
//
// Input is therefore exactly the generate-round input: a snapshot, the hot
// cells, and per demand the families that load them.  Output is the candidate
// list per family plus counters.  Nothing here touches the search, the MIP, or
// the objective, so a run takes seconds, not the 90 s wall cap of a solve.
//
// The hop cache is exercised directly: -hop-cache=false reruns the identical
// family list against an uncached Hops, and the two candidate lists are
// compared element by element.  The cache memoises a pure function, so the
// lists must be bit-identical; anything else is a bug, not a tuning result.
//
// Usage:
//
//	candbench -prefix ../setA/setA-10 -hot-k 6 -recall-k 12
//	candbench -prefix ../setA/setA-01 -slot 0 -arc 37 -demand 12 -dump 20
//	candbench -prefix ../setA/setA-10 -sol ../runs/x.json   (mid-solve state)
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"tasr/internal/cand"
	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/hops"
	"tasr/internal/io"
	"tasr/internal/model"
	"tasr/internal/snap"
)

func main() {
	prefix := flag.String("prefix", "../setA/setA-01", "instance prefix, e.g. ../setA/setA-10")
	solPath := flag.String("sol", "", "optional mid-solve solution JSON (default: the empty snapshot)")

	hotK := flag.Int("hot-k", 6, "number of globally hottest cells to target (ignored with -slot/-arc)")
	slotF := flag.Int("slot", -1, "explicit hot slot (-1 = derive from the snapshot)")
	arcF := flag.Int("arc", -1, "explicit hot arc (-1 = derive from the snapshot)")
	demandF := flag.Int("demand", -1, "restrict to one demand id (-1 = every demand that loads a hot arc)")

	mode := flag.String("mode", cand.ModeMix, "candidate generator mode (mix|hot_center|od_scan|bottleneck)")
	poolCap := flag.Int("pool-cap", 24, "nodes kept per strategy pool")
	maxW1 := flag.Int("max-w1", 24, "1-waypoint budget per demand per family")
	maxW2 := flag.Int("max-w2", 32, "2-waypoint budget per demand per family")
	maxBans := flag.Int("max-bans", 3, "bottleneck: hot arcs banned per demand per slot")
	maxExtraHop := flag.Int("max-extra-hop", 4, "hop-ball growth limit")
	offHotPct := flag.Float64("offhot-pct", 0.05, "off-hot random top-up fraction of PoolCap")
	seed := flag.Int64("seed", 1, "off-hot sample seed")
	globalK := flag.Int("global-k", 0, "global-relief safety net: append up to K best-relief singletons found by scanning every node (0 = off)")

	hopCacheOn := flag.Bool("hop-cache", true, "retain hop BFS per banned-arc set (false = uncached control)")

	maxFams := flag.Int("max-families", 60, "stop after this many families")
	repeat := flag.Int("repeat", 1, "replay the family list this many times (simulates consecutive rounds reusing the cache)")
	reps := flag.Int("reps", 1, "repeat each arm this many times and report the fastest (single runs on a busy box swing 2-3x)")
	recallK := flag.Int("recall-k", 12, "legacy comparator: top-K nodes by relief over ALL nodes (0 = off)")
	indexSize := flag.Int("index-size", 0, "graph index LRU capacity (0 = size to the instance, as solve does)")
	dumpLimit := flag.Int("dump", 0, "print up to N candidates of the first families (0 = off)")
	outTSV := flag.String("out", "", "also write the per-family rows to this TSV path")
	flag.Parse()

	inst, err := io.LoadInstance(*prefix)
	if err != nil {
		fatal(err)
	}
	g := graph.New(inst)

	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	ixSize := *indexSize
	if ixSize <= 0 {
		ixSize = 4 * g.N * inst.NSlots
		if ixSize < 8192 {
			ixSize = 8192
		}
	}
	cache.Index().SetMaxSize(ixSize)

	sn, err := snapshot(inst, g, cache, *solPath)
	if err != nil {
		fatal(err)
	}

	t0 := time.Now()
	desc := eval.SortedDesc(sn.Saturations())
	fmt.Printf("instance=%s m=%d T=%d n=%d demands=%d\n",
		inst.Name, g.M, inst.NSlots, inst.NNodes(), inst.NDemands())
	fmt.Printf("  state=%s  first-bit raw=%.9f rank=%d  total_cost=%d  (%.2fs setup)\n",
		stateName(*solPath), desc[0], eval.RankInt(desc[0]), sn.TotalCost(),
		time.Since(t0).Seconds())

	hots, err := hotCells(sn, inst, *slotF, *arcF, *hotK)
	if err != nil {
		fatal(err)
	}
	sat := sn.Saturations()
	fmt.Printf("  hot cells:")
	for _, h := range hots {
		fmt.Printf(" (t=%d,arc=%d,load=%.6f)", h.T, h.A, sat[h.A*inst.NSlots+h.T])
	}
	fmt.Println()

	opts := cand.Options{
		Mode:        *mode,
		PoolCap:     *poolCap,
		MaxW1:       *maxW1,
		MaxW2:       *maxW2,
		MaxBans:     *maxBans,
		MaxExtraHop: *maxExtraHop,
		OffHotPct:   *offHotPct,
		Seed:        *seed,
		GlobalK:     *globalK,
	}

	// Arms differ only in whether the hop cache is enabled.  Same families,
	// same options, same router: only the Hops implementation moves.
	//
	// "on2" is a repeat of "on" and is the control that keeps the neutrality
	// check honest: without it, any difference between on and off is
	// attributable to *either* the cache or to nondeterminism inside cand, and
	// those two have opposite fixes.  on-vs-on2 isolates the second while
	// holding the cache constant.
	type arm struct {
		name    string
		enabled bool
	}
	var arms []arm
	if *hopCacheOn {
		arms = append(arms, arm{"on", true}, arm{"on2", true}, arm{"off", false})
	} else {
		arms = append(arms, arm{"off", false})
	}

	fams := buildFamilies(sn, inst, g, hots, *demandF, *maxFams)
	fmt.Printf("  families=%d (demand, single-slot) — each routed by every mode of %q\n\n",
		len(fams), *mode)

	// Discarded warmup.  The first pass to run pays the cold ECMP atom cache
	// and the page faults for the whole graph, which on a sub-second instance
	// is a large fraction of it -- measured at 751 ms vs 256 ms for the same
	// arm on setA-19.  Without this, whichever arm runs first looks slowest and
	// the cache arm, always first, takes the blame.  The warmup also fills the
	// shared graph.Index, so every measured arm starts from the same state.
	// It runs the real generator, so it warms the router as well.
	warm := hops.New(g, inst.Scenario.Blocked)
	runArms(sn, inst, g, cache.Index(), warm, fams, opts, 0, *mode)

	results := map[string][]famRow{}
	for _, a := range arms {
		var rows []famRow
		best := math.Inf(1)
		var h, mi, comp, sets int
		for rep := 0; rep < *reps; rep++ {
			hc := hops.New(g, inst.Scenario.Blocked)
			hc.SetEnabled(a.enabled)
			// Recall re-routes every node in the instance, so it dwarfs the
			// cache effect being timed.  Run it once (rep 0) and reuse those
			// rows: Build is deterministic, so the rows are identical anyway.
			rk := *recallK
			if rep > 0 {
				rk = 0
			}
			r, ms := runArms(sn, inst, g, cache.Index(), hc, fams, opts, rk, *mode)
			// Replay: the cache's whole point is reuse across rounds, and one
			// pass over the family list is a single round.  Repeating it
			// without changing the queries is an upper bound on that reuse --
			// consecutive rounds overlap heavily but not perfectly -- and it is
			// the only way the steady-state effect is visible at all.
			for k := 1; k < *repeat; k++ {
				_, m2 := runArms(sn, inst, g, cache.Index(), hc, fams, opts, 0, *mode)
				ms += m2
			}
			if ms < best {
				best = ms
			}
			if rep == 0 {
				rows = r
				// The BFS counters are a deterministic property of the family
				// list, so one rep is the measurement; the wall clock is not,
				// which is why only that is minimised over reps.
				hh, mm, cc := hc.Stats()
				h, mi, comp, sets = hh, mm, cc, hc.Sets()
			}
		}
		results[a.name] = rows
		fmt.Printf("== hop cache %-3s : %6.1f ms best-of-%d, %d families, BFS queries=%d hits=%d misses=%d computed=%d, sets=%d\n",
			a.name, best, *reps, len(rows), h+mi, h, mi, comp, sets)
	}

	if *dumpLimit > 0 {
		dump(sn, fams, results, *dumpLimit)
	}

	base := arms[0].name
	report(results[base], *recallK, *mode)

	// Cache neutrality: the cache is a memo of a pure function of
	// (slot, source, kind), so the two arms must agree exactly.  Compare the
	// whole candidate list per family, not just its size.  The generator's own
	// determinism is reported first, because a neutrality failure caused by
	// nondeterminism needs a different fix than one caused by the memo.
	if len(arms) == 3 {
		if d, at := compare(results["on"], results["on2"]); d == 0 {
			fmt.Printf("\ngenerator determinism: PASS — cache-on repeated gave bit-identical candidate lists on all %d families\n", len(results["on"]))
		} else {
			fmt.Printf("\ngenerator determinism: FAIL — %d/%d families differ between two identical cache-on runs (first: family %d)\n",
				d, len(results["on"]), at)
		}
		diffs, firstAt := compare(results["on"], results["off"])
		if diffs == 0 {
			fmt.Printf("cache neutrality: PASS — cache-on and cache-off produced bit-identical candidate lists on all %d families\n", len(results["on"]))
		} else {
			fmt.Printf("cache neutrality: FAIL — %d/%d families differ (first: family %d)\n",
				diffs, len(results["on"]), firstAt)
		}
	}

	if *outTSV != "" {
		if err := writeTSV(*outTSV, results[base]); err != nil {
			fatal(err)
		}
		fmt.Println("\nwrote", *outTSV)
	}
}

func stateName(sol string) string {
	if sol == "" {
		return "empty snapshot"
	}
	return "solution " + sol
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func snapshot(inst *model.Instance, g *graph.Graph, c *ecmp.Cache, solPath string) (*snap.Snap, error) {
	if solPath == "" {
		return snap.NewEmpty(inst, g, c)
	}
	sol, err := io.ReadSolution(solPath, inst)
	if err != nil {
		return nil, err
	}
	return snap.New(inst, g, c, sol)
}

// hotCells returns the (slot, arc) pairs the round would target: either the
// caller's explicit pair, or the globally hottest cells of the snapshot,
// stopping at the first cold cell exactly as the outer loop does.
func hotCells(sn *snap.Snap, inst *model.Instance, slot, arc, k int) ([]snap.Key, error) {
	if slot >= 0 || arc >= 0 {
		if slot < 0 || arc < 0 {
			return nil, fmt.Errorf("hotCells: -slot and -arc must be given together")
		}
		if slot >= inst.NSlots || arc < 0 || arc >= inst.NArcs() {
			return nil, fmt.Errorf("hotCells: (slot=%d, arc=%d) out of range", slot, arc)
		}
		return []snap.Key{{T: slot, A: arc}}, nil
	}
	sat := sn.Saturations()
	T := inst.NSlots
	var out []snap.Key
	for _, key := range sn.RankKeys(nil) {
		if sat[key.A*T+key.T] <= 0 {
			break
		}
		out = append(out, key)
		if len(out) >= k {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("hotCells: snapshot carries no load")
	}
	return out, nil
}

// familySpec is one (demand, slot) generation scope plus the hot arcs of that
// slot, which is exactly what pool.go hands cand.Build.
type familySpec struct {
	d      int
	src    int
	tgt    int
	slot   int
	target []int
}

// buildFamilies mirrors the single-slot family loop of pool.go: for every
// demand that actually loads a hot arc of some hot slot, one family per such
// slot.  Twin families are deliberately not built here -- they double the
// count for the same generator path and the single-slot family already
// exercises all three strategies.
func buildFamilies(sn *snap.Snap, inst *model.Instance, g *graph.Graph, hots []snap.Key,
	onlyDemand, maxFams int) []familySpec {

	hotBySlot := map[int][]int{}
	var slots []int
	for _, h := range hots {
		if _, seen := hotBySlot[h.T]; !seen {
			slots = append(slots, h.T)
		}
		hotBySlot[h.T] = append(hotBySlot[h.T], h.A)
	}
	sort.Ints(slots)

	var out []familySpec
	for d := 0; d < inst.NDemands(); d++ {
		if onlyDemand >= 0 && d != onlyDemand {
			continue
		}
		dem := &inst.Demands[d]
		for _, s := range slots {
			v := dem.Volume[s]
			if v == 0.0 {
				continue
			}
			wps := sn.GetWaypoints(d, s)
			u, err := sn.UnitRoute(d, s, wps)
			if err != nil {
				continue // incumbent routing disconnected at this slot
			}
			for _, ha := range hotBySlot[s] {
				if v*u[ha] > cand.FracEps {
					out = append(out, familySpec{d: d, src: dem.Source, tgt: dem.Target, slot: s, target: hotBySlot[s]})
					break
				}
			}
			if len(out) >= maxFams {
				return out
			}
		}
	}
	return out
}

// famRow is one family's measurement.
type famRow struct {
	d, src, tgt, slot int
	nhot              int
	n1, n2            int
	topRel            float64
	tags              map[string]int
	ms                float64

	// recall against the brute-force 1-waypoint enumeration
	relieving int // nodes whose singleton route strictly relieves a target
	recallK   int // min(recallK, relieving)
	recallHit int // how many of the legacy top-K are in cand's w1 output
	legacyTop float64
	candTop   float64 // best relief among cand's w1 candidates
	// candSig identifies the full candidate list for the neutrality compare.
	candSig string
}

func runArms(sn *snap.Snap, inst *model.Instance, g *graph.Graph, ix *graph.Index,
	hp cand.Hops, fams []familySpec, opts cand.Options, recallK int, mode string) ([]famRow, float64) {

	start := time.Now()
	m := g.M
	rows := make([]famRow, 0, len(fams))
	for _, f := range fams {
		vol := inst.Demands[f.d].Volume[f.slot]
		// Incumbent load block of this demand at the family slot; relief is
		// measured against exactly this, like pool.go's closure.
		wps := sn.GetWaypoints(f.d, f.slot)
		cur, err := sn.UnitRoute(f.d, f.slot, wps)
		if err != nil {
			continue
		}
		cl := make([]float64, m)
		for a := 0; a < m; a++ {
			cl[a] = vol * cur[a]
		}
		relief := func(slot int, unit []float64) float64 {
			if unit == nil {
				return -1
			}
			best := -1.0
			for _, ha := range f.target {
				if r := cl[ha] - vol*unit[ha]; r > best {
					best = r
				}
			}
			return best
		}

		famStart := time.Now()
		alts, err := cand.Build(sn, ix, hp, g, inst, cand.Family{
			D:      f.d,
			Slots:  []int{f.slot},
			Hots:   map[int][]int{f.slot: f.target},
			Relief: relief,
		}, opts)
		if err != nil {
			fatal(fmt.Errorf("demand %d slot %d: %w", f.d, f.slot, err))
		}

		row := famRow{d: f.d, src: f.src, tgt: f.tgt, slot: f.slot, nhot: len(f.target),
			tags: map[string]int{}, ms: time.Since(famStart).Seconds() * 1000}
		for _, a := range alts {
			row.tags[a.Tag]++
			if a.Rel > row.topRel {
				row.topRel = a.Rel
			}
			if len(a.Wps) == 1 {
				row.n1++
				if a.Rel > row.candTop {
					row.candTop = a.Rel
				}
			} else {
				row.n2++
			}
		}
		row.candSig = signature(alts)

		if recallK > 0 {
			row.relieving, row.recallK, row.recallHit, row.legacyTop = recall(
				sn, inst, f, cl, vol, alts, recallK)
		}
		rows = append(rows, row)
		_ = mode
	}
	return rows, time.Since(start).Seconds() * 1000
}

// recall reruns the legacy 1-waypoint enumeration for one family: every node
// except the demand's own endpoints, routed for real and kept only if it
// strictly relieves a target.  It returns how many such nodes exist, how many
// of the top-K the candidate generator proposed, and the best relief of that
// top-K.  Comparing candTop against legacyTop answers the question the pool
// size cannot: did the targeted search lose the single best node?
func recall(sn *snap.Snap, inst *model.Instance, f familySpec, cl []float64, vol float64,
	alts []cand.Alt, K int) (relieving, k, hit int, legacyTop float64) {

	type scored struct {
		w   int
		rel float64
	}
	var all []scored
	n := inst.NNodes()
	for w := 0; w < n; w++ {
		if w == f.src || w == f.tgt {
			continue
		}
		u, err := sn.UnitRoute(f.d, f.slot, []int{w})
		if err != nil {
			continue
		}
		best := -1.0
		for _, ha := range f.target {
			if r := cl[ha] - vol*u[ha]; r > best {
				best = r
			}
		}
		if best > cand.FracEps {
			all = append(all, scored{w, best})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].rel != all[j].rel {
			return all[i].rel > all[j].rel
		}
		return all[i].w < all[j].w // deterministic tie-break
	})
	relieving = len(all)
	if K > len(all) {
		K = len(all)
	}
	if K == 0 {
		return relieving, 0, 0, 0
	}
	legacyTop = all[0].rel
	inCand := map[int]bool{}
	for _, a := range alts {
		if len(a.Wps) == 1 {
			inCand[a.Wps[0]] = true
		}
	}
	for i := 0; i < K; i++ {
		if inCand[all[i].w] {
			hit++
		}
	}
	return relieving, K, hit, legacyTop
}

// signature renders a candidate list as a comparable string: waypoints plus
// the relief at full float precision.  Two lists with the same signature route
// to the same columns.
func signature(alts []cand.Alt) string {
	var b strings.Builder
	for _, a := range alts {
		fmt.Fprintf(&b, "%v@%.17g;", a.Wps, a.Rel)
	}
	return b.String()
}

func compare(on, off []famRow) (diffs, firstAt int) {
	firstAt = -1
	n := len(on)
	if len(off) < n {
		n = len(off)
	}
	for i := 0; i < n; i++ {
		if on[i].candSig != off[i].candSig {
			diffs++
			if firstAt < 0 {
				firstAt = i
			}
		}
	}
	diffs += abs(len(on) - len(off))
	return diffs, firstAt
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func dump(sn *snap.Snap, fams []familySpec, results map[string][]famRow, limit int) {
	rows := results["on"]
	if _, ok := results["on"]; !ok {
		rows = results["off"]
	}
	fmt.Println("\n--- candidate sets (mode=on) ---")
	for i := range rows {
		if i >= 4 {
			break
		}
		r := rows[i]
		fmt.Printf("\n# family demand=%d slot=%d %d->%d hots=%d: %d cands (%d w1 + %d w2)\n",
			r.d, r.slot, r.src, r.tgt, r.nhot, r.n1+r.n2, r.n1, r.n2)
		_ = limit
		fmt.Printf("  candSig=%s\n", truncate(r.candSig, 300))
		fmt.Printf("  tags=%v  topRel=%.9f\n", r.tags, r.topRel)
	}
	fmt.Println()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func report(rows []famRow, recallK int, mode string) {
	if len(rows) == 0 {
		fmt.Println("\nno families")
		return
	}
	var tot1, tot2 float64
	var sumTop float64
	tags := map[string]int{}
	var ms float64
	for _, r := range rows {
		tot1 += float64(r.n1)
		tot2 += float64(r.n2)
		sumTop += r.topRel
		ms += r.ms
		for t, c := range r.tags {
			tags[t] += c
		}
	}
	n := float64(len(rows))
	fmt.Printf("\n=== summary: mode=%s, %d families ===\n", mode, len(rows))
	fmt.Printf("candidates      : %d total, %.2f per family (%.2f w1 + %.2f w2)\n",
		int(tot1+tot2), (tot1+tot2)/n, tot1/n, tot2/n)
	fmt.Printf("mean top relief : %.9f   (sum %.6f)\n", sumTop/n, sumTop)
	fmt.Printf("candidate time  : %.1f ms total, %.2f ms per family\n", ms, ms/n)
	fmt.Printf("tag histogram   :")
	for _, t := range sortedKeys(tags) {
		fmt.Printf(" %s=%d", t, tags[t])
	}
	fmt.Println()

	// How empty is the generator's output?  A family that yields few candidates
	// is a family the MIP has little to choose from on.
	empty, thin := 0, 0
	for _, r := range rows {
		if r.n1+r.n2 == 0 {
			empty++
		} else if r.n1+r.n2 <= 2 {
			thin++
		}
	}
	fmt.Printf("degenerate      : %d empty, %d with <=2 candidates\n", empty, thin)

	if recallK > 0 {
		var k, hit, relieving int
		var lhs, rhs float64
		perfect := 0
		for _, r := range rows {
			k += r.recallK
			hit += r.recallHit
			relieving += r.relieving
			lhs += r.legacyTop
			rhs += r.candTop
			if r.recallK > 0 && r.recallHit == r.recallK {
				perfect++
			}
		}
		fmt.Printf("\n--- recall vs brute force (1-waypoint, top-%d per family) ---\n", recallK)
		fmt.Printf("relieving nodes : %d over %d families (nodes that strictly reduce a target when used alone)\n",
			relieving, len(rows))
		fmt.Printf("recall@%d        : %d/%d = %.1f%%   (perfect families: %d/%d)\n",
			recallK, hit, k, 100.0*float64(hit)/float64(maxInt(k, 1)), perfect, len(rows))
		if lhs > 0 {
			fmt.Printf("relief capture  : cand best / legacy best over families = %.4f (cand %.6f vs legacy %.6f)\n",
				rhs/lhs, rhs, lhs)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeTSV(path string, rows []famRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "demand\tsrc\ttgt\tslot\tnhot\tn_w1\tn_w2\ttop_rel\tms\trelieving\trecall_k\trecall_hit\tlegacy_top\tcand_top\ttags")
	for _, r := range rows {
		var tg []string
		for _, t := range sortedKeys(r.tags) {
			tg = append(tg, fmt.Sprintf("%s=%d", t, r.tags[t]))
		}
		fmt.Fprintf(f, "%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.9f\t%.2f\t%d\t%d\t%d\t%.9f\t%.9f\t%s\n",
			r.d, r.src, r.tgt, r.slot, r.nhot, r.n1, r.n2, r.topRel, r.ms,
			r.relieving, r.recallK, r.recallHit, r.legacyTop, r.candTop, strings.Join(tg, ","))
	}
	return nil
}

var _ = math.Abs
