// Command solve runs the M3 outer loop of the V1.0 solver on one instance.
//
// It starts from an empty routing snapshot and iterates decompose-MIP rounds:
// pick the globally hottest (slot, arc) that is not marked done, build a
// candidate pool of single-waypoint reroutes that relieve it, solve the
// lexicographic-peeling selection MIP (subject to the Hamming budget), accept
// the result only when the real truncated-lex saturation vector improves.
//
// Before the first round the presolve (internal/presolve) freezes the
// structurally immovable cells: those every routing must load, whatever the
// waypoints.  They leave the hot list for good and their saturation is a hard
// lower bound on the first bit.
//
// Seed bookkeeping follows the design doc: a seed that the MIP cannot improve
// is counted, and after failLimit failures it is marked done and skipped.
// done/fail memory is cleared only when the first bit (max saturation) strictly
// improves, because then every "this arc cannot be moved" conclusion is void.
// Presolve pins are exempt from that clearing -- a structural proof does not
// expire when the routing improves.
//
// An accepted round that the MIP proved optimal can be followed by one call to
// the local search (internal/local), a self-contained pass that looks for
// detour waypoints on the hottest cells.  It only ever consumes the accepted --
// hence legal -- incumbent, and only ever keeps moves that are themselves lex
// improving and budget feasible, so it is a pure post-processing of the round
// rather than a participant in it.
//
// The pass is OFF by default (-local-search=true turns it on).  On setA it is
// nearly free -- 3-86ms per call, 0.2-2.0% of the wall clock -- but the cost is
// driven by the per-round demand scan, and on setB-01 (6519 demands, 12 slots)
// a single call costs 1.5-1.9s.  At that price it competes with the rounds for
// the same wall budget, and on setB-01 the arm with it on ended far worse at
// the first bit (704559 vs 531282).  That pair is not run-to-run noise: a
// repeated arm reproduces the trajectory round for round, and the split opens
// at the first round LS touches (see
// experiments/2026-09-11_05_setB01_local_search).  One instance and one seed
// still is not a verdict, so the flag stays -- but the default is the arm that
// was measured to be safe.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tasr/internal/cand"
	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/hops"
	"tasr/internal/io"
	"tasr/internal/local"
	"tasr/internal/mip"
	"tasr/internal/model"
	"tasr/internal/monitor"
	"tasr/internal/presolve"
	"tasr/internal/sched"
	"tasr/internal/snap"
)

// outputReserve is held back from every wall-clock budget for the work that
// happens after the search stops: writing the solution, the sprint comparison
// and process teardown.  A solution that is never written scores nothing, so
// this is the one part of the budget that must not be spent by the search.
const outputReserve = 10 * time.Second

func main() {
	// t0 is the origin of the wall clock.  The competition timer starts when the
	// solver is launched, not when the search loop begins, so a budget must
	// cover instance loading, the empty-solution ECMP pass and presolve too --
	// otherwise a slow presolve silently eats the search budget (or blows the
	// limit outright while the loop is still waiting to start counting).
	t0 := time.Now()
	prefix := flag.String("prefix", "", "instance prefix, e.g. setA/setA-01")
	out := flag.String("out", "", "write final srpaths to this file")
	sprint := flag.String("sprint", "", "sprint loads_vector.csv to compare final vector against")
	rounds := flag.Int("rounds", 400, "max decompose-MIP rounds")
	peel := flag.Int("peel", 6, "lex peel depth per MIP")
	failLimit := flag.Int("fail-limit", 2, "not-proven attempts before a seed is frozen (miss-fail-max, design doc §21)")
	hotK := flag.Int("hot-k", 6, "number of hot arcs of the focus slot decomposed together")
	schedule := flag.String("schedule", "hot", "round schedule: hot (legacy: every round attacks the globally hottest cell) or pingpong (design doc §14: hot/early parity, focus-N epoch, scale escalator)")
	focusN := flag.Int("focus-n", 10, "pingpong: freeze the top-N live cells as the epoch focus; a hot round may only attack one of them (0 = plain pingpong, no epoch, so a retired cell never comes back)")
	epochTopN := flag.Int("epoch-top-n", 12, "pingpong: end the epoch once the globally hottest N cells are all retired (frozen, skipped or physically pinned), ranked *before* the retired set is subtracted; the first N layers of the load vector are exactly these cells, so an epoch that can no longer move any of them has nothing to aim at. 0 disables the test and leaves the focus-exhaustion rule alone")
	growMult := flag.Int("grow-mult", 2, "pingpong: scale multiplier applied when an epoch restarts")
	scaleCap := flag.Int("scale-cap", 8, "pingpong: ceiling on the search scale (the escalator walks 1,2,4,...,cap; 8 is the widest that still fits the memory budget, since a round's candidate columns grow with hot-k times the waypoint caps); an exhausted epoch that cannot grow stops instead of unfreezing")
	widthStep := flag.Int("width-step", 10, "pingpong: per-scale-step increment of the per-demand candidate budgets -cand-max-w1 and -cand-max-w2 (scale s gives base + step*(s-1), so 24/32 -> 34/42 -> 44/52; 0 = the built-in default). Replaces the old multiplicative law, which compounded with hot-k")
	confGate := flag.Bool("conf-gate", true, "freeze a seed on accumulated confidence (cover**2 + 0.1 per proven-optimal miss, so a purely local miss costs 10 rounds and a network-wide one 1, doc §15) rather than counting every rejection the same; false restores the legacy fail-limit rule for a bit-for-bit A/B control")
	dualProbe := flag.Int("dual-probe", 0, "diagnostic: peel the round's pool as LP relaxations and dump the tracked cells' dual prices (Pi) for the first N rounds, 0 = off. Observation only -- it changes no decision, but it does spend wall-clock time, so a run with it on is not comparable to one without")
	dualPeel := flag.Int("dual-peel", 3, "diagnostic: how many LP peel layers -dual-probe walks (layer 0 is the floored model the solver's own first layer sees; later layers pin the previous layer's max and free z, which is where the prices are live)")
	stickySpan := flag.Int("sticky-span", 1, "sticky: copy the seed waypoint at most this many slots forward")
	budgetMode := flag.String("budget-mode", "full", "Hamming budget mode: full (spend all remaining each round) or first_half (round 1 spends half of the movable remaining budget)")
	wallSec := flag.Int("wall-sec", 0, "total wall-clock budget in seconds, counted from process start (0 = unlimited)")
	mipSec := flag.Float64("mip-sec", 20, "per peel-layer Gurobi time cap in seconds (0 = solve to completion)")
	monitorOn := flag.Bool("monitor", true, "serve a live progress page at http://127.0.0.1:<port> and open the browser (pass -monitor=false for headless batch runs)")
	monitorPort := flag.Int("monitor-port", 8765, "live view port (0 = pick a free port; busy port falls back to free)")
	monitorTop := flag.Int("monitor-top", 10, "bars per time slot on the live view page")
	monitorTopN := flag.Int("monitor-topn", 10, "top-N hottest cells listed at the top of the live view page")
	monitorLinger := flag.Int("monitor-linger", 45, "seconds to keep the live view alive after solving stops (0 = exit immediately)")
	presolveMode := flag.String("presolve", "unmovable", "presolve mode: unmovable (structural class 1+2 scan + budgeted exact pass) or off")
	presolveSec := flag.Float64("presolve-time", 15, "global presolve time budget in seconds")
	presolveDeep := flag.Int("presolve-deep", 100, "exact (class 3) presolve pass: max cells examined, 0 = as many as the budget allows")
	presolvePins := flag.String("presolve-pins", "", "dump the presolve pin table as TSV to this path")
	skipFloor := flag.Bool("skip-floor", true, "do not seed a round on a cell whose load sits on its presolve floor (it can never be relieved)")
	candMode := flag.String("cand-mode", "mix", "candidate generator: mix|hot_center|od_scan|bottleneck|residual (targeted), any \"+\"-joined subset of those (e.g. mix+bottleneck), or off (legacy brute-force enumeration). mix = hot_center+od_scan+residual")
	candPoolCap := flag.Int("cand-pool-cap", 24, "candidate: nodes kept per strategy pool")
	candW1 := flag.Int("cand-max-w1", 24, "candidate: max 1-waypoint candidates per demand per family (independent of -cand-max-w2)")
	candW2 := flag.Int("cand-max-w2", 32, "candidate: max 2-waypoint candidates per demand per family (independent of -cand-max-w1)")
	candBans := flag.Int("cand-max-bans", 3, "candidate bottleneck: max hot arcs banned per demand per slot")
	candSeed := flag.Int64("cand-seed", 0, "candidate: deterministic seed for the off-hot random top-up")
	candGlobalK := flag.Int("cand-global-k", 0, "candidate: global-relief safety net -- append up to K best-relief singletons found by scanning every node, beyond the strategy pools (0 = off)")
	candResidualMinHot := flag.Int("cand-residual-min-hot", 2, "candidate residual: hot arcs the demand's current path must press for the strategy to engage (2 = its premise: one hot arc is a singleton's job, not a pair's)")
	candResidualU := flag.Int("cand-residual-u", 8, "candidate residual: first turning points taken from the A-free shortest-path DAG, per demand per slot")
	candResidualV := flag.Int("cand-residual-v", 8, "candidate residual: second turning points taken per first point from the {A,B}-free residual path")
	candHopCache := flag.Bool("cand-hop-cache", true, "candidate: retain hop-count BFS results per banned-arc set (false = recompute every query, the uncached control)")
	haloOn := flag.Bool("halo", true, "halo: on a triggering round, also unfreeze the demands sitting on the arcs this round's own detours newly load, so the MIP can move them out of the way instead of relieving one hot corner by filling the next")
	haloMult := flag.Int("halo-mult", 1, "halo: size cap, as a multiple of the base pool's demand count (the non-halo budget) -- bounds both the radiation cells targeted and the demands admitted.  Measured 2026-09-11: 1 dominates 2 and 3 on every instance swept (tie-layer count never lower, more rounds, 1.6-1.9x fewer hop queries); 3 pulled in ~4300 pairs per round for an instance whose whole demand count is that size, then discarded 86% of them at merge time -- see experiments/2026-09-11_03_halo_mult_sweep")
	haloProb := flag.Float64("halo-prob", 0.30, "halo: per-round trigger probability; a triggered round builds the whole halo pool")
	haloAdmit := flag.Float64("halo-admit", 1.0, "halo: within a triggered round, probability that any one crowded-out demand enters the halo pool (each demand rolls independently). 1 = admit all, which is the measured default; sampling the demand set instead was tried on 2026-09-11 and was weaker -- see experiments/2026-09-11_02_sticky_only_halo_ab")
	haloSeed := flag.Int64("halo-seed", 0, "halo: seed of the trigger and admission rolls (each draw is a hash of seed and round, plus the demand index for the admission roll, so a decision never depends on how many rounds or demands came before it)")
	localOn := flag.Bool("local-search", false, "local search: after a round the MIP proved optimal, try one detour waypoint per demand sitting on the hottest cells, keeping only lex-improving budget-feasible moves. OFF by default: it is nearly free on setA but costs 1.5-1.9s per call on setB-01, where the one measured arm pair came out worse with it on")
	localCallSec := flag.Float64("local-search-call-sec", 3, "local search: hard wall-clock cap of one call in seconds (the round loop stops at it, and a round with no improvement ends the call, so this is a ceiling rather than the expected cost)")
	localRounds := flag.Int("local-search-rounds", 3, "local search: max rounds per call")
	localBudgetFrac := flag.Float64("local-search-budget-frac", 0.20, "local search: share of -wall-sec the whole feature may spend before it stops being called (0 = unlimited)")
	localTopCells := flag.Int("local-search-top-cells", 20, "local search: attack surface size -- how many of the hottest cells a round considers")
	localSatMin := flag.Float64("local-search-sat-min", 0.01, "local search: saturation floor of the attack surface; cells at or below it are ignored")
	localProbes := flag.Int("local-search-probes", 5, "local search: max detour waypoints probed per demand")
	indexSize := flag.Int("index-size", 0, "graph index LRU capacity (0 = size to the instance: 4*n*T)")
	atomSize := flag.Int("atom-size", 0, "ECMP atom LRU capacity in entries (0 = size it to 800 MB of atom vectors, i.e. 800MB/(8*m); one entry is m float64s). It must stay bounded: the atom key is (u, v, slot) over arbitrary node pairs, so its key space is n^2*T and an unbounded cache grows without limit over a long run -- the tuning table is in the comment where the default is computed")
	heapProfile := flag.String("heap-profile", "", "diagnostic: write a pprof heap profile to this path when the search ends (empty = off; the profile is observation only and changes no decision)")
	flag.Parse()
	if *prefix == "" {
		fmt.Fprintln(os.Stderr, "usage: solve -prefix setA/setA-01 [-out sol.json] [-sprint loads_vector.csv]")
		os.Exit(2)
	}

	// stopAt is the single deadline every phase below derives from: presolve
	// gets a slice of it, each MIP layer gets a slice of it, and the round loop
	// stops at it.  Nothing invents a budget of its own -- a phase that runs out
	// of its slice returns what it has and the caller moves on.  Zero means no
	// budget at all (experiments), in which case the loop is bounded by -rounds.
	var stopAt time.Time
	if *wallSec > 0 {
		stopAt = t0.Add(time.Duration(*wallSec)*time.Second - outputReserve)
	}

	inst, err := io.LoadInstance(*prefix)
	if err != nil {
		fatal(err)
	}

	// --- Emergency exit -------------------------------------------------------
	// Every phase is supposed to respect stopAt, but the whole point of a
	// backstop is the case where one of them cannot: instance loading or the
	// empty-solution ECMP pass stalling on a big setB instance, a Gurobi call
	// that ignores its TimeLimit, a loop nobody expected to be slow.  That is
	// the failure that costs the whole instance -- the process runs past the
	// competition limit and scores zero while holding a perfectly good solution.
	//
	// So a watchdog fires just after stopAt (inside the wall clock, with the
	// output reserve left to write) and writes the best solution found so far.
	// It is armed here, before the empty snapshot is even routed, because that
	// routing is itself one of the phases that can stall: the fallback is the
	// empty waypoint list, i.e. every demand on its shortest path, which is the
	// solution the solver starts from.  A mediocre written solution beats no
	// solution at all, which is what a missed deadline scores.
	//
	// The published value is a *deep copy* (Snap.Solution copies every waypoint
	// slice), so the write cannot race the search.
	publishBest(emptySolutionFor(inst))
	// stopAt + 8s lands 2s before the real wall clock when wall-sec is set, so a
	// normal shutdown has long finished and the timer is just garbage-collected.
	// The floor of 1s keeps an already-blown budget from arming an instant timer
	// that would race the normal write.
	if *out != "" && !stopAt.IsZero() {
		delay := time.Until(stopAt) + 8*time.Second
		if delay < time.Second {
			delay = time.Second
		}
		time.AfterFunc(delay, func() {
			s := emergencySolution()
			fmt.Fprintf(os.Stderr,
				"solve: still running past stopAt; writing best solution and exiting (wall-sec %d)\n", *wallSec)
			// writeOut claims the file: if a slow normal shutdown is already
			// writing it, the emergency path must not interleave, or the two
			// writers would tear the very file the fallback exists to produce.
			// A false return with no error means the normal path won the race
			// and the file on disk is already good, so that is still a success.
			if !writeOut(*out, s, inst) && writeFailed() {
				fmt.Fprintln(os.Stderr, "solve: emergency write failed (see the error above)")
				os.Exit(1)
			}
			os.Exit(0)
		})
	}

	g := graph.New(inst)
	// Size the graph index *before* the first snapshot exists.  The empty
	// snapshot routes every (demand, slot) pair, i.e. one distance array per
	// distinct target per slot, and on setB (n*t well past the 2048 default)
	// building it against the default bound thrashes: every atom misses its
	// distance array and pays a fresh Dijkstra.  Measured on setB-11 that was
	// 94s of pure startup -- 16% of the competition budget spent before the
	// search even begins, and the same cost again on every round's trial
	// snapshot.  With the index sized to the instance it is seconds.
	// The candidate layer then shares this same index, so hop and distance
	// queries reuse what the atom code already paid for instead of duplicating
	// it -- which is why the bound must cover both users.
	ixSize := *indexSize
	if ixSize <= 0 {
		ixSize = 4 * g.N * inst.NSlots
		if ixSize < 8192 {
			ixSize = 8192
		}
	}
	// The atom LRU gets its own bound, and it is not optional: its key is
	// (u, v, t) with u and v *arbitrary* nodes -- UnitRoute asks for a segment
	// per leg of the route, so the waypoints, not the arcs, decide the pairs --
	// which makes the key space n^2*T, not m*T, and every accepted round's
	// rerouteAll re-requests the whole incumbent routing.  Left unbounded (the
	// old 1<<30) every pair the search ever touches is retained for the whole
	// run: a 190s setB-01 pingpong run held 1973 MB of splitAtom vectors, 97%
	// of its live heap, and the growth is linear in rounds, so a 600s run
	// reaches double digits of GB and dies.
	//
	// The bound is set in bytes, not entries, because an entry is m float64s
	// and m is what varies across instances -- an entry is 6.9 KB on setB-01
	// (m=864) but 40 KB on setB-11 (m=5036), so a fixed count would grow
	// exactly where the budget is tightest.  The count that matters is the
	// incumbent's segment set, which is why the index-sized default
	// (4*n*T = 12672 on setB-01) thrashed:
	// measured on setB-01 at a 190s wall, 12672 entries gave a 74% hit rate,
	// 4.14M evictions and 68 rounds against the unbounded arm's 104, and the
	// first bit came out one rank worse (531544 vs 531297).  50000 entries
	// recovered the first bit at 87 rounds, 200000 gave 98% hits and 95 rounds.
	// The default below sits between them; the ceiling is what keeps a 600s run
	// alive, and a miss costs a splitAtom over m arcs (plus a Dijkstra only if
	// the index also evicted), so evicting is cheap next to the alternative.
	atomCap := *atomSize
	if atomCap <= 0 {
		const atomBytesDefault = 800 << 20
		atomCap = atomBytesDefault / (8 * g.M)
		if atomCap < 1<<13 {
			atomCap = 1 << 13
		}
	}
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, atomCap)
	cache.Index().SetMaxSize(ixSize)
	sn, err := snap.NewEmpty(inst, g, cache)
	if err != nil {
		fatal(err)
	}

	// Hop cache for the candidate layer: it interns the banned-arc sets, so
	// every slot with the same maintenance set shares one BFS, and it never
	// evicts.
	hopCache := hops.New(g, inst.Scenario.Blocked)
	hopCache.SetEnabled(*candHopCache)
	candOpts := cand.Options{
		Mode:    *candMode,
		PoolCap: *candPoolCap,
		MaxW1:   *candW1,
		MaxW2:   *candW2,
		MaxBans: *candBans,
		Seed:    *candSeed,
		GlobalK: *candGlobalK,

		ResidualMinHot: *candResidualMinHot,
		ResidualU:      *candResidualU,
		ResidualV:      *candResidualV,
	}

	// Local search (internal/local): the post-accept improvement pass.  It is
	// built once, here, so it shares the graph index the ECMP atom cache already
	// pays for instead of duplicating the Dijkstras.  ls == nil -- the default --
	// means the round loop never calls it.
	var ls *local.Searcher
	if *localOn {
		ls = local.New(inst, g, cache.Index(), local.Options{
			Rounds:     *localRounds,
			CallBudget: time.Duration(*localCallSec * float64(time.Second)),
			SatMin:     *localSatMin,
			TopCells:   *localTopCells,
			MaxProbes:  *localProbes,
			Deadline:   stopAt,
		})
	}

	bestSnap := sn
	bestDesc := eval.SortedDesc(bestSnap.Saturations())
	bestFirst := eval.RankInt(bestDesc[0])
	m, T := g.M, inst.NSlots
	fmt.Printf("instance=%s m=%d T=%d demands=%d\n", inst.Name, m, T, inst.NDemands())
	fmt.Printf("  start first-bit raw=%.9f rank=%d  total_cost=%d\n",
		bestDesc[0], bestFirst, bestSnap.TotalCost())

	publishBest(bestSnap.Solution()) // emergency copy = the routed empty snapshot

	// --- Presolve: freeze the structurally immovable (slot, arc) cells -------
	// One pass, once, over the starting snapshot: the structural argument holds
	// for any snapshot, and the cells it proves carry their load forever.
	pMode, err := presolve.ParseMode(*presolveMode)
	if err != nil {
		fatal(err)
	}
	// Presolve gets -presolve-time, but never more than what is left before
	// stopAt: on a big setB instance the structural pass alone can spend
	// minutes, and that must not come out of the search budget (let alone push
	// the process past the wall clock).  With no room left it is skipped rather
	// than started: a partial structural scan is not worth a lost round.
	pTime := time.Duration(*presolveSec * float64(time.Second))
	if !stopAt.IsZero() {
		if rem := time.Until(stopAt); rem < pTime {
			pTime = rem
		}
	}
	if pTime <= 0 {
		// Said out loud: a budget small enough to swallow presolve would
		// otherwise leave no trace of why the floors vanished.
		fmt.Printf("presolve: skipped, no wall-clock room left before stopAt\n")
		pMode = presolve.Off
	}
	pOpts := presolve.Options{
		Mode:      pMode,
		TimeLimit: pTime,
		MaxDeep:   *presolveDeep,
	}
	bounds := presolve.NewBounds()
	judge := presolve.NewJudge(inst, g, pOpts)
	// preFrozen survives every done/fail reset below: it is a proof, not a
	// heuristic memory.
	preFrozen := map[snap.Key]bool{}
	floors := map[snap.Key]float64{} // sound per-cell load floors (all bounds)
	satFloor := 0.0                  // sound lower bound on the first bit
	if pMode != presolve.Off {
		report, err := presolve.Run(inst, g, sn, bounds, pOpts)
		if err != nil {
			fatal(err)
		}
		for _, k := range bounds.ProvenKeys() {
			preFrozen[k] = true
		}
		floors = bounds.FloorMap()
		satFloor = report.LBSat
		fmt.Printf("presolve: %.2fs cells=%d deep=%d floors=%d proven=%d revoked=%d timeout=%v\n",
			report.Elapsed.Seconds(), report.Cells, report.Deep, report.Pinned,
			report.Proven, report.Revoked, report.TimedOut)
		// Two different counts, easy to confuse: report.Pinned counts every cell
		// that got a floor, while preFrozen only holds cells proven to be
		// completely immovable (lb == ub, no other demand can ever reach them).
		fmt.Printf("  lb_sat=%.9f (first-bit rank %d)  proven_pins=%d floors=%d\n",
			report.LBSat, eval.RankInt(report.LBSat), len(preFrozen), len(floors))
		if start := eval.RankInt(report.LBSat); start >= bestFirst {
			fmt.Printf("  first bit already at the structural floor: start rank %d == lb rank %d (optimal for layer 1)\n",
				bestFirst, start)
		}
		for i, p := range report.Pins {
			if i >= 5 {
				fmt.Printf("  ... %d more pins (see -presolve-pins)\n", len(report.Pins)-i)
				break
			}
			fmt.Printf("  pin t=%d arc=%d(%d->%d) load=%.4f sat=%.6f src=%s proven=%v forced_demands=%d\n",
				p.T, p.Arc, g.From[p.Arc], g.To[p.Arc], p.Load, p.Sat, p.Source, p.Proven, p.Demands)
		}
		if *presolvePins != "" {
			if err := presolve.WritePins(*presolvePins, report.Pins); err != nil {
				fatal(err)
			}
			fmt.Printf("  wrote %d pins -> %s\n", len(report.Pins), *presolvePins)
		}
	}

	// --- Schedule and freeze memory (design doc §14/§15/§21) ----------------
	scMode, err := sched.ParseMode(*schedule)
	if err != nil {
		fatal(err)
	}
	sc := sched.New(sched.Options{
		Mode: scMode, T: T, FocusN: *focusN, HotK: *hotK,
		GrowMult: *growMult, ScaleCap: *scaleCap,
		MaxW1: *candW1, MaxW2: *candW2, WidthStep: *widthStep,
	})
	// Three freeze tiers, in increasing order of how long they last:
	//   preFrozen   a physical arc the presolve proved immovable -- permanent,
	//               and carried through every reset below;
	//   softFrozen  an epoch_focus key retired on confidence -- cleared when
	//               the epoch ends (a key outside the focus is only skipped);
	//   skipEpoch   every other retired key -- also cleared when the epoch ends.
	// done is the union the hot ranking skips; the two epoch sets are exactly
	// what an epoch restart lifts.
	softFrozen := map[snap.Key]bool{}
	skipEpoch := map[snap.Key]bool{}
	done := map[snap.Key]bool{}
	for k := range preFrozen {
		done[k] = true
	}
	// seedMem is the per-seed memory: conf is the accumulated evidence that the
	// MIP cannot drop the first bit from this cell, notProven counts the rounds
	// that did not prove optimality, and scale is the retry multiplier the
	// round geometry is widened by (doc §15).
	type seedState struct {
		conf      float64
		notProven int
		scale     int
	}
	seedMem := map[snap.Key]*seedState{}
	memOf := func(k snap.Key) *seedState {
		st := seedMem[k]
		if st == nil {
			st = &seedState{scale: 1}
			seedMem[k] = st
		}
		return st
	}
	// dropMem voids every "this cell cannot be moved" conclusion: the hot
	// structure changed, so the empirical memory built against the old one says
	// nothing about the new one.  Presolve pins are structural proofs, not
	// empirical memory, and survive.  Both the first-bit accept and a local
	// search call that drops the first bit need exactly this.
	dropMem := func() {
		for k := range done {
			if !preFrozen[k] {
				delete(done, k)
			}
		}
		skipEpoch = map[snap.Key]bool{}
		softFrozen = map[snap.Key]bool{}
		seedMem = map[snap.Key]*seedState{}
		sc.Reschedule()
	}
	tested := map[snap.Key]bool{} // runtime TestKey is asked once per cell
	accepted, rejected := 0, 0
	epochRestarts := 0
	why := map[string]int{}
	var poolSum, candSum, solveCount int
	// Local-search bookkeeping: one call per proven accept, capped in aggregate
	// by -local-search-budget-frac of the wall clock.  The aggregate cap is what
	// keeps a run of many accepts from spending the budget the rounds need: a
	// per-call cap alone bounds one call, not the sum of them.
	var lsCalls, lsRounds, lsImproved int
	var lsSpent time.Duration
	lsBudget := time.Duration(0)
	if *wallSec > 0 && *localBudgetFrac > 0 {
		lsBudget = time.Duration(*localBudgetFrac * float64(*wallSec) * float64(time.Second))
	}
	// mipRtSum accumulates Gurobi's own Runtime attribute over every solved
	// round, so the summary can separate "time spent inside Gurobi" from "time
	// spent building the candidates Gurobi chooses between".
	var mipRtSum float64
	// Halo bookkeeping: how many rounds rolled the halo on, and how many pairs it
	// actually added across them (a triggered round that adds nothing still
	// counts, which is the honest denominator for "is the halo doing anything").
	var haloRounds, haloAdded, haloGain int

	// Live view: bind the HTTP server, print/open the URL, and publish one
	// snapshot per round (before the expensive pool build / MIP, so the cells
	// being worked on stay on screen while Gurobi runs) plus a final one.
	var mon *monitor.Server
	if *monitorOn {
		mon = monitor.New()
		url, err := mon.Listen(*monitorPort)
		if err != nil {
			fmt.Fprintf(os.Stderr, "live view disabled: %v\n", err)
		} else {
			fmt.Printf("live view: %s  (auto-open; TASR_MONITOR_NO_OPEN=1 disables)\n", url)
			monitor.OpenBrowser(url)
		}
		// Heartbeat: re-stamp the live snapshot ~1x/s so the page clock/status
		// move while a round is stuck inside pool build + Gurobi.  The real per-
		// round publishes (below) still carry the fresh heatmap on top of it.
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for range t.C {
				mon.LiveTick()
			}
		}()
	}
	tSolveStart := time.Now()
	startRank := bestFirst
	// confView is the live view's amber cap: the page draws min(1, value/limit)
	// over each frozen cell's bar, so a per-mille confidence with a limit of
	// 1000 shows the accumulated freeze evidence directly.  The old view showed
	// fail/failLimit, which the conf/not-proven split (doc §21) replaced.
	const confPermille = 1000
	confView := func() map[snap.Key]int {
		m := make(map[snap.Key]int, len(seedMem))
		for k, st := range seedMem {
			if st.conf > 0 {
				m[k] = int(st.conf * confPermille)
			}
		}
		return m
	}
	publish := func(status, msg string, round int, hots []snap.Key) {
		if mon == nil {
			return
		}
		mon.Publish(monitor.BuildState(inst, g, bestSnap, monitor.BuildOpts{
			Instance: inst.Name, Status: status, Message: msg,
			Round: round, MaxRounds: *rounds,
			ElapsedMS: time.Since(tSolveStart).Milliseconds(),
			Accepted:  accepted, Rejected: rejected,
			TotalCost: bestSnap.TotalCost(), BudgetOK: bestSnap.BudgetOK(),
			StartRank: startRank, FailLimit: confPermille,
			Hots: hots, Done: done, Fail: confView(),
			BarTop: *monitorTop, TopN: *monitorTopN,
		}))
	}
	publish("running", "", 0, nil)

	for r := 1; r <= *rounds; r++ {
		if !stopAt.IsZero() && time.Now().After(stopAt) {
			fmt.Printf("  wall-sec limit %d reached after %d rounds\n", *wallSec, r-1)
			break
		}
		sat := bestSnap.Saturations()
		ranked := bestSnap.RankKeys(done)
		// Live = the ranked cells that still carry load.  A zero-load cell has
		// nothing to relieve, and it leaves the schedule's view for the same
		// reason a retired one does, so an epoch whose members all drained to
		// zero still ends.
		live := ranked[:0]
		for _, k := range ranked {
			if sat[k.A*T+k.T] <= 0 {
				break
			}
			live = append(live, k)
		}
		if len(live) == 0 {
			break
		}
		// The epoch's direct exhaustion test: are the globally hottest
		// epochTopN cells all retired?  The ranking here is deliberately taken
		// *before* the retired set is subtracted -- a frozen cell must still
		// count where it stands, which is the whole point of asking.  A hot
		// round can only ever attack a live cell, so once these are gone the
		// first epochTopN layers of the vector are out of reach for the rest of
		// the epoch and the whole round budget is better spent at a wider
		// scale.  Physical pins are in done too: they can never move, so they
		// must not hold an epoch open.
		topRetired := false
		if *epochTopN > 0 && sc.Mode() == sched.PingPong {
			topRetired = true
			for i, k := range bestSnap.RankKeys(nil) {
				if i >= *epochTopN {
					break
				}
				if !done[k] {
					topRetired = false
					break
				}
			}
		}
		// The schedule picks this round's cells: the legacy Hot mode hands back
		// the globally hottest hotK, pingpong alternates hot rounds (the hottest
		// live epoch_focus member) with early rounds (a cursor sweeping the
		// slots).  Note the pool is *not* filtered by the focus set -- the focus
		// only decides which slot a hot round attacks, while the decomposition
		// inside it still takes the top arcs by saturation (doc §9).
		dec, ok := sc.Next(live, func(k snap.Key) int { return memOf(k).scale }, topRetired)
		if !ok {
			fmt.Printf("schedule: epoch exhausted at scale cap after %d rounds\n", r-1)
			break
		}
		if dec.Restart {
			// A new epoch: lift the epoch freezes and forget the epoch's
			// confidence memory.  Structural presolve pins are a proof, not
			// heuristic memory, so they survive.
			for k := range skipEpoch {
				if !preFrozen[k] {
					delete(done, k)
				}
				delete(skipEpoch, k)
			}
			for k := range softFrozen {
				if !preFrozen[k] {
					delete(done, k)
				}
				delete(softFrozen, k)
			}
			seedMem = map[snap.Key]*seedState{}
			epochRestarts++
		}
		hots := dec.Hots
		// A cell whose load sits on its presolve floor can never be relieved: no
		// routing goes below that load, so the round as a *max* round is wasted.
		// Drop those cells from the seed set (the MIP's z is floored at the same
		// bound anyway, and the peel still pushes the tail of the vector).  When
		// every hot cell is at its floor the filter is skipped: the round then
		// has nothing to gain on the layer, but the fallback keeps the loop's
		// bookkeeping unchanged.
		if *skipFloor && len(floors) > 0 {
			load := bestSnap.Load()
			kept := make([]snap.Key, 0, len(hots))
			for _, k := range hots {
				fl, ok := floors[k]
				if ok && fl > 0 && load[k.A*T+k.T] <= fl+1e-9*math.Max(1, fl) {
					continue
				}
				kept = append(kept, k)
			}
			if len(kept) > 0 {
				hots = kept
			}
		}
		publish("running", "", r, hots)
		if debug(r) {
			fmt.Printf("round %d: hot cells=%v sat=[%.4f..%.4f] done=%d\n",
				r, hots, sat[hots[len(hots)-1].A*T+hots[len(hots)-1].T],
				sat[hots[0].A*T+hots[0].T], len(done))
		}

		// freeze retires one seed for the rest of the epoch (doc §15).  It is
		// also the moment to re-ask whether the cell is structurally immovable
		// against the *current* snapshot: a cell that only becomes structural
		// after a few rounds is upgraded to a permanent pre_frozen pin rather
		// than a soft freeze, so it is never attacked again (doc §13).
		freeze := func(k snap.Key) {
			if skipEpoch[k] {
				return
			}
			skipEpoch[k] = true
			done[k] = true
			if sc.InFocus(k) {
				softFrozen[k] = true
			}
			if tested[k] || preFrozen[k] || pMode == presolve.Off {
				return
			}
			tested[k] = true
			if p, ok := judge.TestKey(bestSnap, bounds, k.T, k.A); ok {
				preFrozen[k] = true
				delete(softFrozen, k)
				fmt.Printf("  presolve: runtime pin t=%d arc=%d load=%.4f sat=%.6f (forced_demands=%d)\n",
					p.T, p.Arc, p.Load, p.Sat, p.Demands)
			}
		}

		// hotsHold reports whether this round's cell set contains k.  It is what
		// tells an early round that is asking the gate's question anyway (see
		// retire) from one that is sweeping a slot the top cell has nothing to
		// do with.
		hotsHold := func(k snap.Key) bool {
			for _, h := range hots {
				if h == k {
					return true
				}
			}
			return false
		}

		// confBase is the floor of one proven-optimal miss's confidence: a miss
		// costs at least this much no matter how narrow the round's search was,
		// so even a cover of 0 retires a seed in 1/confBase misses.  It is what
		// keeps the gate from reading a narrow round -- which proves very little
		// -- as if it proved as much as a wide one.
		const confBase = 0.1

		// retire books one failed round against every attacked cell.  Which
		// counter moves depends on what the MIP proved (doc §21): a not-proven
		// round (timeout or infeasible) is no evidence that the cell is
		// immovable, so it feeds not_proven and freezes after miss-fail-max; a
		// proven-optimal round that still could not drop the first bit feeds
		// the confidence by the round's cover (cover**2 + confBase) and freezes
		// the seed once conf >= 1.  Below that threshold the seed's retry scale
		// is bumped, so the next round on it searches wider before it can be
		// retired.
		//
		// An early round feeds the confidence gate only when it happened to
		// attack the hottest live cell.  It picks its cells off the cursor, not
		// off the heat, so as a rule the sub-problem it just solved is not the
		// one the gate is asking about: its optimality says nothing about
		// whether those cells can be moved out of the vector's top, and a wide
		// sweep would otherwise hand the tail a confidence the hot rounds never
		// earned.  When the cursor's slot does hold the hottest live cell the
		// round is asking exactly the gate's question, so its proof counts like
		// any hot round's.  Ungated rounds book an ordinary miss instead --
		// under their own reason, so the histogram keeps telling the two apart.
		// -conf-gate=false collapses both counters back into the legacy
		// every-rejection-counts rule.
		retire := func(proven bool, cover float64, early bool) {
			gated := !early || hotsHold(live[0])
			for _, k := range hots {
				st := memOf(k)
				if *confGate && proven && gated {
					why["conf"]++
					st.conf += cover*cover + confBase
					if st.conf >= 1 {
						freeze(k)
						continue
					}
					st.scale *= *growMult
					if st.scale > *scaleCap {
						st.scale = *scaleCap
					}
					continue
				}
				st.notProven++
				if early {
					why["early-miss"]++
				} else {
					why["not-proven"]++
				}
				if st.notProven >= *failLimit {
					freeze(k)
				}
			}
			rejected++
		}

		// The round's candidate budget is the schedule's scale applied to the
		// base widths (doc §15/§16: a bumped scale multiplies mip_budget, whose
		// closest Go analogue is the per-demand waypoint cap).
		roundCand := candOpts
		roundCand.MaxW1 = dec.MaxW1
		roundCand.MaxW2 = dec.MaxW2
		gen := &mip.Generator{
			Inst: inst, G: g, Snap: bestSnap,
			// Legacy path only; unused (and not merged with the candidate
			// budgets) when the targeted generator is on.
			MaxCandPerDemand: 12,
			Cand:             roundCand,
			CandIX:           cache.Index(),
			CandHops:         hopCache,
			// Pool build is the only per-round phase with no natural cost bound,
			// so it is where a round is allowed to give up.
			Deadline: stopAt,
			// Seed of the halo's per-demand admission roll (ExpandHalo fills in
			// AdmitProb and AdmitRound on its own clone of this generator).
			AdmitSeed: *haloSeed,
		}
		tPoolStart := time.Now()
		var pool *mip.Pool
		// Sticky anchor + reach (design doc §12/§14): a round makes decisions
		// at the seed slot and the chosen waypoint list is copied forward over
		// the following slots, so a cell on slot s can only be relieved by a
		// round anchored at some t <= s (the copy never runs backwards).
		// The anchor alternates between the two round kinds of the schedule:
		//   - hot anchor (the slot the schedule picked, or the globally hottest
		//     cell under -schedule hot): the run is short (a divergence at that
		//     slot) so the MIP can *spend* Hamming budget to shave a cell no
		//     free twin can reach;
		//   - early anchor (earliest attacked slot): the copy reaches the later
		//     slots of the set, reproducing the twin reach — a slot-1 hot cell
		//     is relieved budget-free by a seed-0 copy that writes both slots,
		//     the only affordable shape when the budget is ~0.
		// Either way the whole cell set inside the copy window is handed to the
		// pool so every reachable cell is ranked and relieved.
		// A hot (divergence) round spends Hamming budget, so it is only worth
		// running while the transition budgets still have headroom; with the
		// budget exhausted a divergence is unaffordable and the round can only
		// reject.  Remaining = sum over transitions of Budget - incumbent cost.
		// The 8 threshold is above the ~2-4 units the cheapest single-waypoint
		// divergence costs.
		seedT := dec.Slot
		hot := !dec.Early
		if sc.Mode() == sched.Hot {
			// Legacy rule, kept bit-for-bit: -schedule hot must reproduce the
			// pre-pingpong solver exactly.  (Only the sticky model reads the
			// anchor at all, so the twin model is unaffected either way.)
			rem := 0
			for tt := 1; tt < T; tt++ {
				if tt < len(inst.Scenario.Budget) {
					rem += inst.Scenario.Budget[tt] - bestSnap.CostAt(tt)
				}
			}
			hot = r%2 == 0 && rem >= 8
			seedT = hots[0].T
		} else if hot {
			rem := 0
			for tt := 1; tt < T; tt++ {
				if tt < len(inst.Scenario.Budget) {
					rem += inst.Scenario.Budget[tt] - bestSnap.CostAt(tt)
				}
			}
			if rem < 8 {
				hot = false
			}
		}
		if !hot {
			// early/free round: anchor at the earliest attacked slot so the
			// copy reaches every decomposed cell (free twin relief).
			seedT = hots[0].T
			for _, k := range hots {
				if k.T < seedT {
					seedT = k.T
				}
			}
		}
		reach := seedT + *stickySpan
		if reach > T-1 {
			reach = T - 1
		}
		// Keep only the cells the copy can actually reach; bookkeeping (retire /
		// done) must match the cells the round really attacks.
		kept := hots[:0]
		for _, k := range hots {
			if k.T >= seedT && k.T <= reach {
				kept = append(kept, k)
			}
		}
		hots = kept
		if len(hots) == 0 {
			why["no-seed-hot"]++
			retire(false, 0, dec.Early)
			continue
		}
		hc := make([]mip.HotCell, len(hots))
		for i, k := range hots {
			hc[i] = mip.HotCell{Slot: k.T, Arc: k.A}
		}
		pool, err = gen.BuildSticky(seedT, hc, *stickySpan)
		if err != nil {
			fatal(err)
		}
		if pool == nil {
			// sticky found no demand active on the seed slot: nothing to build.
			pool = &mip.Pool{}
		}
		// Halo: a deliberate gamble, not a stable gain, so it fires on a seeded
		// per-round roll rather than every round.  When it fires it looks at the
		// arcs this round's own candidates have just decided to load and pulls
		// their demands into the same MIP, which is the only way the solver can see
		// that relieving one hot cell is about to crowd another (pool.go
		// ExpandHalo has the full argument).  The halo run repeats the base
		// round's own seed and span, so the two pools describe the same horizon.
		//
		// -halo-admit optionally samples the demands inside a triggered round as
		// well.  It defaults to 1 (admit everything) because sampling was measured
		// weaker: sprinkling a 30% demand subset into every round added columns
		// without ever giving the peel a decisive alternative, so it mostly
		// reproduced the no-halo answer while paying halo cost on every round.
		if *haloOn && len(pool.Pairs) > 0 && haloRoll(*haloSeed, r, *haloProb) {
			before := len(pool.Pairs)
			expanded, herr := gen.ExpandHalo(pool, seedT, *stickySpan, r, *haloMult, *haloAdmit)
			if herr != nil {
				fatal(herr)
			}
			if expanded != nil {
				pool = expanded
			}
			haloRounds++
			haloAdded += len(pool.Pairs) - before
			haloGain = len(pool.Pairs) - before
		}
		tPool := time.Since(tPoolStart).Seconds()
		if timing() && debug(r) {
			fmt.Printf("    round %d pool %d pairs in %.2fs (halo +%d)\n",
				r, len(pool.Pairs), tPool, haloGain)
		}
		if len(pool.Pairs) == 0 {
			why["no-pool"]++
			retire(false, 0, dec.Early)
			continue
		}
		// How broad this round's candidate columns are, as a fraction of the
		// network's arcs (doc §15).  It is the weight of a proven-optimal miss:
		// a wide search that still failed is far stronger evidence than a narrow
		// one, so it is measured here, once, rather than per reject path.  Only
		// the confidence gate reads it, so it is not paid for without it -- nor
		// for a round whose miss the gate will not read at all.
		cover := 0.0
		if *confGate && (!dec.Early || hotsHold(live[0])) {
			cover = pool.Cover()
		}

		prob, err := mip.BuildMode(pool, bestSnap, mip.BuildOptions{
			// first_half rounds the movable budget of the very first round down
			// to 50% so one early decision cannot spend the whole budget.
			HalfFirst: *budgetMode == "first_half" && r == 1,
			Pinned:    preFrozen,
			Floors:    floors,
			SatFloor:  satFloor,
		})
		if err != nil {
			fatal(err)
		}
		if v := prob.PinnedViolations(); len(v) > 0 {
			// A candidate can move a cell the presolve called immovable: the
			// structural proof is wrong for it (log it -- it is a bug signal,
			// and the real lex gate still protects the result).
			why["pin-violation"]++
			fmt.Printf("  WARN round %d: %d presolve pin(s) movable by this pool, e.g. t=%d arc=%d delta=%.6g\n",
				r, len(v), v[0].Key.T, v[0].Key.A, v[0].Delta)
		}
		poolSum += len(pool.Pairs)
		for _, pr := range pool.Pairs {
			candSum += len(pr.Cand)
		}
		solveCount++
		limit := time.Duration(*mipSec * float64(time.Second))
		// When a wall deadline is set, cap the per-layer Gurobi limit at what is
		// left over the remaining peel layers plus one, so a round whose layers
		// all hit their cap cannot consume the whole remaining budget and the
		// loop still gets to run again.  This holds even with -mip-sec 0 (solve
		// to completion): the wall clock outranks the "no cap" request.
		if !stopAt.IsZero() {
			rem := time.Until(stopAt) / time.Duration(*peel+1)
			if limit <= 0 || rem < limit {
				limit = rem
			}
			if limit <= 0 {
				fmt.Printf("  wall-sec limit %d reached after %d rounds\n", *wallSec, r-1)
				break
			}
		}
		// Dual probe (observation only): solve the same pool as a continuous LP
		// and dump the tracked cells' prices.  It reads no solver state and
		// writes none -- the decisions below are untouched -- but it does spend
		// real seconds, so it is off by default and a run with it on is not
		// comparable to one without.
		if *dualProbe >= r {
			// The peel, plus one control.  Layer 0 of the peel carries the
			// presolve floor and its z therefore sits on its own lower bound, so
			// no cell row binds and the dual comes back identically zero -- true
			// on every instance measured, and not a defect.  The control re-solves
			// layer 0 with zlb=0, where z is set by whichever cell actually binds;
			// comparing the two is what separates "the prices are zero" from "the
			// floor swallowed the prices", which look identical in one run.
			if rep, derr := prob.DualProbe(0, limit); derr != nil {
				fmt.Printf("  dual round %d: L0free probe failed: %v\n", r, derr)
			} else {
				printDualProbe(r, "L0free", rep, 5, satFloor)
			}
			if probes, derr := prob.DualProbePeel(*dualPeel, limit); derr != nil {
				fmt.Printf("  dual round %d: peel probe failed: %v\n", r, derr)
			} else {
				printDualPeel(r, probes, 5, satFloor)
			}
		}
		tMipStart := time.Now()
		res, err := prob.Solve(mip.SolveOptions{MaxPeel: *peel, TimeLimit: limit})
		if err != nil {
			fatal(err)
		}
		mipRtSum += res.Runtime
		if timing() && debug(r) {
			// wall = model build + Gurobi; grb = Gurobi's own Runtime.  The gap
			// is the Go-side build, and the round total minus this line is the
			// candidate pool -- the number that decides where to optimise.
			fmt.Printf("    round %d mip %.2fs (grb %.3fs) status=%d\n",
				r, time.Since(tMipStart).Seconds(), res.Runtime, res.Status)
		}
		if !res.HasValue || len(res.Choices) == 0 {
			// infeasible / not-proven / no candidate selection
			why["no-value"]++
			if debug(0) {
				fmt.Printf("    reject: mip status=%d no solution\n", res.Status)
			}
			retire(res.Proven(), cover, dec.Early)
			continue
		}

		// Materialise the chosen atomic moves on a trial solution.
		cur := bestSnap.Solution()
		trial := cur.Copy()
		changed := 0
		for _, c := range res.Choices {
			for _, op := range c.Apply {
				if !equalWps(cur.Waypoints[c.D][op.Slot], op.Wps) {
					trial.Set(c.D, op.Slot, op.Wps)
					changed++
				}
			}
		}
		if changed == 0 {
			why["no-change"]++
			retire(res.Proven(), cover, dec.Early)
			continue
		}

		trialSnap, err := snap.New(inst, g, cache, trial)
		if err != nil {
			fatal(err)
		}
		trialDesc := eval.SortedDesc(trialSnap.Saturations())
		if !trialSnap.BudgetOK() {
			// MIP rows should have enforced budgets; guard anyway.
			why["budget"]++
			retire(res.Proven(), cover, dec.Early)
			continue
		}
		if eval.LexCompare(trialDesc, bestDesc) >= 0 {
			why["lex"]++
			if debug(0) {
				fmt.Printf("    reject: changed=%d but not lex better (mip obj=%.6f)\n", changed, res.ObjVal)
			}
			retire(res.Proven(), cover, dec.Early)
			continue
		}

		// Accept.  Any move proves the involved cells are movable, so their
		// failure memory is void; a first-bit improvement voids every prior
		// "this cell cannot be moved" conclusion.
		oldFirst := bestFirst
		bestSnap = trialSnap
		bestDesc = trialDesc
		bestFirst = eval.RankInt(bestDesc[0])
		publishBest(bestSnap.Solution()) // keep the emergency copy in step with the incumbent
		accepted++
		for _, k := range hots {
			delete(done, k)
			delete(skipEpoch, k)
			delete(softFrozen, k)
			delete(seedMem, k)
		}
		if bestFirst < oldFirst {
			// The hot structure changed: every freeze and every confidence
			// memory is void, and the epoch restarts from a fresh focus.
			dropMem()
		}
		fmt.Printf("  round %d ACCEPT: first-bit rank %d -> %d  total_cost=%d changed=%d hots=%d scale=%d%s\n",
			r, oldFirst, bestFirst, bestSnap.TotalCost(), changed, len(hots), dec.Scale,
			map[bool]string{true: " early", false: ""}[dec.Early])
		if debug(r) {
			fmt.Printf("    top raw=%.9f  second raw=%.9f\n", bestDesc[0], bestDesc[1])
		}

		// --- Post-accept local search (internal/local) ---------------------
		// It hangs off the accept branch, not the round loop, for two reasons:
		// an accept is the only state that is certainly legal (budget-OK and lex
		// accepted), so the pass can never be handed something it would have to
		// repair; and a rejected or unproven round is rolled back, so there is
		// nothing to improve on.  res.Proven() narrows it further to the rounds
		// whose pool optimum the MIP actually established.
		//
		// The pass mutates bestSnap in place and every move it accepts is itself
		// lex-improving and budget-feasible, so the incumbent invariants hold
		// afterwards; only the cached vector and the emergency copy need the
		// refresh below.
		if ls != nil && res.Proven() && (lsBudget <= 0 || lsSpent < lsBudget) {
			before := bestFirst
			lr := ls.Run(bestSnap)
			lsCalls++
			lsSpent += lr.Elapsed
			lsRounds += lr.Rounds
			lsImproved += lr.Improved
			if lr.Improved > 0 {
				bestDesc = eval.SortedDesc(bestSnap.Saturations())
				bestFirst = eval.RankInt(bestDesc[0])
				publishBest(bestSnap.Solution()) // in step with the incumbent
				if bestFirst < before {
					// The pass dropped the first bit, which is the same event as a
					// first-bit accept and voids the same conclusions.
					dropMem()
				}
				fmt.Printf("  round %d local: %d move(s) in %d round(s) %.3fs  first-bit rank %d -> %d  total_cost=%d\n",
					r, lr.Improved, lr.Rounds, lr.Elapsed.Seconds(), before, bestFirst, bestSnap.TotalCost())
			} else if timing() {
				fmt.Printf("    round %d local: no move (%d demand(s), %d probe(s), %d ban(s)) in %.3fs\n",
					r, lr.Demands, lr.Probes, lr.Bans, lr.Elapsed.Seconds())
			}
		}
	}
	publish("done", "solver loop finished", 0, nil)

	// elapsed is time since process start, i.e. against the same origin as
	// -wall-sec: it is what tells an operator whether the budget was actually
	// respected, including the phases (load, presolve, write-out) that are not
	// part of the round loop.
	fmt.Printf("done: accepted=%d rejected=%d  final first-bit raw=%.9f rank=%d  total_cost=%d budget_ok=%v elapsed=%.1fs\n",
		accepted, rejected, bestDesc[0], bestFirst, bestSnap.TotalCost(), bestSnap.BudgetOK(),
		time.Since(t0).Seconds())
	keys := make([]string, 0, len(why))
	for k := range why {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		fmt.Printf("  reject reason %-10s %d\n", k, why[k])
	}
	if solveCount > 0 {
		// The Gurobi share is against total elapsed (same origin as -wall-sec),
		// so it is comparable across runs of different budgets.
		fmt.Printf("  solves=%d avg pool pairs=%.1f avg cands=%.1f gurobi=%.2fs (%.1f%% of elapsed)\n",
			solveCount, float64(poolSum)/float64(solveCount), float64(candSum)/float64(solveCount),
			mipRtSum, 100*mipRtSum/time.Since(t0).Seconds())
	}
	if *haloOn {
		fmt.Printf("  halo: fired on %d/%d rounds, +%d pairs total (mult=%d round prob=%.2f admit=%.2f seed=%d)\n",
			haloRounds, solveCount, haloAdded, *haloMult, *haloProb, *haloAdmit, *haloSeed)
	}
	// Candidate layer reporting.  The hop counters are what distinguishes the
	// cache arms of the benchmark: same queries, different "computed" count.
	fmt.Printf("  cand mode=%s pool_cap=%d max_w1=%d max_w2=%d global_k=%d hop_cache=%v\n",
		candOpts.Mode, candOpts.PoolCap, candOpts.MaxW1, candOpts.MaxW2, candOpts.GlobalK, *candHopCache)
	hHits, hMiss, hComp := hopCache.Stats()
	fmt.Printf("  hops queries=%d hits=%d misses=%d computed=%d sets=%d\n",
		hHits+hMiss, hHits, hMiss, hComp, hopCache.Sets())
	// Schedule reporting: the freeze tiers and the confidence memory are the
	// evidence a pingpong A/B is read against, so they are printed even when
	// every counter is zero.
	fmt.Printf("  schedule=%s focus_n=%d hot_k=%d grow_mult=%d scale_cap=%d conf_gate=%v scale=%d epoch_restarts=%d\n",
		scMode, *focusN, *hotK, *growMult, *scaleCap, *confGate, sc.Scale(), epochRestarts)
	// Local-search reporting: the elapsed share is the number that decides
	// whether the pass is paying for itself, since it comes out of the same wall
	// clock the rounds run in.
	if ls != nil {
		pct := 0.0
		if el := time.Since(t0).Seconds(); el > 0 {
			pct = 100 * lsSpent.Seconds() / el
		}
		fmt.Printf("  local: calls=%d rounds=%d moves=%d spent=%.2fs (%.1f%% of elapsed) top_cells=%d sat_min=%.4f probes=%d call_cap=%.1fs budget=%.1fs\n",
			lsCalls, lsRounds, lsImproved, lsSpent.Seconds(), pct,
			*localTopCells, *localSatMin, *localProbes, *localCallSec, lsBudget.Seconds())
	}
	fmt.Printf("  freeze: pre=%d soft=%d skip=%d conf_seeds=%d\n",
		len(preFrozen), len(softFrozen), len(skipEpoch), len(seedMem))
	// The atom LRU's numbers are what say whether -atom-size is set sensibly:
	// a high eviction count against a low hit rate is thrash, and thrash costs
	// a splitAtom (and often a fresh Dijkstra behind it) per miss.
	fmt.Printf("  atoms cap=%d hits=%d misses=%d evicted=%d\n",
		atomCap, cache.Hits, cache.Misses, cache.Evicted)

	if *heapProfile != "" {
		// A live heap of this size at this point in the run is the number the
		// memory diagnosis reads: it is taken after the last round, so the
		// per-round garbage is finalised and what remains is what the loop
		// retains.
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Printf("  heap live=%d MB (heap_inuse=%d MB sys=%d MB)\n",
			ms.HeapAlloc>>20, ms.HeapInuse>>20, ms.Sys>>20)
		if f, err := os.Create(*heapProfile); err == nil {
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(os.Stderr, "heap profile write %s: %v\n", *heapProfile, err)
			}
			f.Close()
			fmt.Printf("  heap profile -> %s\n", *heapProfile)
		} else {
			fmt.Fprintf(os.Stderr, "heap profile create %s: %v\n", *heapProfile, err)
		}
	}

	fin := bestSnap.Solution()
	// The first bit can never go below the structural floor, so a solution whose
	// rank equals the floor's rank has a *proven optimal* layer 1; the remaining
	// gap (if any) is in the tail.  Printed as a certificate, not a guess.
	if satFloor > 0 {
		if fl := eval.RankInt(satFloor); fl >= bestFirst {
			fmt.Printf("  first bit PROVEN optimal: rank %d == structural floor %d (raw %.9f)\n",
				bestFirst, fl, satFloor)
		} else {
			fmt.Printf("  first-bit floor rank %d vs achieved %d (raw floor %.9f)\n",
				fl, bestFirst, satFloor)
		}
	}
	fmt.Printf("RESULT %s accepted=%d firstbit=%d total_cost=%d budget_ok=%v\n",
		inst.Name, accepted, bestFirst, bestSnap.TotalCost(), bestSnap.BudgetOK())
	if *sprint != "" {
		compareSprint(*sprint, bestDesc, inst)
	}
	if *out != "" {
		// A false here means the watchdog beat us to the file (it is about to
		// exit 0 with the same incumbent written) or the write itself failed.
		// Only the latter is an error; the former is the fallback working.
		if !writeOut(*out, fin, inst) {
			if writeFailed() {
				fatal(fmt.Errorf("write %s: see the error above", *out))
			}
			fmt.Printf("solution already written -> %s\n", *out)
		}
	}
	if mon != nil {
		linger := time.Duration(*monitorLinger) * time.Second
		if *wallSec > 0 {
			// The live view is a debugging aid; it may not push the process past
			// the wall clock.  With a budget set, its share is whatever is left
			// after the search stopped.
			if left := time.Until(t0.Add(time.Duration(*wallSec) * time.Second)); left < linger {
				linger = left
			}
		}
		if linger > 0 {
			fmt.Printf("solve finished; live view stays up for %ds (Ctrl-C to exit now).\n", int(linger.Seconds()))
			time.Sleep(linger)
		}
		mon.Close()
	}
}

// haloRoll decides whether round r runs its halo.
//
// It is a hash of (seed, round) rather than a draw from a seeded stream: round
// r's decision must not depend on how many rounds ran before it.  Under a wall
// clock that count varies run to run, so a stream would make the 30% trigger a
// different set of rounds every time and the feature untestable.  Hashing the
// round index pins the decision to the round, which makes an A/B between two
// seeds (or against -halo=false) compare the same rounds.
func haloRoll(seed int64, r int, prob float64) bool {
	if prob >= 1 {
		return true
	}
	if prob <= 0 {
		return false
	}
	x := uint64(seed)*0x9E3779B97F4A7C15 ^ uint64(r)*0xBF58476D1CE4E5B9
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return float64(x>>11)/float64(uint64(1)<<53) < prob
}

// debug gates verbose per-round output behind an env var (kept small for runs).
func debug(r int) bool {
	v, err := strconv.Atoi(os.Getenv("TASR_SOLVE_VERBOSE"))
	return err == nil && v > 0 && r%max(1, v) == 0
}

// timing turns on per-round pool/MIP elapsed prints (needs verbose too).
func timing() bool {
	return os.Getenv("TASR_TIMING") == "1"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// emptySolutionFor is the all-shortest-path solution: every demand with an
// empty waypoint list.  It is the emergency fallback the watchdog writes, and
// it lives at package scope because inside main the -model flag shadows the
// model package name.
func emptySolutionFor(inst *model.Instance) *model.Solution {
	return model.EmptySolution(inst.NDemands(), inst.NSlots)
}

// bestMu guards bestSol, the newest solution the emergency writer may use.  The
// values exchanged are always deep copies (Snap.Solution copies, and
// emptySolutionFor allocates), so the watchdog never reads a solution the
// search is still mutating -- only the pointer handoff is synchronised.
var (
	bestMu  sync.Mutex
	bestSol *model.Solution
)

func publishBest(s *model.Solution) {
	bestMu.Lock()
	bestSol = s
	bestMu.Unlock()
}

func emergencySolution() *model.Solution {
	bestMu.Lock()
	defer bestMu.Unlock()
	return bestSol
}

// writeOut writes the solution file at most once per process, reporting whether
// this call did the write (false = someone else already did, or it failed and
// they reported it).  Both the normal shutdown and the deadline watchdog go
// through it, so the two can never interleave on the same path: whichever gets
// there first wins and the other is a no-op.  The normal path normally wins by
// seconds, but a loaded machine could otherwise overlap them and tear the file.
// writeOut is called from the main goroutine and from the watchdog goroutine,
// so the claim flag and the recorded error are both mutex-guarded: the second
// caller reads the first caller's outcome, and an unsynchronised read there
// would be a data race.
var writeMu struct {
	sync.Mutex
	wrote bool
	err   error
}

func writeOut(path string, s *model.Solution, inst *model.Instance) bool {
	writeMu.Lock()
	defer writeMu.Unlock()
	if writeMu.wrote || writeMu.err != nil {
		return false
	}
	if err := io.WriteSolution(path, s, inst); err != nil {
		writeMu.err = err
		fmt.Fprintln(os.Stderr, "solve: write failed:", err)
		return false
	}
	writeMu.wrote = true
	fmt.Printf("wrote solution -> %s\n", path)
	return true
}

// writeFailed reports a failed write, distinguishing it from "the other writer
// got there first" (which is the normal outcome of the watchdog race).
func writeFailed() bool {
	writeMu.Lock()
	defer writeMu.Unlock()
	return writeMu.err != nil
}

func equalWps(a, b []int) bool {
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

// compareSprint prints the quantified gap of the final vector against the
// sprint reference row (tie-layer count + first-gap layer and values).
func compareSprint(path string, desc []float64, inst *model.Instance) {
	f, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	recs, err := rd.ReadAll()
	if err != nil {
		fatal(err)
	}
	var ref []float64
	for _, rec := range recs[1:] {
		if len(rec) < 2 || rec[0] != inst.Name {
			continue
		}
		for _, tok := range rec[2:] {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			v, err := strconv.ParseFloat(tok, 64)
			if err != nil {
				fatal(fmt.Errorf("sprint row %s: %q", inst.Name, tok))
			}
			ref = append(ref, v)
		}
		break
	}
	if ref == nil {
		fatal(fmt.Errorf("no sprint row for %s", inst.Name))
	}
	ours := eval.RankMatrix(desc)
	n := len(ours)
	if len(ref) < n {
		n = len(ref)
	}
	tied := 0
	for tied < n && int64(ref[tied]*1e6+0.5) == ours[tied] {
		tied++
	}
	if tied == n && n == len(ref) && n == len(ours) {
		fmt.Printf("sprint compare: FULL TIE across %d layers\n", n)
		return
	}
	layer := tied + 1
	var rq, oq int64 = -1, -1
	if layer <= len(ref) {
		rq = int64(ref[layer-1]*1e6 + 0.5)
	}
	if layer <= len(ours) {
		oq = ours[layer-1]
	}
	fmt.Printf("sprint compare: tie %d/%d layers; first gap layer %d (%d vs %d)\n",
		tied, len(ref), layer, oq, rq)
	// Profile the first few divergent layers for context.
	shown := 0
	for i := tied; i < len(ours) && i < len(ref) && shown < 5; i++ {
		rr := int64(ref[i]*1e6 + 0.5)
		if rr == ours[i] {
			continue
		}
		fmt.Printf("    layer %d: ours %d vs sprint %d\n", i+1, ours[i], rr)
		shown++
	}
}

// printLayerSummary is one layer's verdict on a single line: the LP optimum
// measured against the presolve's sound floor, and whether the dual vector is
// alive or a run of exact zeros.  live==0 is the expected outcome at layer 0 on
// every instance measured; live>0 at a later layer is the signal the peel exists
// to find.
func printLayerSummary(rep *mip.DualReport, satFloor float64) {
	n := len(rep.Cells)
	k := 10
	if k > n {
		k = n
	}
	off, considered := rep.Disagreement(k)
	fmt.Printf("z=%.6f (sound floor %.6f, +%.6f) priced=%d live=%d pairLive=%d/%d pairMax=%.3g sumLcap=%.6f disagree=%d/%d maxPi=%.6g\n",
		rep.Zlp, satFloor, rep.Zlp-satFloor, n, rep.CellLive,
		rep.PairNonZero, len(rep.PairPi), rep.PairAbsMax, rep.SumAbsPiCap, off, considered, rep.CellAbsMax)
}

// printTopPrices lists the cells by decreasing |price|.  It sorts by magnitude
// rather than by signed price on purpose: a live price can be negative (a cell
// that must not gain load prices below zero), and a signed sort would bury it
// under a hundred exact zeros.  note=="at-z" marks a cell sitting on this
// layer's own optimum -- the cells the layer is about to pin, and by
// complementary slackness the only ones whose price can be non-zero at all.
func printTopPrices(rep *mip.DualReport, top int, lockedNow map[snap.Key]bool) {
	cs := append([]mip.CellPrice(nil), rep.Cells...)
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := math.Abs(cs[i].Pi), math.Abs(cs[j].Pi)
		if a != b {
			return a > b
		}
		if cs[i].Key.T != cs[j].Key.T {
			return cs[i].Key.T < cs[j].Key.T
		}
		return cs[i].Key.A < cs[j].Key.A
	})
	if top > len(cs) {
		top = len(cs)
	}
	fmt.Printf("      %3s %5s %6s %12s %12s %12s %11s %s\n",
		"#", "slot", "arc", "sat", "lsat", "cap", "Pi", "note")
	for i := 0; i < top; i++ {
		c := cs[i]
		// Only this layer's own locks are "at z".  A cell pinned by an earlier
		// layer still carries its old load in the LP, but its row no longer
		// mentions z at all, so its saturation has nothing to do with this
		// layer's optimum.
		note := ""
		if lockedNow[c.Key] {
			note = "at-z"
		}
		fmt.Printf("      %3d %5d %6d %12.6f %12.6f %12.2f %11.4f %s\n",
			i+1, c.Key.T, c.Key.A, c.Sat, c.LoadSat, c.Cap, c.Pi, note)
	}
}

// printDualProbe dumps one standalone LP's prices.  It is used for the
// layer-0-with-no-floor control, whose contrast against the peel's own layer 0
// is what separates "the prices are zero" from "the floor swallowed the prices".
func printDualProbe(r int, label string, rep *mip.DualReport, top int, satFloor float64) {
	if !rep.Proven {
		fmt.Printf("  dual round %d [%s]: LP not proven (status=%d), no prices\n", r, label, rep.Status)
		return
	}
	fmt.Printf("  dual round %d [%s] ", r, label)
	printLayerSummary(rep, satFloor)
	printTopPrices(rep, top, nil)
}

// printDualPeel prints one round's LP peel.  Layer 0 restates the standalone
// probe (z on the presolve floor, dual dead); the layers after it are the point:
// their predecessors are pinned as hard rows and their z is free, so whichever
// cell decides that layer's max is the cell whose price is live -- layer-k
// information the saturation ranking cannot reach.
func printDualPeel(r int, probes []mip.LayerProbe, top int, satFloor float64) {
	fmt.Printf("  dual round %d peel: %d layer(s), sound floor %.6f\n", r, len(probes), satFloor)
	for _, lp := range probes {
		rep := lp.Report
		if !rep.Proven {
			fmt.Printf("    L%d zlb=%.6f: LP not proven (status=%d), peel stops\n", lp.Layer, lp.Zlb, rep.Status)
			return
		}
		fmt.Printf("    L%d zlb=%.6f ", lp.Layer, lp.Zlb)
		printLayerSummary(rep, satFloor)
		if len(lp.Locked) > 0 {
			fmt.Printf("      locked:")
			for i, lc := range lp.Locked {
				if i == 4 {
					fmt.Printf(" +%d more", len(lp.Locked)-4)
					break
				}
				fmt.Printf(" (t=%d arc=%d sat=%.6f)", lc.Key.T, lc.Key.A, lc.Sat)
			}
			fmt.Println()
		}
		if lp.Repeated {
			fmt.Printf("      nothing locked -> the next layer would re-solve this LP, peel stops\n")
		}
		lockedNow := make(map[snap.Key]bool, len(lp.Locked))
		for _, lc := range lp.Locked {
			lockedNow[lc.Key] = true
		}
		printTopPrices(rep, top, lockedNow)
	}
}

// samplePi formats the first n entries of a price vector for eyeballing; the
// values are only ever printed, never compared.
func samplePi(v []float64, n int) string {
	if n > len(v) {
		n = len(v)
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("%.4f", v[i]))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
