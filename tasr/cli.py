"""Command-line entry point.

Usage:
    python -m tasr.cli --net <net.json> --tm <tm.json> --scenario <scenario.json> --out <srpaths.json>
    python -m tasr.cli --instance setA/setA-01 --out out/setA-01-srpaths.json
"""
from __future__ import annotations

import argparse
import time

import numpy as np

from .algos import baseline_solution
from .ecmp import AtomCache
from .eval import CompareSpec, Evaluator, saturations_vector
from .io import load_instance, write_solution
from .model import DirectedGraph


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="tasr", description="T-ASR solver (ROADEF 2026)")
    p.add_argument("--net", help="network JSON file")
    p.add_argument("--tm", help="traffic matrix JSON file")
    p.add_argument("--scenario", help="scenario JSON file")
    p.add_argument("--out", required=True, help="output srpaths JSON file")
    p.add_argument(
        "--instance",
        help="instance name prefix; expands to <prefix>-net.json, -tm.json, -scenario.json",
    )
    return p


def resolve_args(args: argparse.Namespace):
    if args.instance:
        prefix = args.instance
        net = f"{prefix}-net.json"
        tm = f"{prefix}-tm.json"
        sc = f"{prefix}-scenario.json"
        return net, tm, sc
    if not (args.net and args.tm and args.scenario):
        raise SystemExit("either --instance or --net/--tm/--scenario must be given")
    return args.net, args.tm, args.scenario


def main(argv=None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    net, tm, sc = resolve_args(args)
    t0 = time.perf_counter()

    inst = load_instance(net, tm, sc)
    graph = DirectedGraph.build(inst)
    cache = AtomCache(graph, inst.scenario.interventions)
    evaluator = Evaluator(inst, graph, cache)

    sol = baseline_solution(inst)
    sat = evaluator.saturations(sol)
    vec = saturations_vector(sat)
    spec = CompareSpec()

    write_solution(args.out, sol, inst)

    print(f"instance : {inst.name}")
    print(f"nodes    : {inst.n_nodes}   arcs: {inst.n_arcs}   slots: {inst.n_slots}   demands: {inst.n_demands}")
    print(f"max_seg  : {inst.scenario.max_segments}")
    print(f"mlu      : {sat.max():.9f}")
    print(f"top loads(trunc6, desc):")
    print("  " + ", ".join(f"{x:.6f}" for x in spec.quantize(vec)[:15]))
    print(f"budget   : 0 (baseline, nothing changes between slots)")
    print(f"wrote    : {args.out}")
    print(f"elapsed  : {time.perf_counter() - t0:.2f}s")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
