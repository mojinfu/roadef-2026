#!/usr/bin/env python3
"""Tabulate the two-arm global-relief-net A/B.

Reads  <root>/G0/summary.tsv   (-cand-global-k 0,  v1 candidate layer)
       <root>/G12/summary.tsv  (-cand-global-k 12, net on)
and writes <root>/gk_ab.tsv plus a markdown table on stdout.

Reporting rule (CLAUDE.md): a bare layer-1 comparison is not an acceptable
result.  Every instance gets its tie-layer count and, when the vector does not
fully tie, the layer at which the first gap falls and the two values there.

The net is *additive* by construction (see run_gk_ab.sh), so a G12 regression
would be a real effect of the added nodes, not a reshuffled pool.
"""

import csv
import os
import sys

ARMS = {"G0": "-cand-global-k 0 (no net)", "G12": "-cand-global-k 12 (net on)"}
ORDER = ["G0", "G12"]


def load(path):
    rows = {}
    if not os.path.exists(path):
        return rows
    with open(path, newline="", encoding="utf-8") as f:
        for r in csv.DictReader(f, delimiter="\t"):
            # An NA row is a failed sample, not a result.
            if r.get("firstbit") in (None, "", "NA"):
                continue
            rows[r["instance"]] = r
    return rows


def num(row, key, default=None):
    v = row.get(key, "")
    if v in ("", "NA", None):
        return default
    try:
        return int(v)
    except ValueError:
        try:
            return float(v)
        except ValueError:
            return default


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "_pyref/gk_ab"
    arms = {k: load(os.path.join(root, k, "summary.tsv")) for k in ARMS}
    out = os.path.join(root, "gk_ab.tsv")

    common = sorted(set(arms["G0"]) & set(arms["G12"]),
                    key=lambda s: int(s.split("-")[1]))

    lines = []
    lines.append("### 全局 relief 兜底 A/B（G0 = 无兜底 / G12 = 兜底 12 节点）")
    lines.append("")
    lines.append("共同覆盖实例数：**%d**（G0 有 %d，G12 有 %d）"
                 % (len(common), len(arms["G0"]), len(arms["G12"])))
    lines.append("")
    lines.append("| instance | G0 firstbit | G12 firstbit | 相对变化 | G0 tie 层 | G12 tie 层 "
                 "| G0 首gap层 | G12 首gap层 | G12 gap 处 (ours vs ref) | G0 secs | G12 secs |")
    lines.append("|---|---|---|---|---|---|---|---|---|---|---|")

    wins, losses, ties = [], [], []
    for inst in common:
        a, b = arms["G0"][inst], arms["G12"][inst]
        fa, fb = num(a, "firstbit"), num(b, "firstbit")
        ta, tb = num(a, "tie_layers", 0), num(b, "tie_layers", 0)
        ga, gb = num(a, "first_gap_layer", 0), num(b, "first_gap_layer", 0)
        oa, ra = a.get("ours", "NA"), a.get("ref", "NA")
        ob, rb = b.get("ours", "NA"), b.get("ref", "NA")
        sa, sb = num(a, "secs", 0.0), num(b, "secs", 0.0)

        delta = ""
        if fa and fb is not None:
            if fa == fb:
                delta = "0"
            else:
                delta = "%+.1f%%" % (100.0 * (fb - fa) / fa)

        # Layer-1 parity with the sprint reference is the useful annotation on
        # the gap column: at parity there is no gap to quote.
        gap = "-" if (gb == 1 and ob == rb) else "%s vs %s" % (ob, rb)

        lines.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %.1f | %.1f |" % (
            inst, fa, fb, delta, ta, tb, ga, gb, gap, sa, sb))

        if fa is None or fb is None:
            continue
        if fb < fa:
            wins.append((inst, fa, fb))
        elif fb > fa:
            losses.append((inst, fa, fb))
        else:
            ties.append((inst, ta, tb))

    lines.append("")
    lines.append("### 量化结论")
    lines.append("")
    lines.append("- **G12 首分严格优于 G0**：%d/%d 个实例：%s"
                 % (len(wins), len(common),
                    "、".join("%s %d→%d（%+.1f%%）" % (i, a, b, 100.0 * (b - a) / a)
                              for i, a, b in wins) or "（无）"))
    lines.append("- **G12 首分等于 G0**：%d/%d 个实例：%s"
                 % (len(ties), len(common),
                    "、".join("%s（tie 层 %s→%s）" % t for t in ties) or "（无）"))
    lines.append("- **G12 首分劣于 G0**：%d/%d 个实例：%s"
                 % (len(losses), len(common),
                    "、".join("%s %d→%d（%+.1f%%）" % (i, a, b, 100.0 * (b - a) / a)
                              for i, a, b in losses) or "（无）"))
    # Tie-layer-only wins: same first bit, more layers matched -- strictly better
    # under the lexicographic objective even though layer 1 looks flat.
    tl = [(i, ta, tb) for i, ta, tb in ties if tb > ta]
    if tl:
        lines.append("- 打平实例中 tie 层增加的：%s"
                     % "、".join("%s %d→%d" % t for t in tl))
    la = [(i, ta, tb) for i, ta, tb in ties if tb < ta]
    if la:
        lines.append("- 打平实例中 tie 层**减少**的：%s"
                     % "、".join("%s %d→%d" % t for t in la))

    sa = sum(num(arms["G0"][i], "secs", 0.0) for i in common)
    sb = sum(num(arms["G12"][i], "secs", 0.0) for i in common)
    if common:
        lines.append("- **墙钟**：G0 合计 %.1fs，G12 合计 %.1fs（%+.1f%%）"
                     % (sa, sb, 100.0 * (sb - sa) / sa if sa else 0.0))

    text = "\n".join(lines)
    with open(out, "w", encoding="utf-8") as f:
        f.write(text + "\n")
    print(text)
    print("\nwrote", out)


if __name__ == "__main__":
    main()
