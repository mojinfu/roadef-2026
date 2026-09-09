"""Run the official checker (WSL) over every bench_m4 JSON and emit a TSV.

For each instance the checker prints {valid, total_cost, ...}; we also take the
max mlu across slots (= first element of our sorted-desc saturation vector).
The output columns mirror _pyref/bench_m4/summary.tsv so a side-by-side
validity check is trivial.
"""
import json
import subprocess
import sys
from pathlib import Path

WSL = ["wsl", "-d", "Ubuntu-24.04", "-u", "root"]
CHECKER = "/root/tasr-checker/checker-src/checker-v1.2.2-x86-64_linux"
SOL = Path(__file__).resolve().parents[1]  # solution/
REPO = SOL.parents[0]                      # repo root (contains setA/, sprint_results/)
INST = REPO / "setA"


def wsl_path(p: Path) -> str:
    p = Path(p).resolve()
    drive, rest = p.drive, str(p)[len(p.drive):]
    return f"/mnt/{drive[0].lower()}{rest.replace(chr(92), '/')}"


def run_checker(name: str, jsondir: Path) -> dict:
    net, tm, scen = (INST / f"{name}-net.json", INST / f"{name}-tm.json",
                     INST / f"{name}-scenario.json")
    srp = jsondir / f"{name}.json"
    args = WSL + ["--", CHECKER, "--net", wsl_path(net), "--tm", wsl_path(tm),
                  "--scenario", wsl_path(scen), "--srpaths", wsl_path(srp)]
    proc = subprocess.run(args, capture_output=True, timeout=180)
    if proc.returncode != 0:
        raise RuntimeError(f"{name}: checker exit {proc.returncode}: "
                           f"{proc.stderr[:200]!r}")
    out = proc.stdout
    start, end = out.find(b"{"), out.rfind(b"}")
    if start < 0 or end < start:
        raise RuntimeError(f"{name}: no JSON in checker stdout: {out[:200]!r}")
    return json.loads(out[start:end + 1].decode("utf-8"))


def main() -> None:
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", default="bench_m4", help="subdir under solution/_pyref holding JSONs")
    ap.add_argument("--tsv", default=None, help="TSV (relative to the json dir) whose 'firstbit' column to compare")
    ap.add_argument("instances", nargs="*", help="instance names; default all setA-01..20")
    a = ap.parse_args()
    jsondir = SOL / "_pyref" / a.dir
    names = a.instances or [f"setA-{i:02d}" for i in range(1, 21)]
    tsv_fb = {}
    if a.tsv:
        tsvp = Path(a.tsv) if Path(a.tsv).is_absolute() else jsondir / a.tsv
        for line in tsvp.read_text().splitlines():
            parts = line.split("\t")
            if len(parts) > 2:
                tsv_fb[parts[0]] = parts[2]
    print("instance\tvalid\ttotal_cost\tmax_mlu_rank\tfirstbit_tsv")
    for name in names:
        res = run_checker(name, jsondir)
        mlus = [o["mlu"] for o in res["objectives"]]
        maxmlu = max(mlus)
        fb = tsv_fb.get(name, "?")
        print(f"{name}\t{res['valid']}\t{res['total_cost']}\t{int(maxmlu*1e6+1e-6)}\t{fb}")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
