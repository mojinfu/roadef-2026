#!/usr/bin/env python3
"""Build 0910record.md from a finished setB sweep.

Reads solution/runs_setB_full/{hot,pingpong}/setB-XX.{json,log}, compares the two
arms per instance on the official yardstick (the sorted load vector, truncated to
6 decimals, compared lexicographically) and appends the top-10 hot arcs of the
winning arm.  Reporting rule (CLAUDE.md): every instance gets a tie-layer count
and, when the vectors differ, the layer the first gap falls on and its size --
an unqualified "first bit tied N/12" is not a result.

Usage:  python scripts/setB_record.py [--out 0910record.md] [--sweep runs_setB_full]
The Go helpers are built into _pyref/ (gitignored) if missing.
"""
import argparse
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SOL = os.path.dirname(HERE)          # .../solution
ROOT = os.path.dirname(SOL)          # repo root
PYREF = os.path.join(SOL, "_pyref")

ARMS = ["hot", "pingpong"]


def build_tool(name):
    exe = os.path.join(PYREF, name + ".exe")
    if not os.path.exists(exe):
        os.makedirs(PYREF, exist_ok=True)
        subprocess.run(["go", "build", "-o", exe, "./cmd/" + name],
                       cwd=SOL, check=True)
    return exe


def parse_log(path):
    """Pull the headline numbers out of one solver log.

    A run that overruns stopAt inside a Gurobi solve is cut by the watchdog,
    which writes the incumbent and exits *without* the summary block -- so a
    log can have a perfectly good solution and no RESULT line.  Those runs are
    still reported: the counters the round loop printed are recovered here and
    the vector is taken from the solution file (main() fills firstbit in).
    """
    txt = open(path, encoding="utf-8", errors="replace").read()
    txt = "\n".join(l for l in txt.splitlines() if "LicenseID" not in l)
    n = {}
    m = re.search(r"^RESULT (\S+) accepted=(\d+) firstbit=(\d+) total_cost=(\d+) budget_ok=(\w+)",
                  txt, re.M)
    if m:
        n["inst"], n["accepted"] = m.group(1), int(m.group(2))
        n["firstbit"], n["total_cost"], n["budget_ok"] = int(m.group(3)), int(m.group(4)), m.group(5)
        m = re.search(r"rejected=(\d+).*?elapsed=([\d.]+)s", txt, re.S)
        if m:
            n["rejected"] = int(m.group(1))
            n["elapsed"] = float(m.group(2))
            n["rounds"] = n["accepted"] + n["rejected"]
    else:
        if not re.search(r"^solve: still running past stopAt", txt, re.M):
            return None          # not a watchdog cut: the log carries nothing
        acc = [l for l in txt.splitlines() if re.search(r"round \d+ ACCEPT", l)]
        rej = [l for l in txt.splitlines() if re.search(r"round \d+ REJECT", l)]
        last = re.findall(r"total_cost=(\d+)", txt)
        n["accepted"] = len(acc)
        n["rejected"] = len(rej)
        n["rounds"] = len(acc) + len(rej)
        n["total_cost"] = int(last[-1]) if last else -1
        n["budget_ok"] = "watchdog 截断，未复核"
        n["watchdog"] = True

    # The summary block indents every line ("  schedule=..."), so the anchor has
    # to be \s*, not ^ alone.
    m = re.search(r"^\s*schedule=(\S+).*?scale=(\d+) epoch_restarts=(\d+)", txt, re.M)
    if m:
        n["schedule"], n["scale"], n["restarts"] = m.group(1), int(m.group(2)), int(m.group(3))

    m = re.search(r"freeze: pre=(\d+) soft=(\d+) skip=(\d+) conf_seeds=(\d+)", txt)
    if m:
        n["pre"], n["soft"], n["skip"], n["conf_seeds"] = (int(x) for x in m.groups())

    m = re.search(r"epoch exhausted at scale cap after (\d+) rounds", txt)
    n["stopped"] = "scale-cap epoch exhaustion" if m else "round/wall cap"
    if re.search(r"^solve: still running past stopAt; writing best solution", txt, re.M):
        n["stopped"] = "看门狗截断（stopAt 到点时正卡在 Gurobi 里）"

    m = re.search(r"^\s*first-bit floor rank (\d+) vs achieved (\d+)", txt, re.M)
    if m:
        n["floor"], n["achieved"] = int(m.group(1)), int(m.group(2))
    m = re.search(r"first bit PROVEN optimal: rank (\d+)", txt)
    n["proven"] = bool(m)

    m = re.search(r"^\s*atoms cap=(\d+) hits=(\d+) misses=(\d+) evicted=(\d+)", txt, re.M)
    if m:
        n["acap"], n["ahits"], n["amiss"], n["aevict"] = (int(x) for x in m.groups())

    m = re.search(r"^instance=(\S+) m=(\d+) T=(\d+) demands=(\d+)", txt, re.M)
    if m:
        n["m"], n["T"], n["demands"] = int(m.group(2)), int(m.group(3)), int(m.group(4))
    return n


def load_vector(validate, inst, sol, tmp):
    """The solver's own sorted load vector, with each entry's rank.

    Ranks come from the dump's Q rows (eval.RankInt), not from re-deriving the
    trunc6 encoding here: the first bit is compared as a rank everywhere else,
    and -dump is already the official-format path.
    """
    subprocess.run([validate, "-prefix", os.path.join(ROOT, "setB", inst),
                    "-srpaths", sol, "-dump", tmp],
                   check=True, stdout=subprocess.DEVNULL)
    vec, ranks = [], []
    for l in open(tmp):
        if l.startswith("V "):
            vec.append(float(l[2:]))
        elif l.startswith("Q "):
            ranks.append(int(l[2:]))
    return vec, ranks


def top_arcs(hotarcs, inst, sol, n=10):
    out = subprocess.run([hotarcs, "-prefix", os.path.join(ROOT, "setB", inst),
                          "-sol", sol, "-top", str(n)],
                         check=True, capture_output=True, text=True).stdout
    rows, mlu = [], None
    for line in out.splitlines():
        m = re.match(r"MLU \(1st component, trunc6\) = ([\d.]+)", line)
        if m:
            mlu = m.group(1)
        m = re.match(r"\s+(\d+)\s+(\d+)\s+(\d+)\s+([\d.]+)\s+([\d.]+)\s+(\S+ -> \S+)", line)
        if m:
            rows.append(m.groups())
    return mlu, rows


def compare(na, nb):
    """Tie-layer count and the first layer two arms disagree on.

    Decisions are made on the *rank* vectors (trunc6, the official yardstick):
    two raw saturations that differ below the 6th decimal are the same layer,
    so comparing the floats would invent a gap the checker does not see.  The
    values returned for display are the raw ones, which is what a reader needs
    to judge the size of the gap.
    """
    ra, va = na["ranks"], na["vec"]
    rb, vb = nb["ranks"], nb["vec"]
    n = 0
    for x, y in zip(ra, rb):
        if x != y:
            break
        n += 1
    if n == len(ra) == len(rb):
        return n, None, None, None
    return n, n + 1, va[n], vb[n]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.join(SOL, "0910record.md"))
    ap.add_argument("--sweep", default=os.path.join(SOL, "runs_setB_full"))
    ap.add_argument("--top", type=int, default=10)
    args = ap.parse_args()

    validate, hotarcs = build_tool("validate"), build_tool("hotarcs")
    tmp = os.path.join(PYREF, "_vec.tmp")
    commit = subprocess.run(["git", "rev-parse", "--short", "HEAD"], cwd=ROOT,
                            capture_output=True, text=True).stdout.strip()
    dirty = subprocess.run(["git", "status", "--porcelain"], cwd=ROOT,
                           capture_output=True, text=True).stdout.strip()
    if dirty:
        commit += "-dirty（工作区有未提交改动，跑分用的就是这份）"

    insts, rows, bodies = [], [], []
    for i in range(1, 13):
        inst = "setB-%02d" % i
        per = {}
        for arm in ARMS:
            d = os.path.join(args.sweep, arm)
            log, sol = os.path.join(d, inst + ".log"), os.path.join(d, inst + ".json")
            if not (os.path.exists(log) and os.path.exists(sol)):
                per[arm] = None
                continue
            n = parse_log(log)
            if n is None:
                per[arm] = None
                continue
            n["vec"], n["ranks"] = load_vector(validate, inst, sol, tmp)
            if "firstbit" not in n:
                # Watchdog cut: the log has no RESULT, so the first bit comes
                # from the solution the watchdog actually wrote.
                n["firstbit"] = n["ranks"][0]
            n["sol"] = sol
            per[arm] = n
        if not any(per.values()):
            continue
        insts.append(inst)
        rows.append((inst, per))
        bodies.append((inst, per))

    lines = []
    A = lines.append
    A("# setB 全量跑分记录（2026-09-11）")
    A("")
    A("- 求解器 commit: `%s`（分支 `SOLUTION`）" % commit)
    A("- 预算: 每实例墙钟上限 **600 s**，`-rounds 100000` 让墙钟成为唯一约束；串行执行")
    A("  （Gurobi 单 license，并行跑不是同等预算的比较）")
    A("- 两臂:")
    A("  - `hot` = `-schedule hot`，即默认基线（无 epoch、无 scale escalator）")
    A("  - `pingpong` = `-schedule pingpong` + `-epoch-top-n 12`，"
      "含 `cover²+0.1` 置信度与 early 轮不进闸门")
    A("- 对比口径: 官方尺子 —— 负载向量降序、每位截断 6 位小数、逐项字典序。")
    A("  **打平层数** = 两向量完全相同的前缀长度；一旦不同，胜负由**首分差层**唯一决定，"
      "更靠后的层不再影响结论。")
    A("- 原始数据: `solution/runs_setB_full/{hot,pingpong}/setB-XX.{json,log}`")
    A("")
    A("## 总览")
    A("")
    A("| 实例 | hot 首位 | pp 首位 | 打平层数 | 首分差层 | 该层 hot → pp | lex 胜者 | hot 轮 | pp 轮 |")
    A("|---|---|---|---|---|---|---|---|---|")

    wins = {"hot": 0, "pingpong": 0, "tie": 0}
    for inst, per in rows:
        h, p = per["hot"], per["pingpong"]
        if h and p:
            tie, layer, hv, pv = compare(h, p)
            if layer is None:
                winner, gap = "tie", "—"
            else:
                winner = "hot" if hv < pv else "pingpong"
                wins[winner] += 1
                gap = "%s → %s (%+.2e, %+.3f%%)" % (
                    ("%.6f" % hv).rstrip("0"), ("%.6f" % pv).rstrip("0"),
                    pv - hv, (pv - hv) / hv * 100)
                layer = str(layer)
            mark = lambda n: ("%s ⚠" % n["rounds"]) if n.get("watchdog") else str(n["rounds"])
            A("| %s | %s | %s | %d | %s | %s | **%s** | %s | %s |" % (
                inst, h["firstbit"], p["firstbit"], tie, layer or "—", gap, winner,
                mark(h), mark(p)))
        else:
            one = h or p
            arm = "hot" if h else "pingpong"
            A("| %s | %s | %s | — | — | — | %s（单臂） | %s | %s |" % (
                inst, one["firstbit"] if h else "—",
                one["firstbit"] if p else "—", arm, "—", "—"))
    A("")
    A("**lex 胜负计数: hot %d / pingpong %d / 打平 %d**（共 %d 个实例）"
      % (wins["hot"], wins["pingpong"], wins["tie"], len(rows)))
    A("")
    A("表里轮数末尾的 ⚠ = 该臂被看门狗截断（见下节），那一行的成绩不是 600 s 搜索的产物。")
    A("")

    A("## 已知限制：大实例（setB-10/11/12）没跑满")
    A("")
    cuts = [(inst, arm, per[arm]) for inst, per in rows for arm in ARMS
            if per[arm] and per[arm].get("watchdog")]
    A("%d/%d 个臂没有正常收尾：%s。它们只有个位数到十几轮就撞上 600 s 墙钟，"
      "成绩基本等于 presolve incumbent + 头几轮，**不能当搜索质量读**。"
      % (len(cuts), 2 * len(rows),
         "、".join("%s %s（%d 轮）" % (i, a, n["rounds"]) for i, a, n in cuts)))
    A("")
    A("原因不在 Gurobi 求解时间（`gurobi=0.82s` 量级），而在 **ECMP atom 缓存在大实例上打不中**。"
      "缓存的 key 是 `(u, v, slot)`、u/v 是任意节点，一条 entry 是 `m` 个 float64，"
      "而每轮 `rerouteAll` 要把整个 incumbent 的路由重问一遍：")
    A("")
    A("| 实例 | m（弧） | demands | 每 entry | 该实例 cap（=800MB/(8m)） | 命中率 |")
    A("|---|---|---|---|---|---|")
    for inst, per in rows:
        n = per["hot"] or per["pingpong"]
        if "m" not in n:
            continue
        hit = "—（无收尾行）"
        for arm in ARMS:
            a = per[arm]
            if a and "acap" in a:
                tot = a["ahits"] + a["amiss"]
                hit = "%.1f%%" % (100.0 * a["ahits"] / tot) if tot else "—"
                break
        A("| %s | %d | %d | %.1f KB | %s | %s |"
          % (inst, n["m"], n["demands"], 8 * n["m"] / 1024.0,
             ("%d" % n["acap"]) if "acap" in n else
             ("%d" % (800 * (1 << 20) // (8 * n["m"]))), hit))
    A("")
    A("- setB-01..09 + setB-10 + pingpong setB-12 都正常收尾，命中率 32%–98%（见上表），"
      "跑满 600 s / 上千轮，结果可用。")
    A("- 真正卡住的是 **demands 数**，不只是 m：setB-10/11/12 各有 ~15000 demands"
      "（其余 ≤7600），一轮要重问 15000×12 条路由；setB-11 又同时是 m=5036 的最大实例，"
      "一条 entry 40 KB，cap 只能给到约 2 万条。两者叠加 → 一轮约 2–3 分钟。")
    A("  实测 setB-11 的命中率掉到约 9%（cap 20821，hits 60622 / misses 631286），"
      "每轮几乎全量重算，所以 600 s 只走完 3（hot）/ 7（pingpong）轮就被看门狗截断。")
    A("- 这是**内存上限与命中率的取舍**，不是 bug：不加 cap 就是 600 s 内涨到两位数 GB 被 OOM kill"
      "（实测无限 cap 时 190 s 就持有 1973 MB 的 atom 向量 = 97% 活堆）。")
    A("- 注意 hot setB-12 虽然被截断（15 轮），首位 643805 与 pingpong 的 643805 打平前 12 层，"
      "所以那一行截断没有改变结论；setB-11 两臂则是真的没搜。")
    A("- 后续要动的话，方向是让 atom 不按 `(u,v,slot)` 而是按「最短路的 tight 弧集合」共享，"
      "或对大实例改用更省内存的段路由缓存；这是改求解器本体，本表没有做。")
    A("")

    A("## 逐实例明细")
    A("")
    for inst, per in bodies:
        A("### %s" % inst)
        A("")
        for arm in ARMS:
            n = per[arm]
            if not n:
                A("- **%s**: 无数据" % arm)
                continue
            elapsed = "%.1f s" % n["elapsed"] if "elapsed" in n else "—（看门狗未打印收尾）"
            A("- **%s**: 首位 `%s`，total_cost %d，budget_ok=%s，轮 %d"
              "（acc %d / rej %d），用时 %s，停因 %s"
              % (arm, n["firstbit"], n["total_cost"], n["budget_ok"],
                 n.get("rounds", -1), n["accepted"], n.get("rejected", -1),
                 elapsed, n["stopped"]))
            if n.get("watchdog"):
                A("  - ⚠ 日志无 RESULT：这一轮是看门狗在 stopAt 之后写出 incumbent 就退出，"
                  "没走正常收尾。轮数/接受数是按日志行回数的，首位取自它写出的解文件"
                  "（与同臂其它实例口径不同）")
            if "scale" in n:
                A("  - schedule=%s scale=%d epoch_restarts=%d" % (n["schedule"], n["scale"], n["restarts"]))
            if "soft" in n:
                A("  - 冻结: pre=%d soft=%d skip=%d conf_seeds=%d" % (n["pre"], n["soft"], n["skip"], n["conf_seeds"]))
            if "floor" in n:
                A("  - presolve 地板 rank %d，实际 %d（%.2f×）"
                  % (n["floor"], n["achieved"], n["achieved"] / n["floor"]))
            if n.get("proven"):
                A("  - 首位 **已证明最优**（= 结构地板，不是时间用完停下的）")
            if "acap" in n:
                tot = n["ahits"] + n["amiss"]
                A("  - atom 缓存 cap=%d 命中率 %.1f%%，逐出 %d"
                  % (n["acap"], 100.0 * n["ahits"] / tot if tot else 0, n["aevict"]))
        A("")
        h, p = per["hot"], per["pingpong"]
        if h and p:
            tie, layer, hv, pv = compare(h, p)
            win = "tie" if layer is None else ("hot" if hv < pv else "pingpong")
        else:
            win = "hot" if h else "pingpong"
        pick = per.get("hot") if win != "pingpong" else per["pingpong"]
        if not pick:
            A("")
            continue
        mlu, arcs = top_arcs(hotarcs, inst, pick["sol"], args.top)
        A("**top %d 热 arc（%s 臂）**，一位 = 最大负载弧；worst-slot 是该弧负载最重的时隙"
          % (args.top, win))
        A("")
        A("| rank | arc | worst-slot | saturation | mean | from → to |")
        A("|---|---|---|---|---|---|")
        for r in arcs:
            A("| %s | %s | %s | %s | %s | %s |" % r)
        A("")

    with open(args.out, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    print("wrote %s (%d instances)" % (args.out, len(rows)))


if __name__ == "__main__":
    sys.exit(main())
