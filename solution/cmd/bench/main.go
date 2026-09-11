// Command bench runs the M3 outer-loop solver over every setA instance and
// writes a quantified per-instance summary (RESULT line + sprint compare) into
// a TSV.  Instances whose row already exists in the TSV are skipped, so the
// bench can be resumed after a wall-clock interruption.  Each instance runs as
// its own solve.exe child with an optional -wall-sec cap.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	bin := flag.String("bin", "_pyref/solve.exe", "solver binary")
	dir := flag.String("dir", "_pyref/bench_m4", "output directory")
	rounds := flag.Int("rounds", 150, "rounds per instance")
	wallSec := flag.Int("wall-sec", 0, "wall-clock cap per instance in seconds (0 = none)")
	hotK := flag.Int("hot-k", 6, "hot cells per round")
	peel := flag.Int("peel", 6, "lex peel depth")
	budgetMode := flag.String("budget-mode", "full", "solver Hamming budget mode: full or first_half")
	presolve := flag.String("presolve", "unmovable", "solver presolve mode: unmovable or off")
	presolveSec := flag.Float64("presolve-time", 15, "solver presolve time budget in seconds")
	presolveDeep := flag.Int("presolve-deep", 100, "solver: max cells examined by the exact (class 3) presolve pass, 0 = uncapped")
	skipFloor := flag.Bool("skip-floor", true, "solver: do not seed a round on a cell sitting on its presolve floor")
	candMode := flag.String("cand-mode", "mix", "solver: candidate generator (mix|hot_center|od_scan|bottleneck|residual, or any \"+\"-joined subset; mix = hot_center+od_scan+residual)")
	candPoolCap := flag.Int("cand-pool-cap", 24, "solver: candidate nodes kept per strategy pool")
	candW1 := flag.Int("cand-max-w1", 24, "solver: max 1-waypoint candidates per demand per family")
	candW2 := flag.Int("cand-max-w2", 32, "solver: max 2-waypoint candidates per demand per family")
	candHopCache := flag.Bool("cand-hop-cache", true, "solver: retain hop BFS per banned-arc set (false = uncached control)")
	candGlobalK := flag.Int("cand-global-k", 0, "solver: global-relief safety net size (0 = off)")
	only := flag.String("only", "", "comma-separated instance suffixes to run, e.g. 01,04,19 (default: all 20)")
	schedule := flag.String("schedule", "hot", "solver: round schedule (hot|pingpong)")
	focusN := flag.Int("focus-n", 10, "solver: pingpong epoch focus size")
	epochTopN := flag.Int("epoch-top-n", 12, "solver: pingpong end-of-epoch test on the top-N hottest cells (0 = off)")
	growMult := flag.Int("grow-mult", 2, "solver: pingpong scale multiplier per epoch restart")
	scaleCap := flag.Int("scale-cap", 8, "solver: pingpong search-scale ceiling (1,2,4,...,cap)")
	confGate := flag.Bool("conf-gate", true, "solver: freeze on accumulated confidence instead of counting every rejection")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal(err)
	}
	tsvPath := filepath.Join(*dir, "summary.tsv")
	doneRows := map[string]bool{}
	if f, err := os.Open(tsvPath); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.IndexByte(line, '\t'); i > 0 {
				doneRows[line[:i]] = true
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(tsvPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	header := "instance\taccepted\tfirstbit\ttotal_cost\tbudget_ok\ttie_layers\ttotal_layers\tfirst_gap_layer\tours\tref\tsecs\tsolves\thop_queries\thop_computed"
	if len(doneRows) == 0 {
		fmt.Fprintln(f, header)
	}

	for _, inst := range instanceList(*only) {
		if doneRows[inst] {
			fmt.Printf("== %s (cached)\n", inst)
			continue
		}
		fmt.Printf("== %s ==\n", inst)
		start := time.Now()
		args := []string{
			"-prefix", "../setA/" + inst,
			"-out", filepath.Join(*dir, inst+".json"),
			"-sprint", "../sprint_results/loads_vector.csv",
			"-rounds", fmt.Sprint(*rounds),
			"-hot-k", fmt.Sprint(*hotK),
			"-peel", fmt.Sprint(*peel),
			"-budget-mode", *budgetMode,
			"-presolve", *presolve,
			"-presolve-time", fmt.Sprintf("%g", *presolveSec),
			"-presolve-deep", fmt.Sprint(*presolveDeep),
			fmt.Sprintf("-skip-floor=%v", *skipFloor),
			"-cand-mode", *candMode,
			"-cand-pool-cap", fmt.Sprint(*candPoolCap),
			"-cand-max-w1", fmt.Sprint(*candW1),
			"-cand-max-w2", fmt.Sprint(*candW2),
			"-cand-global-k", fmt.Sprint(*candGlobalK),
			// Bool flags need the "=" form: a bare "-cand-hop-cache false"
			// parses as true plus a stray positional argument.
			fmt.Sprintf("-cand-hop-cache=%v", *candHopCache),
			"-schedule", *schedule,
			"-focus-n", fmt.Sprint(*focusN),
			"-epoch-top-n", fmt.Sprint(*epochTopN),
			"-grow-mult", fmt.Sprint(*growMult),
			"-scale-cap", fmt.Sprint(*scaleCap),
			fmt.Sprintf("-conf-gate=%v", *confGate),
		}
		if *wallSec > 0 {
			args = append(args, "-wall-sec", fmt.Sprint(*wallSec))
		}
		// The child gets -wall-sec and is supposed to honour it, but bench is the
		// backstop of last resort: a child stuck somewhere it cannot check must
		// not hang the whole sweep (that instance never returns and every later
		// instance is never measured).  The kill fires a minute beyond the
		// child's own budget, so a well-behaved child never sees it.
		ctx, cancel := context.WithCancel(context.Background())
		if *wallSec > 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(*wallSec+60)*time.Second)
		}
		cmd := exec.CommandContext(ctx, *bin, args...)
		// Separate buffers, not one shared one: exec writes stdout and stderr
		// from two goroutines, and bytes.Buffer is not safe for concurrent
		// writes.  Sharing it garbles lines nondeterministically -- in practice
		// the tail-of-run "RESULT"/"sprint compare" lines survived (they are
		// printed after Gurobi stops) while the mid-run "solves=" and "hops "
		// counters did not, which silently turned those columns into NA.
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		_ = cmd.Run() // solver exit codes are not meaningful for the summary
		cancel()

		res, cmp := extract(&out, &errb)
		st := extractStats(&out, &errb)
		st.secs = time.Since(start).Seconds()
		if res == "" {
			// No RESULT line means the solver crashed or was killed.  Do not
			// persist a row: an NA row would be skipped as "cached" on the next
			// run and the instance would silently never be measured.
			fmt.Printf("  %s\tNO RESULT (skipped, will retry on rerun)  (%.1fs)\n",
				inst, time.Since(start).Seconds())
			continue
		}
		line := row(inst, res, cmp, st)
		if _, err := fmt.Fprintln(f, line); err != nil {
			fatal(err)
		}
		doneRows[inst] = true
		fmt.Printf("  %s  (%.1fs)\n", line, time.Since(start).Seconds())
	}
}

func instanceList(only string) []string {
	if only != "" {
		var out []string
		for _, s := range strings.Split(only, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			out = append(out, "setA-"+s)
		}
		return out
	}
	out := make([]string, 20)
	for i := range out {
		out[i] = fmt.Sprintf("setA-%02d", i+1)
	}
	return out
}

// stats carries the solver-side counters the summary records alongside the
// result: how long the instance took and what the hop cache absorbed.
type stats struct {
	secs        float64
	solves      string
	hopQueries  string
	hopComputed string
}

// solverLines splits a captured stream into the solver's own lines.  Gurobi
// writes its log NUL-separated and with no newlines, so a line boundary in the
// raw stream is any of \n, \r or \x00 -- matching only on \n leaves the solver's
// counters glued to the tail of a Gurobi token and silently drops them.
// Verified against a real run: the same stream scanned on \n alone loses
// "solves=", scanned on all three it keeps it.
func solverLines(bufs ...*bytes.Buffer) []string {
	var out []string
	for _, buf := range bufs {
		for _, chunk := range strings.FieldsFunc(buf.String(), func(r rune) bool {
			return r == '\n' || r == '\r' || r == 0
		}) {
			// FieldsFunc keeps the leading indent, so trim before prefix-matching.
			if chunk = strings.TrimSpace(chunk); chunk != "" {
				out = append(out, chunk)
			}
		}
	}
	return out
}

// extract pulls the "RESULT ..." and "sprint compare: ..." lines out of the
// solver output.  Both streams are scanned: which one carries the solver's own
// prints is Gurobi's business, not ours.
func extract(bufs ...*bytes.Buffer) (res, cmp string) {
	for _, line := range solverLines(bufs...) {
		if res == "" && strings.HasPrefix(line, "RESULT ") {
			res = line
		}
		if strings.HasPrefix(line, "sprint compare: ") {
			cmp = line
		}
	}
	return res, cmp
}

// extractStats reads the solves / hop-cache counters the solver prints.
func extractStats(bufs ...*bytes.Buffer) stats {
	var st stats
	for _, line := range solverLines(bufs...) {
		switch {
		case strings.HasPrefix(line, "solves="):
			st.solves = line[len("solves="):]
			if i := strings.IndexByte(st.solves, ' '); i > 0 {
				st.solves = st.solves[:i]
			}
		case strings.HasPrefix(line, "hops "):
			for _, tok := range strings.Fields(line) {
				if v, ok := strings.CutPrefix(tok, "queries="); ok {
					st.hopQueries = v
				}
				if v, ok := strings.CutPrefix(tok, "computed="); ok {
					st.hopComputed = v
				}
			}
		}
	}
	return st
}

func row(inst, res, cmp string, st stats) string {
	field := func(pattern string) string {
		i := strings.Index(res, pattern)
		if i < 0 {
			return "NA"
		}
		rest := res[i+len(pattern):]
		j := strings.IndexAny(rest, " \t")
		if j < 0 {
			return rest
		}
		return rest[:j]
	}
	na := func(v string) string {
		if v == "" {
			return "NA"
		}
		return v
	}
	if res == "" {
		return fmt.Sprintf("%s\tNA\tNA\tNA\tNA\t0\tNA\t0\tNA\tNA\t%.1f\t%s\t%s\t%s",
			inst, st.secs, na(st.solves), na(st.hopQueries), na(st.hopComputed))
	}
	acc := field("accepted=")
	fb := field("firstbit=")
	tc := field("total_cost=")
	bo := field("budget_ok=")
	// cmp: tie 3/160 layers; first gap layer 4 (.. vs ..)
	tie, tot, gl, ours, ref := "0", "NA", "0", "NA", "NA"
	if cmp != "" {
		if i := strings.Index(cmp, "tie "); i >= 0 {
			mid := cmp[i+4:]
			if j := strings.IndexByte(mid, '/'); j >= 0 {
				tie = mid[:j]
			}
		}
		if i := strings.Index(cmp, "/"); i >= 0 {
			rest := cmp[i+1:]
			if j := strings.IndexAny(rest, " layer"); j >= 0 {
				tot = rest[:j]
			}
		}
		if i := strings.Index(cmp, "first gap layer "); i >= 0 {
			rest := cmp[i+len("first gap layer "):]
			if j := strings.IndexByte(rest, ' '); j >= 0 {
				gl = rest[:j]
				tail := rest[j+1:] // "(.. vs ..)"
				if k := strings.IndexByte(tail, '('); k >= 0 {
					body := tail[k+1:]
					if m := strings.Index(body, " vs "); m >= 0 {
						ours = body[:m]
						ref = strings.TrimRight(body[m+4:], ")")
					}
				}
			}
		}
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.1f\t%s\t%s\t%s",
		inst, acc, fb, tc, bo, tie, tot, gl, ours, ref,
		st.secs, na(st.solves), na(st.hopQueries), na(st.hopComputed))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
