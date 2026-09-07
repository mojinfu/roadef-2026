"""Compare two solution runs instance-by-instance on the official objective.

For every instance common to both ``--dirA`` / ``--dirB`` (each holding
``<name>-srpaths.json``), recompute the trunc6 lexicographic load vector and
report:

  * best-vector winner of A vs B (whole vector, trunc6) and, if not tied,
    the first divergent layer + both values + absolute/relative gap;
  * for each run: number of lex layers tied with the sprint reference
    (``--ref sprint_results/loads_vector.csv``) when given.

Quantified per-instance reporting convention: this is the raw data behind any
"X/Y better than Y" claim.
"""
from __future__ import annotations

import argparse
import csv
import glob
import json
import os
import re
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from tasr.ecmp import AtomCache
from tasr.eval import Evaluator
from tasr.eval.objective import lex_compare_sorted, saturations_vector, truncate_loads
from tasr.io import load_instance, read_solution
from tasr.model import DirectedGraph, Solution

SPEC_DEC = 6


def load_best_vectors(path: str):
    out = {}
    with open(path, newline="") as f:
        r = csv.reader(f)
        next(r)
        for row in r:
            vals = [float(x) for x in row[2:] if x not in ("", "nan")]
            out[row[0]] = np.array(vals)
    return out


def first_divergence(a: np.ndarray, b: np.ndarray, dec: int = SPEC_DEC):
    """Lex layers (trunc6) equal, then 1-based index/values of first gap."""
    ta, tb = truncate_loads(a), truncate_loads(b)
    n = min(len(ta), len(tb))
    n_equal = 0
    while n_equal < n and ta[n_equal] == tb[n_equal]:
        n_equal += 1
    if n_equal == n and len(ta) == len(tb):
        return n_equal, None, None, None, None
    i = n_equal
    return (n_equal, i + 1, ta[i] if i < len(ta) else None,
            tb[i] if i < len(tb) else None, None)


def vector_for(name: str, srpaths_path: str):
    pre = f"setA/{name}"
    inst = load_instance(f"{pre}-net.json", f"{pre}-tm.json",
                         f"{pre}-scenario.json", name=name)
    graph = DirectedGraph.build(inst)
    cache = AtomCache(graph, inst.scenario.interventions, maxsize=200_000)
    sol = read_solution(srpaths_path, inst)
    sat = Evaluator(inst, graph, cache).saturations(sol)
    return saturations_vector(sat)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dirA", required=True)
    ap.add_argument("--dirB", required=True)
    ap.add_argument("--ref", default="sprint_results/loads_vector.csv")
    args = ap.parse_args()

    ref = load_best_vectors(args.ref) if os.path.exists(args.ref) else {}
    files = sorted(glob.glob(os.path.join(args.dirA, "*-srpaths.json")))
    header = (f"{'inst':>10} {'vecA=vecB':>9} {'nEqAB':>6} {'divLyrAB':>8} "
              f"{'A_div':>10} {'B_div':>10} {'nEqA_ref':>8} {'nEqB_ref':>8} {'winner':>6}")
    print(header)
    for fa in files:
        name = Path(fa).stem.replace("-srpaths", "")
        fb = os.path.join(args.dirB, Path(fa).name)
        if not os.path.exists(fb):
            continue
        va = vector_for(name, fa)
        vb = vector_for(name, fb)
        n_eq, div, va_div, vb_div, _ = first_divergence(va, vb)
        winner = "tie" if div is None else ("A" if lex_compare_sorted(
            va, vb, SPEC_DEC) == -1 else "B")
        n_ref_a = first_divergence(va, ref[name])[0] if name in ref else -1
        n_ref_b = first_divergence(vb, ref[name])[0] if name in ref else -1
        print(f"{name:>10} {str(div is None):>9} {n_eq:>6} "
              f"{str(div):>8} {str(va_div):>10} {str(vb_div):>10} "
              f"{n_ref_a:>8} {n_ref_b:>8} {winner:>6}")


if __name__ == "__main__":
    main()
