"""Run the v0.1 local-search solver across all setA instances and report.

For each instance:
  * baseline MLU (empty waypoints)
  * solver MLU + top-5 loads (full precision and trunc6)
  * inter-slot distance used vs budget
  * lexicographic comparison against the sprint best load vector
    (loads_vector.csv): -1 => we are strictly better, 0 => equal on the
    truncated grid, +1 => strictly worse.
Writes the best solution found to `--outdir/<name>-srpaths.json`.
"""
import argparse
import csv
import glob
import json
import re
import sys
import time
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from tasr.algos.search import SearchState, solve_with_time_budget
from tasr.ecmp import AtomCache
from tasr.eval import Evaluator
from tasr.eval.objective import CompareSpec, lex_compare_sorted, saturations_vector, truncate_loads
from tasr.io import load_instance, write_solution
from tasr.model import DirectedGraph, Solution

SPEC = CompareSpec()


def load_best_vectors():
    """instance -> sorted-desc load vector from sprint best (trunc6 grid)."""
    out = {}
    with open("sprint_results/loads_vector.csv", newline="") as f:
        r = csv.reader(f)
        next(r)
        for row in r:
            vals = [float(x) for x in row[2:] if x not in ("", "nan")]
            out[row[0]] = np.array(vals)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--set", default="setA", choices=["setA", "setB"])
    ap.add_argument("--outdir", default="runs")
    ap.add_argument("--instances", default="", help="comma list to run only a subset")
    ap.add_argument("--time", type=float, default=20.0, help="per-instance solver seconds")
    ap.add_argument("--restarts", type=int, default=2)
    args = ap.parse_args()

    best_vectors = load_best_vectors()
    files = sorted(glob.glob(f"{args.set}/*-net.json"),
                   key=lambda f: int(re.search(r"(\d+)", f).group(1)))
    if args.instances:
        want = set(args.instances.split(","))
        files = [f for f in files if Path(f).stem.replace("-net", "") in want]

    outdir = Path(args.outdir)
    outdir.mkdir(exist_ok=True)
    header = (f"{'inst':>10} {'dem':>5} {'bl_mlu':>9} {'sr_mlu':>9} "
              f"{'budget':>7} {'dist':>5} {'top5(trunc6)':>60} {'vs_best':>7} {'sec':>6}")
    print(header)
    for nf in files:
        name = Path(nf).stem.replace("-net", "")
        pre = nf[: -len("-net.json")]
        t0 = time.time()
        inst = load_instance(f"{pre}-net.json", f"{pre}-tm.json", f"{pre}-scenario.json",
                             name=name)
        graph = DirectedGraph.build(inst)
        cache = AtomCache(graph, inst.scenario.interventions, maxsize=200_000)
        ev = Evaluator(inst, graph, cache)
        bl = ev.saturations(Solution.empty(inst.n_demands, inst.n_slots))
        sol, state, sat = solve_with_time_budget(inst, graph, cache,
                                                 time_budget=args.time,
                                                 restarts=args.restarts)
        elapsed = time.time() - t0
        budget = state.budget1
        dist = state.distance()
        v = saturations_vector(sat)
        trunc = truncate_loads(v)
        top5 = ",".join(f"{x:.6f}" for x in trunc[:5])
        vs = "   -"
        if name in best_vectors:
            c = lex_compare_sorted(v, best_vectors[name], 6)
            vs = {-1: "BETTER", 0: "equal", 1: "worse"}[c] if c in (-1, 0, 1) else str(c)
        out_path = outdir / f"{name}-srpaths.json"
        write_solution(out_path, sol, inst)
        print(f"{name:>10} {inst.n_demands:>5} {bl.max():>9.6f} {sat.max():>9.6f} "
              f"{str(budget):>7} {dist:>5} {top5:>60} {vs:>7} {elapsed:>6.1f}")


if __name__ == "__main__":
    main()
