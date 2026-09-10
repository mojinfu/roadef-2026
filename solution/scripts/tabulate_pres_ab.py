#!/usr/bin/env python3
"""Tabulate the three-arm presolve A/B (off / on,skip-floor=false / on,skip-floor=true).

Reads runs_pres_ab/<arm>/summary.tsv and writes a derived per-instance table
plus arm-level aggregates.  All comparisons are against the sprint reference
vector, so the reported quantities are the ones CLAUDE.md requires:
tie-layer count, first-gap layer, and the gap magnitude at that layer.
"""
import csv
import os
import sys

def repo_root():
    """The repo root is the ancestor of this script that holds setA/ and the
    runs_* directories (the script lives in solution/scripts/, one level down
    from the Go module, two from the repo root)."""
    here = os.path.dirname(os.path.abspath(__file__))
    for _ in range(3):
        if os.path.isdir(os.path.join(here, "setA")):
            return here
        here = os.path.dirname(here)
    raise SystemExit("cannot locate repo root (no setA/ found above scripts/)")


ROOT = repo_root()
RUNS = os.environ.get("RUNS", os.path.join(ROOT, "solution", "runs_pres_ab"))
ARMS = ["off", "on", "on_skip", "on100"]

# Arm definitions, for the report:
#   off      -presolve off
#   on       -presolve unmovable -presolve-deep 0     (uncapped class-3 pass)
#   on_skip  -presolve unmovable -presolve-deep 0     -skip-floor true
#   on100    -presolve unmovable -presolve-deep 100   (capped class-3 pass)
# Everything else identical: -model sticky -rounds 150 -wall-sec 90 -hot-k 6
# -peel 6 -budget-mode full.


def load(arm):
    path = os.path.join(RUNS, arm, "summary.tsv")
    rows = {}
    if not os.path.exists(path):
        return rows
    with open(path, newline="") as f:
        for r in csv.DictReader(f, delimiter="\t"):
            rows[r["instance"]] = r
    return rows


def num(s):
    try:
        return float(s)
    except (TypeError, ValueError):
        return None


def main():
    data = {a: load(a) for a in ARMS}
    insts = sorted(set().union(*[set(d) for d in data.values()]))
    out = os.path.join(ROOT, "experiments", "2026-09-10_02_presolve_setA_ab", "data")
    os.makedirs(out, exist_ok=True)

    # ---- per-instance comparison -------------------------------------------
    lines = [
        "instance\t" + "\t".join(
            f"{a}_accepted\t{a}_firstbit\t{a}_tie\t{a}_gaplayer\t{a}_gap_ours\t{a}_gap_ref"
            for a in ARMS
        ) + "\tfirstbit_same\ttie_delta(on-on_skip)"
    ]
    agg = {a: dict(fb_sum=0.0, tie_sum=0, n=0, gap1=0) for a in ARMS}
    for inst in insts:
        cells = []
        fbs, ties = {}, {}
        for a in ARMS:
            r = data[a].get(inst)
            if not r:
                cells += ["NA"] * 6
                continue
            fb, tie = num(r["firstbit"]), num(r["tie_layers"])
            fbs[a], ties[a] = fb, tie
            cells += [
                r["accepted"], r["firstbit"], r["tie_layers"], r["first_gap_layer"],
                r["ours"], r["ref"],
            ]
            if fb is not None:
                agg[a]["fb_sum"] += fb
                agg[a]["tie_sum"] += tie or 0
                agg[a]["n"] += 1
                if num(r["first_gap_layer"]) == 1:
                    agg[a]["gap1"] += 1
        same = "yes" if len(set(fbs.values())) == 1 and len(fbs) == len(ARMS) else "no"
        dtie = (ties.get("on", 0) - ties.get("on_skip", 0)) if len(ties) == len(ARMS) else "NA"
        lines.append(inst + "\t" + "\t".join(cells) + f"\t{same}\t{dtie}")

    # ---- arm aggregates -----------------------------------------------------
    lines.append("")
    lines.append("arm\tn\tfirstbit_sum\ttie_sum\tfirst_layer1_gaps")
    for a in ARMS:
        g = agg[a]
        lines.append(f"{a}\t{g['n']}\t{g['fb_sum']:.0f}\t{g['tie_sum']}\t{g['gap1']}")

    dst = os.path.join(out, "presolve_ab.tsv")
    with open(dst, "w", newline="") as f:
        f.write("\n".join(lines) + "\n")
    print("\n".join(lines))
    print(f"\nwrote {dst}")


if __name__ == "__main__":
    sys.exit(main())
