// Command solve runs the M3 outer loop of the V1.0 solver on one instance.
//
// It starts from an empty routing snapshot and iterates decompose-MIP rounds:
// pick the globally hottest (slot, arc) that is not marked done, build a
// candidate pool of single-waypoint reroutes that relieve it, solve the
// lexicographic-peeling selection MIP (subject to the Hamming budget), accept
// the result only when the real truncated-lex saturation vector improves.
//
// Seed bookkeeping follows the design doc: a seed that the MIP cannot improve
// is counted, and after failLimit failures it is marked done and skipped.
// done/fail memory is cleared only when the first bit (max saturation) strictly
// improves, because then every "this arc cannot be moved" conclusion is void.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/mip"
	"tasr/internal/model"
	"tasr/internal/monitor"
	"tasr/internal/snap"
)

func main() {
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
	wallSec := flag.Int("wall-sec", 0, "stop the round loop after this many seconds (0 = unlimited)")
	mipSec := flag.Float64("mip-sec", 20, "per peel-layer Gurobi time cap in seconds (0 = solve to completion)")
	monitorOn := flag.Bool("monitor", false, "serve a live progress page at http://127.0.0.1:<port> and open the browser")
	monitorPort := flag.Int("monitor-port", 8765, "live view port (0 = pick a free port; busy port falls back to free)")
	monitorTop := flag.Int("monitor-top", 10, "bars per time slot on the live view page")
	monitorTopN := flag.Int("monitor-topn", 10, "top-N hottest cells listed at the top of the live view page")
	monitorLinger := flag.Int("monitor-linger", 45, "seconds to keep the live view alive after solving stops (0 = exit immediately)")
	flag.Parse()
	if *prefix == "" {
		fmt.Fprintln(os.Stderr, "usage: solve -prefix setA/setA-01 [-out sol.json] [-sprint loads_vector.csv]")
		os.Exit(2)
	}

	inst, err := io.LoadInstance(*prefix)
	if err != nil {
		fatal(err)
	}
	g := graph.New(inst)
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	sn, err := snap.NewEmpty(inst, g, cache)
	if err != nil {
		fatal(err)
	}

	bestSnap := sn
	bestDesc := eval.SortedDesc(bestSnap.Saturations())
	bestFirst := eval.RankInt(bestDesc[0])
	m, T := g.M, inst.NSlots
	fmt.Printf("instance=%s m=%d T=%d demands=%d\n", inst.Name, m, T, inst.NDemands())
	fmt.Printf("  start first-bit raw=%.9f rank=%d  total_cost=%d\n",
		bestDesc[0], bestFirst, bestSnap.TotalCost())

	done := map[snap.Key]bool{}
	fail := map[snap.Key]int{}
	accepted, rejected := 0, 0
	why := map[string]int{}
	var poolSum, candSum, solveCount int
	deadline := time.Time{}
	if *wallSec > 0 {
		deadline = time.Now().Add(time.Duration(*wallSec) * time.Second)
	}

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
		if !deadline.IsZero() && time.Now().After(deadline) {
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
		publish("running", "", r, hots)
		if debug(r) {
			fmt.Printf("round %d: hot cells=%v sat=[%.4f..%.4f] done=%d\n",
				r, hots, sat[hots[len(hots)-1].A*T+hots[len(hots)-1].T],
				sat[hots[0].A*T+hots[0].T], len(done))
		}

		// Mark one rejected round against every hot cell.
		markFail := func() {
			for _, k := range hots {
				fail[k]++
				if fail[k] >= *failLimit {
					done[k] = true
				}
			}
			rejected++
		}

		gen := &mip.Generator{Inst: inst, G: g, Snap: bestSnap, MaxCandPerDemand: 12}
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
		})
		if err != nil {
			fatal(err)
		}
		poolSum += len(pool.Pairs)
		for _, pr := range pool.Pairs {
			candSum += len(pr.Cand)
		}
		solveCount++
		limit := time.Duration(*mipSec * float64(time.Second))
		// When a wall deadline is set, trim the per-layer cap so one round can
		// not run far past it (the loop re-checks the deadline each round).
		if !deadline.IsZero() && *mipSec > 0 {
			remain := time.Until(deadline)
			if rem := remain / time.Duration(*peel+1); rem < limit {
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
		accepted++
		for _, k := range hots {
			delete(done, k)
			fail[k] = 0
		}
		if bestFirst < oldFirst {
			done = map[snap.Key]bool{}
			fail = map[snap.Key]int{}
		}
		fmt.Printf("  round %d ACCEPT: first-bit rank %d -> %d  total_cost=%d changed=%d hots=%d\n",
			r, oldFirst, bestFirst, bestSnap.TotalCost(), changed, len(hots))
		if debug(r) {
			fmt.Printf("    top raw=%.9f  second raw=%.9f\n", bestDesc[0], bestDesc[1])
		}
	}
	publish("done", "solver loop finished", 0, nil)

	fmt.Printf("done: accepted=%d rejected=%d  final first-bit raw=%.9f rank=%d  total_cost=%d budget_ok=%v\n",
		accepted, rejected, bestDesc[0], bestFirst, bestSnap.TotalCost(), bestSnap.BudgetOK())
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

	fin := bestSnap.Solution()
	fmt.Printf("RESULT %s accepted=%d firstbit=%d total_cost=%d budget_ok=%v\n",
		inst.Name, accepted, bestFirst, bestSnap.TotalCost(), bestSnap.BudgetOK())
	if *sprint != "" {
		compareSprint(*sprint, bestDesc, inst)
	}
	if *out != "" {
		if err := io.WriteSolution(*out, fin, inst); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote solution -> %s\n", *out)
	}
	if mon != nil {
		if *monitorLinger > 0 {
			fmt.Printf("solve finished; live view stays up for %ds (Ctrl-C to exit now).\n", *monitorLinger)
			time.Sleep(time.Duration(*monitorLinger) * time.Second)
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
