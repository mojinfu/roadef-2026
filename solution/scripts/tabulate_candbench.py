#!/usr/bin/env python3
"""Turn runs_candbench2/setA-*.txt into one metrics row per instance.

Each .txt is the stdout of `candbench -repeat N`, so the BFS and wall-clock
figures on the "hop cache on/off" lines cover N passes over the family list
while the candidate statistics cover a single pass (the first).

Reporting rule (CLAUDE.md): the interesting question is not "did the cache
help" but "by how much, on what, and is the result it produces the same one" --
so the neutral BFS/wall ratio and the recall figures are emitted side by side
rather than summarised into a single verdict.
"""

import glob
import os
import re
import sys

NUM = r"[-+]?\d*\.?\d+"


def parse(path):
    txt = open(path, encoding="utf-8").read()
    row = {"instance": os.path.basename(path)[:-4]}
    m = re.search(r"m=(\d+) T=(\d+) n=(\d+) demands=(\d+)", txt)
    if m:
        row["m"], row["T"], row["n"], row["demands"] = (int(x) for x in m.groups())
    m = re.search(r"families=(\d+)", txt)
    row["families"] = int(m.group(1)) if m else 0

    for arm in ("on", "off"):
        m = re.search(
            r"hop cache %s\s*:\s*(%s) ms (?:total|best-of-\d+).*?"
            r"BFS queries=(\d+) hits=(\d+) misses=(\d+) computed=(\d+)" % (arm, NUM),
            txt)
        if m:
            row["ms_" + arm] = float(m.group(1))
            row["q_" + arm] = int(m.group(2))
            row["hits_" + arm] = int(m.group(3))
            row["bfs_" + arm] = int(m.group(5))

    m = re.search(r"candidates\s+:\s*(\d+) total, (%s) per family \((%s) w1 \+ (%s) w2\)"
                  % (NUM, NUM, NUM), txt)
    if m:
        row["cands"] = int(m.group(1))
        row["cands_per_fam"] = float(m.group(2))
        row["w1_per_fam"] = float(m.group(3))
        row["w2_per_fam"] = float(m.group(4))

    m = re.search(r"candidate time\s+:\s*(%s) ms total, (%s) ms per family" % (NUM, NUM), txt)
    if m:
        row["ms_cand_total"] = float(m.group(1))
        row["ms_per_fam"] = float(m.group(2))

    m = re.search(r"degenerate\s+:\s*(\d+) empty", txt)
    row["empty_fams"] = int(m.group(1)) if m else 0

    m = re.search(r"recall@\d+\s+:\s*(\d+)/(\d+) = (%s)%%" % NUM, txt)
    if m:
        row["recall_hit"], row["recall_k"], row["recall_pct"] = (
            int(m.group(1)), int(m.group(2)), float(m.group(3)))

    m = re.search(r"relief capture\s+:\s*cand best / legacy best over families = (%s)" % NUM, txt)
    if m:
        row["relief_capture"] = float(m.group(1))

    row["determinism"] = "PASS" if "determinism: PASS" in txt else "FAIL"
    row["neutrality"] = "PASS" if "neutrality: PASS" in txt else "FAIL"
    return row


COLS = ["instance", "n", "families", "cands_per_fam", "w1_per_fam", "w2_per_fam",
        "ms_per_fam", "empty_fams", "recall_pct", "recall_hit", "recall_k",
        "relief_capture", "bfs_on", "bfs_off", "bfs_ratio",
        "ms_on", "ms_off", "ms_ratio", "determinism", "neutrality"]


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "runs_candbench2"
    out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(root, "cand_metrics.tsv")
    rows = []
    for p in sorted(glob.glob(os.path.join(root, "setA-*.txt"))):
        r = parse(p)
        if r.get("bfs_off"):
            r["bfs_ratio"] = r["bfs_off"] / r["bfs_on"] if r.get("bfs_on") else 0.0
        if r.get("ms_off"):
            r["ms_ratio"] = r["ms_off"] / r["ms_on"] if r.get("ms_on") else 0.0
        rows.append(r)
    rows.sort(key=lambda r: int(r["instance"].split("-")[1]))

    with open(out, "w", encoding="utf-8") as f:
        f.write("\t".join(COLS) + "\n")
        for r in rows:
            f.write("\t".join(
                ("%.3f" % r[c]) if isinstance(r.get(c), float) else str(r.get(c, ""))
                for c in COLS) + "\n")

    print("| instance | n | families | cands/fam | w1 | w2 | ms/fam | empty | recall@12 | capture | BFS on | BFS off | ratio | ms on | ms off | det | neut |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in rows:
        print("| %s | %s | %s | %.1f | %.1f | %.1f | %.2f | %s | %.1f%% | %.4f | %s | %s | %.1f× | %.0f | %.0f | %s | %s |" % (
            r["instance"], r.get("n", ""), r.get("families", ""),
            r.get("cands_per_fam", 0), r.get("w1_per_fam", 0), r.get("w2_per_fam", 0),
            r.get("ms_per_fam", 0), r.get("empty_fams", ""),
            r.get("recall_pct", 0), r.get("relief_capture", 0),
            r.get("bfs_on", ""), r.get("bfs_off", ""), r.get("bfs_ratio", 0),
            r.get("ms_on", 0), r.get("ms_off", 0),
            r.get("determinism", ""), r.get("neutrality", "")))

    if rows:
        tb_on = sum(r.get("bfs_on", 0) for r in rows)
        tb_off = sum(r.get("bfs_off", 0) for r in rows)
        tm_on = sum(r.get("ms_on", 0) for r in rows)
        tm_off = sum(r.get("ms_off", 0) for r in rows)
        tot_c = sum(r.get("cands", 0) for r in rows)
        tot_f = sum(r.get("families", 0) for r in rows)
        rh = sum(r.get("recall_hit", 0) for r in rows)
        rk = sum(r.get("recall_k", 0) for r in rows)
        caps = [r.get("relief_capture", 1.0) for r in rows]
        print()
        print("totals: families=%d candidates=%d (%.1f/family)" % (tot_f, tot_c, tot_c / tot_f))
        print("BFS: on=%d off=%d ratio=%.1f×  (saved %d)" % (tb_on, tb_off, tb_off / tb_on, tb_off - tb_on))
        print("wall: on=%.0fms off=%.0fms ratio=%.3f (>1 means the cache was faster)"
              % (tm_on, tm_off, tm_off / tm_on))
        print("recall@12: %d/%d = %.1f%%" % (rh, rk, 100.0 * rh / rk))
        print("relief capture: min=%.4f mean=%.4f  (instances below 1.0: %s)"
              % (min(caps), sum(caps) / len(caps),
                 ", ".join(r["instance"] for r in rows if r.get("relief_capture", 1) < 0.999)))
    print("\nwrote", out)


if __name__ == "__main__":
    main()
