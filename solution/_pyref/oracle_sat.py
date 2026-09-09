"""Bit-exact evaluation oracle (reference Python implementation).

Computes the saturation matrix for a routing solution the same way the old
(toy) solver did -- ECMP atoms from tasr/ecmp + dense superposition in
tasr/eval -- and dumps it in a parseable text format for cross-checking the Go
port.

The Python reference was validated 40/40 against the official checker (exp02 /
exp03), so Go bit-parity with this output implies checker parity.

Usage:
    python oracle_sat.py <inst-prefix> [<srpaths.json>] [<out.txt>]

    <inst-prefix>  e.g. setA/setA-01  (resolves *-net/tm/scenario.json)
    <srpaths.json> optional solution file; omitted => empty solution
    <out.txt>      default stdout

Output format (one record per line):
    HDR m=<m> n_slots=<t> n_demands=<d> n_nodes=<n>
    A <slot> <arc> <sat as %.17g>          (slot-major, arc-minor, raw float64)
    M <slot> <arc> <trunc6 int>            (floor(sat*1e6 + 1e-6))
    V <desc-vector entry as %.17g>         (m*t entries, saturations sorted desc)
    Q <trunc6 int of the desc-vector entry>
Ordering of A/M and V/Q rows must be replicated exactly by the Go side.
"""
from __future__ import annotations

import json
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0] if "/" in __file__ else ".")

import numpy as np

from tasr.eval.evaluator import Evaluator
from tasr.eval.objective import CompareSpec
from tasr.io.parser import load_instance
from tasr.io.reader import read_solution
from tasr.model.graph import DirectedGraph
from tasr.ecmp import AtomCache


def main() -> None:
    prefix = sys.argv[1]
    srpaths = sys.argv[2] if len(sys.argv) > 2 and sys.argv[2] != "-" else None
    out_path = sys.argv[3] if len(sys.argv) > 3 else None

    inst = load_instance(f"{prefix}-net.json", f"{prefix}-tm.json", f"{prefix}-scenario.json")
    graph = DirectedGraph.build(inst)
    cache = AtomCache(graph, inst.scenario.interventions)
    evaluator = Evaluator(inst, graph, cache)

    sol = (
        read_solution(srpaths, inst)
        if srpaths
        else __import__("tasr.model.solution", fromlist=["Solution"]).Solution.empty(
            inst.n_demands, inst.n_slots
        )
    )

    sat = evaluator.saturations(sol)  # (m, n_slots) float64
    spec = CompareSpec()
    rank = spec.rank_int(sat)  # (m, n_slots) int64

    lines = []
    m, ts = sat.shape
    lines.append(f"HDR m={m} n_slots={ts} n_demands={inst.n_demands} n_nodes={graph.n_nodes}")
    for t in range(ts):
        for a in range(m):
            lines.append(f"A {t} {a} {sat[a, t]:.17g}")
            lines.append(f"M {t} {a} {int(rank[a, t])}")
    v = np.sort(sat.ravel())[::-1]
    vq = spec.rank_int(v)
    for i in range(len(v)):
        lines.append(f"V {v[i]:.17g}")
        lines.append(f"Q {int(vq[i])}")

    text = "\n".join(lines) + "\n"
    if out_path:
        with open(out_path, "w", encoding="utf-8") as fh:
            fh.write(text)
    else:
        sys.stdout.write(text)


if __name__ == "__main__":
    main()
