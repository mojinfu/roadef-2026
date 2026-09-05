"""Cross-validate our Python ECMP/evaluator against the official C++ checker.

The official checker is a Linux static binary built from ``checker/src`` (see
``scripts/docker_checker.Dockerfile`` or a WSL Ubuntu build).  It reads the four
instance JSON files and prints one JSON document on stdout:

    {valid, total_cost, total_srpaths, total_segments,
     objectives: [{t, mlu, inv_load_cost, jain_index}],
     saturations: [{t, from, to, sat}]}          # sat desc-sorted, ids = net file node ids

This script runs the checker (through WSL when on Windows) on:

  1. an *empty* solution  -> isolates the ECMP core (direct shortest paths)
  2. our ``runs/*-srpaths.json`` solutions -> full pipeline incl. waypoints

and compares against :func:`Evaluator.saturations` value-by-value per slot
(tolerance ``_TOL``, chosen well below the 1e-6 truncation of the official
score), plus the per-slot max load (mlu).
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

import numpy as np

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO))

from tasr.ecmp import AtomCache  # noqa: E402
from tasr.eval.evaluator import Evaluator, RoutingInfeasibleError  # noqa: E402
from tasr.io import load_instance, read_solution, write_solution  # noqa: E402
from tasr.model import DirectedGraph, Solution  # noqa: E402

_TOL = 1e-6  # max abs diff tolerated per saturation value (<< 1e-6 trunc)

INSTANCES = [f"setA-{i:02d}" for i in range(1, 21)]


def wsl_path(p: Path) -> str:
    """Convert a Windows path to the /mnt/<drive>/... form WSL understands."""
    p = Path(p).resolve()
    drive, rest = p.drive, str(p)[len(p.drive):]
    return f"/mnt/{drive[0].lower()}{rest.replace(os.sep, '/')}"


def run_checker(checker: str, inst_dir: Path, name: str, srpaths: Path) -> dict:
    """Invoke the checker binary (directly or via WSL) and return parsed JSON."""
    net, tm, scen = (inst_dir / f"{name}-net.json", inst_dir / f"{name}-tm.json",
                     inst_dir / f"{name}-scenario.json")
    args = [checker, "--net", str(net), "--tm", str(tm),
            "--scenario", str(scen), "--srpaths", str(srpaths)]
    if sys.platform.startswith("win"):
        # checker runs inside WSL: absolute paths must be Linux-style
        checker_lx = checker if checker.startswith("/") else wsl_path(Path(checker))
        args = ["wsl", "-d", "Ubuntu-24.04", "-u", "root", "--", checker_lx,
                "--net", wsl_path(net), "--tm", wsl_path(tm),
                "--scenario", wsl_path(scen), "--srpaths", wsl_path(srpaths)]
    proc = subprocess.run(args, capture_output=True, text=True, timeout=120)
    if proc.returncode != 0:
        raise RuntimeError(f"checker exited {proc.returncode}:\n{proc.stderr[-2000:]}")
    out = proc.stdout
    start, end = out.find("{"), out.rfind("}")
    if start < 0 or end < start:
        raise RuntimeError(f"no JSON in checker stdout:\n{out[-2000:]}")
    return json.loads(out[start:end + 1])


def sorted_sat_values(per_arc_slot: np.ndarray) -> dict:
    """Return {t: sorted-desc list of saturations} from our (arcs, slots) matrix."""
    return {t: np.sort(per_arc_slot[:, t])[::-1].tolist() for t in range(per_arc_slot.shape[1])}


def checker_sat_by_slot(result: dict) -> dict:
    """Rebuild {t: sorted-desc sat list} from the checker's flattened saturations."""
    out = {t: [] for t in range(len(result["objectives"]))}
    for item in result["saturations"]:
        out[item["t"]].append(float(item["sat"]))
    for t in out:
        out[t].sort(reverse=True)
    return out


def compare_instance(checker: str, inst_dir: Path, name: str, sol_path: Path) -> dict:
    """Run the checker on one solution and compare to our evaluator."""
    inst = load_instance(f"{inst_dir}/{name}-net.json", f"{inst_dir}/{name}-tm.json",
                         f"{inst_dir}/{name}-scenario.json")
    g = DirectedGraph.build(inst)
    cache = AtomCache(g, inst.scenario.interventions)
    ev = Evaluator(inst, g, cache)
    sol = read_solution(sol_path, inst)

    res = run_checker(checker, inst_dir, name, sol_path)
    if not res.get("valid"):
        return {"name": name, "checker_valid": False, "error": "checker says INVALID"}

    ours = ev.saturations(sol)
    checker_sats = checker_sat_by_slot(res)
    checker_mlu = {o["t"]: float(o["mlu"]) for o in res["objectives"]}

    per_slot = {}
    max_abs = 0.0
    n_slots = ours.shape[1]
    for t in range(n_slots):
        ov = sorted(ours[:, t].tolist(), reverse=True)
        cv = checker_sats.get(t, [])
        if len(ov) != len(cv):
            per_slot[t] = {"fatal": f"arc count mismatch ours={len(ov)} checker={len(cv)}"}
            continue
        dv = [abs(a - b) for a, b in zip(ov, cv)]
        dmax = max(dv) if dv else 0.0
        max_abs = max(max_abs, dmax)
        per_slot[t] = {
            "n_arcs": len(ov),
            "max_abs_diff": dmax,
            "ours_mlu": float(max(ov)),
            "checker_mlu": checker_mlu.get(t),
            "ok": dmax <= _TOL,
            "n_over_tol": sum(1 for x in dv if x > _TOL),
        }
    ok = res.get("valid", False) and all(v.get("ok", False) for v in per_slot.values())
    return {
        "name": name,
        "checker_valid": True,
        "slots": per_slot,
        "overall_max_abs_diff": max_abs,
        "ok": ok,
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--checker", default=os.environ.get("TASR_CHECKER", ""),
                    help="path to official checker binary (Linux). On Windows the binary "
                         "is reached through WSL.")
    ap.add_argument("--instances", nargs="*", default=INSTANCES)
    ap.add_argument("--seta", type=Path, default=REPO / "setA")
    ap.add_argument("--runs", type=Path, default=REPO / "runs")
    ap.add_argument("--solutions", action="store_true",
                    help="also validate runs/<name>-srpaths.json (default: baseline only)")
    args = ap.parse_args()

    if not args.checker:
        print("--checker required (or set TASR_CHECKER)", file=sys.stderr)
        return 2

    summary = []
    tmp = Path(tempfile.mkdtemp(prefix="tasr_checker_"))
    try:
        for name in args.instances:
            inst = load_instance(f"{args.seta}/{name}-net.json", f"{args.seta}/{name}-tm.json",
                                 f"{args.seta}/{name}-scenario.json")
            g = DirectedGraph.build(inst)
            cache = AtomCache(g, inst.scenario.interventions)
            ev = Evaluator(inst, g, cache)

            cases = []
            # case 1: empty baseline
            empty = Solution.empty(inst.n_demands, inst.n_slots)
            bl_path = tmp / f"{name}-empty-srpaths.json"
            write_solution(bl_path, empty, inst)
            bl_check = run_checker(args.checker, args.seta, name, bl_path)
            ours_bl = ev.saturations(empty)
            cases.append(("baseline", bl_check, ours_bl))

            # case 2: our solved file (optional)
            if args.solutions:
                sol_path = args.runs / f"{name}-srpaths.json"
                if not sol_path.exists():
                    print(f"  ! no solution for {name}, skip", file=sys.stderr)
                    continue
                sol_check = run_checker(args.checker, args.seta, name, sol_path)
                sol = read_solution(sol_path, inst)
                try:
                    ours_sol = ev.saturations(sol)
                except RoutingInfeasibleError as exc:
                    print(f"  ! {name}: our evaluator flags infeasible: {exc}", file=sys.stderr)
                    continue
                cases.append(("solved", sol_check, ours_sol))

            for label, res, ours in cases:
                if not res.get("valid"):
                    print(f"[{name}/{label}] CHECKER INVALID", flush=True)
                    continue
                checker_sats = checker_sat_by_slot(res)
                checker_mlu = {o["t"]: float(o["mlu"]) for o in res["objectives"]}
                worst, n_over = 0.0, 0
                slot_rows = []
                for t in range(ours.shape[1]):
                    ov = np.sort(ours[:, t])[::-1]
                    cv = np.array(sorted(checker_sats.get(t, []), reverse=True))
                    if len(ov) != len(cv):
                        slot_rows.append(f"t{t}: arccount {len(ov)} vs {len(cv)}")
                        n_over = 10 ** 9
                        continue
                    d = float(np.max(np.abs(ov - cv))) if len(ov) else 0.0
                    worst = max(worst, d)
                    if d > _TOL:
                        n_over += int(np.sum(np.abs(ov - cv) > _TOL))
                    tag = "ok" if d <= _TOL else "MISMATCH"
                    slot_rows.append(f"t{t}: mlu ours={float(np.max(ov)):.6f} "
                                     f"checker={checker_mlu.get(t):.6f} maxdiff={d:.2e} {tag}")
                ok = worst <= _TOL and res.get("valid", False)
                summary.append((name, label, ok, worst))
                print(f"[{name}/{label}] {'PASS' if ok else 'FAIL'}  maxdiff={worst:.2e}  n_over_tol={n_over}")
                for row in slot_rows:
                    print(f"      {row}")
    finally:
        pass
    # leave tmpdir for inspection
    print(f"\n(tmp srpaths kept at {tmp})")

    n_pass = sum(1 for *_, ok, _ in summary if ok)
    print(f"\n== {n_pass}/{len(summary)} comparisons within tol {_TOL}")
    for name, label, ok, worst in summary:
        print(f"   {'PASS' if ok else 'FAIL'}  {name}/{label}  maxdiff={worst:.2e}")
    return 0 if n_pass == len(summary) else 1


if __name__ == "__main__":
    raise SystemExit(main())
