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

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/mip"
	"tasr/internal/model"
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

	for r := 1; r <= *rounds; r++ {
		keys := bestSnap.RankKeys(done)
		if len(keys) == 0 {
			break
		}
		sat := bestSnap.Saturations()
		if sat[keys[0].A*T+keys[0].T] <= 0 {
			// All remaining non-done keys carry no load; nothing left to relieve.
			break
		}
		// Focus the slot of the global hottest key; decompose its top hot arcs.
		focusT := keys[0].T
		var hots []snap.Key
		var hotArc []int
		for _, k := range keys {
			if k.T != focusT {
				continue
			}
			if sat[k.A*T+k.T] <= 0 {
				break
			}
			hots = append(hots, k)
			hotArc = append(hotArc, k.A)
			if len(hots) >= *hotK {
				break
			}
		}
		if len(hots) == 0 {
			break
		}
		if debug(r) {
			arcs := make([]int, len(hots))
			for i := range hots {
				arcs[i] = hots[i].A
			}
			fmt.Printf("round %d: focus slot=%d arcs=%v sats=[%.4f..%.4f] done=%d\n",
				r, focusT, arcs, sat[hots[len(hots)-1].A*T+focusT], sat[hots[0].A*T+focusT], len(done))
		}

		// Mark one rejected round against every hot arc of the focus.
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
		pool, err := gen.BuildKeys(focusT, hotArc)
		if err != nil {
			fatal(err)
		}
		if len(pool.Pairs) == 0 {
			why["no-pool"]++
			markFail()
			continue
		}

		prob, err := mip.Build(pool, bestSnap)
		if err != nil {
			fatal(err)
		}
		poolSum += len(pool.Pairs)
		for _, pr := range pool.Pairs {
			candSum += len(pr.Cand)
		}
		solveCount++
		res, err := prob.Solve(mip.SolveOptions{MaxPeel: *peel})
		if err != nil {
			fatal(err)
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

		// Materialise the chosen waypoints on a trial solution.
		cur := bestSnap.Solution()
		trial := cur.Copy()
		changed := 0
		for _, c := range res.Choices {
			if !equalWps(cur.Waypoints[c.D][c.T], c.Wps) {
				trial.Set(c.D, c.T, c.Wps)
				changed++
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

		// Accept.  Any move proves the involved arcs are movable, so their
		// failure memory is void; a first-bit improvement voids every prior
		// "this arc cannot be moved" conclusion.
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
		fmt.Printf("  round %d ACCEPT: first-bit rank %d -> %d  total_cost=%d changed=%d focus=(t=%d)\n",
			r, oldFirst, bestFirst, bestSnap.TotalCost(), changed, focusT)
		if debug(r) {
			fmt.Printf("    top raw=%.9f  second raw=%.9f\n", bestDesc[0], bestDesc[1])
		}
	}

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
	if *sprint != "" {
		compareSprint(*sprint, bestDesc, inst)
	}
	if *out != "" {
		if err := io.WriteSolution(*out, fin, inst); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote solution -> %s\n", *out)
	}
}

// debug gates verbose per-round output behind an env var (kept small for runs).
func debug(r int) bool {
	v, err := strconv.Atoi(os.Getenv("TASR_SOLVE_VERBOSE"))
	return err == nil && v > 0 && r%max(1, v) == 0
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
