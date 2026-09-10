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
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
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
	"tasr/internal/mip"
	"tasr/internal/model"
	"tasr/internal/monitor"
	"tasr/internal/presolve"
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
	failLimit := flag.Int("fail-limit", 2, "failed attempts before a seed is marked done")
	hotK := flag.Int("hot-k", 6, "number of hot arcs of the focus slot decomposed together")
	model := flag.String("model", "twin", "pool model: twin (explicit single/twin families) or sticky (seed decision extends over the incumbent plateau)")
	stickySpan := flag.Int("sticky-span", 1, "sticky: copy the seed waypoint at most this many slots forward")
	budgetMode := flag.String("budget-mode", "full", "Hamming budget mode: full (spend all remaining each round) or first_half (round 1 spends half of the movable remaining budget)")
	wallSec := flag.Int("wall-sec", 0, "total wall-clock budget in seconds, counted from process start (0 = unlimited)")
	mipSec := flag.Float64("mip-sec", 20, "per peel-layer Gurobi time cap in seconds (0 = solve to completion)")
	monitorOn := flag.Bool("monitor", false, "serve a live progress page at http://127.0.0.1:<port> and open the browser")
	monitorPort := flag.Int("monitor-port", 8765, "live view port (0 = pick a free port; busy port falls back to free)")
	monitorTop := flag.Int("monitor-top", 10, "bars per time slot on the live view page")
	monitorTopN := flag.Int("monitor-topn", 10, "top-N hottest cells listed at the top of the live view page")
	monitorLinger := flag.Int("monitor-linger", 45, "seconds to keep the live view alive after solving stops (0 = exit immediately)")
	presolveMode := flag.String("presolve", "unmovable", "presolve mode: unmovable (structural class 1+2 scan + budgeted exact pass) or off")
	presolveSec := flag.Float64("presolve-time", 15, "global presolve time budget in seconds")
	presolveDeep := flag.Int("presolve-deep", 100, "exact (class 3) presolve pass: max cells examined, 0 = as many as the budget allows")
	presolvePins := flag.String("presolve-pins", "", "dump the presolve pin table as TSV to this path")
	skipFloor := flag.Bool("skip-floor", true, "do not seed a round on a cell whose load sits on its presolve floor (it can never be relieved)")
	candMode := flag.String("cand-mode", "mix", "candidate generator: mix|hot_center|od_scan|bottleneck (targeted, default) or off (legacy brute-force enumeration)")
	candPoolCap := flag.Int("cand-pool-cap", 24, "candidate: nodes kept per strategy pool")
	candW1 := flag.Int("cand-max-w1", 24, "candidate: max 1-waypoint candidates per demand per family (independent of -cand-max-w2)")
	candW2 := flag.Int("cand-max-w2", 32, "candidate: max 2-waypoint candidates per demand per family (independent of -cand-max-w1)")
	candBans := flag.Int("cand-max-bans", 3, "candidate bottleneck: max hot arcs banned per demand per slot")
	candSeed := flag.Int64("cand-seed", 0, "candidate: deterministic seed for the off-hot random top-up")
	candGlobalK := flag.Int("cand-global-k", 0, "candidate: global-relief safety net -- append up to K best-relief singletons found by scanning every node, beyond the strategy pools (0 = off)")
	candHopCache := flag.Bool("cand-hop-cache", true, "candidate: retain hop-count BFS results per banned-arc set (false = recompute every query, the uncached control)")
	indexSize := flag.Int("index-size", 0, "graph index LRU capacity (0 = size to the instance: 4*n*T)")
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
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
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

	done := map[snap.Key]bool{}
	fail := map[snap.Key]int{}
	for k := range preFrozen {
		done[k] = true
	}
	tested := map[snap.Key]bool{} // runtime TestKey is asked once per cell
	accepted, rejected := 0, 0
	why := map[string]int{}
	var poolSum, candSum, solveCount int

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
			StartRank: startRank, FailLimit: *failLimit,
			Hots: hots, Done: done, Fail: fail,
			BarTop: *monitorTop, TopN: *monitorTopN,
		}))
	}
	publish("running", "", 0, nil)

	for r := 1; r <= *rounds; r++ {
		if !stopAt.IsZero() && time.Now().After(stopAt) {
			fmt.Printf("  wall-sec limit %d reached after %d rounds\n", *wallSec, r-1)
			break
		}
		keys := bestSnap.RankKeys(done)
		if len(keys) == 0 {
			break
		}
		sat := bestSnap.Saturations()
		if sat[keys[0].A*T+keys[0].T] <= 0 {
			// All remaining non-done keys carry no load; nothing left to relieve.
			break
		}
		// Decompose the global hottest cells (both slots when T=2), so the pool
		// can relieve a slot-1 hot cell with a budget-free twin reroute as well
		// as a single-slot divergence.
		var hots []snap.Key
		for _, k := range keys {
			if sat[k.A*T+k.T] <= 0 {
				break
			}
			hots = append(hots, k)
			if len(hots) >= *hotK {
				break
			}
		}
		if len(hots) == 0 {
			break
		}
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

		// Mark one rejected round against every hot cell.  Reaching failLimit
		// retires the seed -- and is exactly the moment to re-ask whether the
		// cell is structurally immovable against the *current* snapshot: a cell
		// that only becomes structural after a few rounds is then retired for
		// good instead of consuming rounds to the end.
		markFail := func() {
			for _, k := range hots {
				fail[k]++
				if fail[k] < *failLimit {
					continue
				}
				done[k] = true
				if tested[k] || preFrozen[k] || pMode == presolve.Off {
					continue
				}
				tested[k] = true
				if p, ok := judge.TestKey(bestSnap, bounds, k.T, k.A); ok {
					preFrozen[k] = true
					delete(fail, k)
					fmt.Printf("  presolve: runtime pin t=%d arc=%d load=%.4f sat=%.6f (forced_demands=%d)\n",
						p.T, p.Arc, p.Load, p.Sat, p.Demands)
				}
			}
			rejected++
		}

		gen := &mip.Generator{
			Inst: inst, G: g, Snap: bestSnap,
			// Legacy path only; unused (and not merged with the candidate
			// budgets) when the targeted generator is on.
			MaxCandPerDemand: 12,
			Cand:             candOpts,
			CandIX:           cache.Index(),
			CandHops:         hopCache,
			// Pool build is the only per-round phase with no natural cost bound,
			// so it is where a round is allowed to give up.
			Deadline: stopAt,
		}
		tPoolStart := time.Now()
		var pool *mip.Pool
		if *model == "sticky" {
			// Sticky anchor + reach (design doc §12/§14): a round makes decisions
			// at the seed slot and the chosen waypoint list is copied forward over
			// the following slots, so a cell on slot s can only be relieved by a
			// round anchored at some t <= s (the copy never runs backwards).
			// The anchor alternates between the two round kinds of the doc's
			// schedule (even = hot, odd = early):
			//   - hot anchor (slot of the global hottest cell): the run is short
			//     (a divergence at that slot) so the MIP can *spend* Hamming
			//     budget to shave a cell that no free twin can reach;
			//   - early anchor (earliest hot slot): the copy reaches the later
			//     slots of the top-K set, reproducing the twin reach — a slot-1
			//     hot cell is relieved budget-free by a seed-0 copy that writes
			//     both slots, the only affordable shape when the budget is ~0.
			// Either way the whole top-K hot set inside the copy window is handed
			// to the pool so every reachable cell is ranked and relieved.
			// A hot (divergence) round spends Hamming budget, so it is only worth
			// running while the transition budgets still have headroom; with the
			// budget exhausted a divergence is unaffordable and the round can only
			// reject.  Remaining = sum over transitions of Budget - incumbent
			// cost.  The 8 threshold is above the ~2-4 units the cheapest
			// single-waypoint divergence costs.
			rem := 0
			for tt := 1; tt < T; tt++ {
				if tt < len(inst.Scenario.Budget) {
					rem += inst.Scenario.Budget[tt] - bestSnap.CostAt(tt)
				}
			}
			hot := r%2 == 0 && rem >= 8
			seed := hots[0].T
			if !hot {
				// early/free round: anchor at the earliest hot slot so the copy
				// reaches every decomposed cell (free twin relief).
				for _, k := range hots {
					if k.T < seed {
						seed = k.T
					}
				}
			}
			// hot round anchors at the global hottest cell's slot (seed stays
			// hots[0].T): the run is short and the MIP can spend budget.
			reach := seed + *stickySpan
			if reach > T-1 {
				reach = T - 1
			}
			// Keep only the cells the copy can actually reach; bookkeeping (fail /
			// done) must match the cells the round really attacks.
			kept := hots[:0]
			for _, k := range hots {
				if k.T >= seed && k.T <= reach {
					kept = append(kept, k)
				}
			}
			hots = kept
			if len(hots) == 0 {
				why["no-seed-hot"]++
				markFail()
				continue
			}
			hc := make([]mip.HotCell, len(hots))
			for i, k := range hots {
				hc[i] = mip.HotCell{Slot: k.T, Arc: k.A}
			}
			pool, err = gen.BuildSticky(seed, hc, *stickySpan)
		} else {
			hc := make([]mip.HotCell, len(hots))
			for i, k := range hots {
				hc[i] = mip.HotCell{Slot: k.T, Arc: k.A}
			}
			pool, err = gen.BuildCells(hc)
		}
		if err != nil {
			fatal(err)
		}
		if pool == nil {
			// sticky found no demand active on the seed slot: nothing to build.
			pool = &mip.Pool{}
		}
		tPool := time.Since(tPoolStart).Seconds()
		if timing() && debug(r) {
			fmt.Printf("    round %d pool %d pairs in %.2fs\n", r, len(pool.Pairs), tPool)
		}
		if len(pool.Pairs) == 0 {
			why["no-pool"]++
			markFail()
			continue
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
		tMipStart := time.Now()
		res, err := prob.Solve(mip.SolveOptions{MaxPeel: *peel, TimeLimit: limit})
		if err != nil {
			fatal(err)
		}
		if timing() && debug(r) {
			fmt.Printf("    round %d mip %.2fs status=%d\n", r, time.Since(tMipStart).Seconds(), res.Status)
		}
		if !res.HasValue || len(res.Choices) == 0 {
			// infeasible / not-proven / no candidate selection
			why["no-value"]++
			if debug(0) {
				fmt.Printf("    reject: mip status=%d no solution\n", res.Status)
			}
			markFail()
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
			markFail()
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
			markFail()
			continue
		}
		if eval.LexCompare(trialDesc, bestDesc) >= 0 {
			why["lex"]++
			if debug(0) {
				fmt.Printf("    reject: changed=%d but not lex better (mip obj=%.6f)\n", changed, res.ObjVal)
			}
			markFail()
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
			fail[k] = 0
		}
		if bestFirst < oldFirst {
			done = map[snap.Key]bool{}
			fail = map[snap.Key]int{}
			// Presolve pins are structural, not empirical: they stay retired.
			for k := range preFrozen {
				done[k] = true
			}
		}
		fmt.Printf("  round %d ACCEPT: first-bit rank %d -> %d  total_cost=%d changed=%d hots=%d\n",
			r, oldFirst, bestFirst, bestSnap.TotalCost(), changed, len(hots))
		if debug(r) {
			fmt.Printf("    top raw=%.9f  second raw=%.9f\n", bestDesc[0], bestDesc[1])
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
		fmt.Printf("  solves=%d avg pool pairs=%.1f avg cands=%.1f\n",
			solveCount, float64(poolSum)/float64(solveCount), float64(candSum)/float64(solveCount))
	}
	// Candidate layer reporting.  The hop counters are what distinguishes the
	// cache arms of the benchmark: same queries, different "computed" count.
	fmt.Printf("  cand mode=%s pool_cap=%d max_w1=%d max_w2=%d global_k=%d hop_cache=%v\n",
		candOpts.Mode, candOpts.PoolCap, candOpts.MaxW1, candOpts.MaxW2, candOpts.GlobalK, *candHopCache)
	hHits, hMiss, hComp := hopCache.Stats()
	fmt.Printf("  hops queries=%d hits=%d misses=%d computed=%d sets=%d\n",
		hHits+hMiss, hHits, hMiss, hComp, hopCache.Sets())

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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
