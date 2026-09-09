// Command mipround runs a single M2 decompose+MIP round on one instance: it
// takes the current routing (empty by default), targets the globally hottest
// (slot, arc), builds a candidate pool of single-waypoint reroutes that relieve
// that arc, solves a Gurobi MIP selecting one candidate per demand to minimise
// the maximum saturation, and accepts the outcome only when the real
// lexicographic saturation vector improves.
package main

import (
	"flag"
	"fmt"
	"os"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/mip"
	"tasr/internal/snap"
)

func main() {
	prefix := flag.String("prefix", "", "instance prefix, e.g. setA/setA-01")
	out := flag.String("out", "", "write accepted srpaths to this file")
	peel := flag.Int("peel", 8, "lex peel depth (layers of z)")
	flag.Parse()
	if *prefix == "" {
		fmt.Fprintln(os.Stderr, "usage: mipround -prefix setA/setA-01 [-out sol.json]")
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
	bestSol := sn.Solution()
	bestSat := sn.Saturations()
	bestDesc := eval.SortedDesc(bestSat)
	m, T := g.M, inst.NSlots

	fmt.Printf("instance=%s m=%d T=%d demands=%d\n", inst.Name, m, T, inst.NDemands())
	fmt.Printf("  start first-bit raw=%.9f rank=%d  total_cost=%d\n",
		bestDesc[0], eval.RankInt(bestDesc[0]), sn.TotalCost())

	_ = bestDesc
	// Global hot key on the current solution.
	ht, ha := -1, -1
	hsat := -1.0
	for a := 0; a < m; a++ {
		for t := 0; t < T; t++ {
			v := bestSat[a*T+t]
			if v > hsat {
				hsat, ht, ha = v, t, a
			}
		}
	}
	fmt.Printf("  hot key: slot=%d arc=%d sat=%.9f\n", ht, ha, hsat)

	gen := &mip.Generator{Inst: inst, G: g, Snap: sn, MaxCandPerDemand: 8}
	pool, err := gen.Build(ht, ha)
	if err != nil {
		fatal(err)
	}
	nCand := 0
	for _, pr := range pool.Pairs {
		nCand += len(pr.Cand)
	}
	fmt.Printf("  pool: pairs=%d candidates=%d\n", len(pool.Pairs), nCand)
	if len(pool.Pairs) == 0 {
		fmt.Println("  no candidate reroutes relieve the hot arc; nothing to solve")
		return
	}

	prob, err := mip.Build(pool, sn)
	if err != nil {
		fatal(err)
	}
	res, err := prob.Solve(mip.SolveOptions{MaxPeel: *peel})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("  mip status=%d obj=%.9f has_value=%v\n", res.Status, res.ObjVal, res.HasValue)
	if !res.HasValue || len(res.Choices) == 0 {
		fmt.Println("  no MIP solution; no change")
		return
	}

	// Build the trial solution.
	trial := bestSol.Copy()
	changed := 0
	for _, c := range res.Choices {
		// Only materialise when the candidate actually differs from current.
		if !equalWps(trial.Waypoints[c.D][c.T], c.Wps) {
			trial.Set(c.D, c.T, c.Wps)
			changed++
		}
	}
	fmt.Printf("  changed (d,t) pairs: %d\n", changed)
	if changed == 0 {
		fmt.Println("  MIP chose current routing everywhere; no change")
		return
	}

	trialSnap, err := snap.New(inst, g, cache, trial)
	if err != nil {
		fatal(err)
	}
	trialSat := trialSnap.Saturations()
	trialDesc := eval.SortedDesc(trialSat)
	lex := eval.LexCompare(trialDesc, bestDesc)
	if lex >= 0 {
		fmt.Println("  REJECTED: trial not lexicographically better")
		return
	}
	layer, aq, bq := eval.FirstDivergence(trialDesc, bestDesc)
	fmt.Printf("  ACCEPTED: first divergence at layer %d (%d vs %d)\n", layer, aq, bq)
	fmt.Printf("  new first-bit raw=%.9f rank=%d  total_cost=%d budget_ok=%v\n",
		trialDesc[0], eval.RankInt(trialDesc[0]), trialSnap.TotalCost(), trialSnap.BudgetOK())

	if *out != "" {
		if err := io.WriteSolution(*out, trial, inst); err != nil {
			fatal(err)
		}
		fmt.Printf("  wrote solution -> %s\n", *out)
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
