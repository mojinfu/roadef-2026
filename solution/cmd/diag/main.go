// Command diag reports where a solve's wall clock actually goes, phase by
// phase, without running a search.  It exists because the budget question is
// "can this instance finish inside the competition limit", and answering it
// needs the fixed costs (parse, graph build, the empty-snapshot ECMP pass, the
// structural presolve scan) separated from each other -- they have completely
// different fixes.
//
// Usage: diag -prefix ../setB/setB-11 [-presolve-time 15] [-index-size 0]
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"tasr/internal/ecmp"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/presolve"
	"tasr/internal/snap"
)

func main() {
	prefix := flag.String("prefix", "", "instance prefix, e.g. setB/setB-11")
	presolveSec := flag.Float64("presolve-time", 15, "presolve budget to measure (0 = skip)")
	indexSize := flag.Int("index-size", 0, "graph index LRU capacity (0 = size to the instance, as solve does)")
	flag.Parse()
	if *prefix == "" {
		fmt.Fprintln(os.Stderr, "usage: diag -prefix setB/setB-11")
		os.Exit(2)
	}

	type phase struct {
		name string
		dur  time.Duration
	}
	var phases []phase
	mark := func(name string, start time.Time) time.Time {
		now := time.Now()
		phases = append(phases, phase{name, now.Sub(start)})
		return now
	}

	t := time.Now()
	inst, err := io.LoadInstance(*prefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	t = mark("load (parse 3 json)", t)

	g := graph.New(inst)
	t = mark("graph build", t)

	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	ixSize := *indexSize
	if ixSize <= 0 {
		ixSize = 4 * g.N * inst.NSlots
		if ixSize < 8192 {
			ixSize = 8192
		}
	}
	cache.Index().SetMaxSize(ixSize)
	t = mark("ecmp cache + index size", t)

	sn, err := snap.NewEmpty(inst, g, cache)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	t = mark("empty snapshot (all (d,t) ECMP, cold)", t)

	// The cold pass computes every distinct atom once.  Every round then builds
	// a trial snapshot from a full re-route (Snap.New -> rerouteAll), which
	// pays the O(demands*slots*m) add loop again with a warm atom cache.  That
	// warm number is the per-round fixed cost, so measure it rather than guess.
	if _, err := snap.NewEmpty(inst, g, cache); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	t = mark("trial snapshot (warm, per-round cost)", t)

	var pDur time.Duration
	if *presolveSec > 0 {
		pOpts := presolve.Options{
			Mode:      presolve.Unmovable,
			TimeLimit: time.Duration(*presolveSec * float64(time.Second)),
			MaxDeep:   100,
		}
		rep, err := presolve.Run(inst, g, sn, presolve.NewBounds(), pOpts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		t = mark("presolve", t)
		pDur = rep.Elapsed
		fmt.Printf("presolve detail: elapsed=%.2fs cells=%d deep=%d floors=%d timeout=%v\n",
			rep.Elapsed.Seconds(), rep.Cells, rep.Deep, rep.Pinned, rep.TimedOut)
	}

	fmt.Printf("instance=%s n=%d m=%d demands=%d T=%d index_size=%d\n",
		inst.Name, g.N, g.M, inst.NDemands(), inst.NSlots, ixSize)
	var total time.Duration
	for _, p := range phases {
		fmt.Printf("  %-34s %8.2fs\n", p.name, p.dur.Seconds())
		total += p.dur
	}
	fmt.Printf("  %-34s %8.2fs\n", "TOTAL (fixed cost before search)", total.Seconds())
	if pDur > 0 {
		fmt.Printf("  note: presolve's own elapsed was %.2fs of that\n", pDur.Seconds())
	}
}
