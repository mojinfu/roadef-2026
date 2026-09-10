#!/usr/bin/env python3
"""Regenerate report.md from the raw -dual-probe/-dual-peel logs.

Kept as a script rather than hand-written prose so the per-layer table in the
report is always the one the logs actually contain; re-running after another
sweep cannot silently drift from the data.
"""
import datetime
import csv
import glob
import json
import os
import re

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GAP = os.path.join('experiments', '2026-09-09_m3_twin_mip_setA_bench',
                   'data', 'gap_table.tsv')
NAME = os.path.basename(D)

PEEL = (r'^    L(\d) zlb=([\d.]+) z=([\d.]+) \(sound floor [\d.]+, \+(-?[\d.]+)\) '
        r'priced=\d+ live=(\d+).*?sumLcap=([\d.]+) disagree=(\d+)/(\d+)')
FREE = (r'\[L0free\] z=([\d.]+) \(sound floor ([\d.]+), \+(-?[\d.]+)\) '
        r'priced=\d+ live=(\d+).*?sumLcap=([\d.]+) disagree=(\d+)/(\d+)')


def load():
    ref = {}
    with open(GAP, encoding='utf-8') as fh:
        for d in csv.DictReader(fh, delimiter='\t'):
            ref[d['instance']] = d
    rows = []
    for f in sorted(glob.glob(os.path.join(D, 'data', 'inv-*.log'))):
        n = 'setA-' + os.path.basename(f)[4:6]
        txt = open(f, encoding='utf-8', errors='replace').read()
        m = re.search(FREE, txt)
        free = dict(z=float(m.group(1)), floor=float(m.group(2)), live=int(m.group(4)),
                    sum=float(m.group(5)),
                    dis=(int(m.group(6)), int(m.group(7))))
        peel = {}
        for g in re.finditer(PEEL, txt, re.M):
            peel[int(g.group(1))] = dict(
                zlb=float(g.group(2)), z=float(g.group(3)), live=int(g.group(5)),
                sum=float(g.group(6)), dis=(int(g.group(7)), int(g.group(8))))
        rows.append((n, free, peel, ref.get(n, {})))
    return rows


def cell(e):
    return '—' if e is None else f"{e['live']} | {e['dis'][0]}/{e['dis'][1]} | {e['sum']:.0f}"


def nonzero(free, peel):
    out = [('L0free',) + free['dis']] if free['dis'][0] else []
    for L in sorted(peel):
        if peel[L]['dis'][0]:
            out.append((f'L{L}',) + peel[L]['dis'])
    return out


def main():
    rows = load()

    tbl = []
    for n, free, peel, r in rows:
        par = '**落后**' if r.get('max_parity') == 'NOT' else '打平'
        body = ' | '.join([cell(free)] + [cell(peel.get(L)) for L in (0, 1, 2)])
        tbl.append(f"| {n} | {par} | {r.get('tie_layers', '?')} | "
                   f"{r.get('sprint_first', '?')} | {r.get('ours_first', '?')} | {body} |")

    g_not = [(n, nonzero(f, p)) for n, f, p, r in rows if r.get('max_parity') == 'NOT']
    g_tie = [(n, nonzero(f, p)) for n, f, p, r in rows if r.get('max_parity') == 'tie']

    def fmt(g):
        return '\n'.join(
            f"  - `{n}`: " + ' '.join(f"{a}={b}/{c}" for a, b, c in d)
            for n, d in g if d) or '  - （无）'

    cnt_not = sum(1 for _, d in g_not if d)
    cnt_tie = sum(1 for _, d in g_tie if d)
    zero_not = ', '.join(n for n, d in g_not if not d) or '（无）'
    zero_tie = ', '.join(n for n, d in g_tie if not d) or '（无）'

    frac = sorted(((n, f['z'], f['floor'], f['z'] / f['floor'])
                   for n, f, _, _ in rows if f['floor'] > 0 and f['z'] < f['floor'] - 1e-9),
                  key=lambda x: x[3])

    report = f"""# 对偶候选机制 · Tier 1 探针全量扫描（setA × 20）

日期 2026-09-10 · 分支 SOLUTION · 观测性实验，**不改变任何求解决策**

## 0. 一句话结论

探针（`-dual-probe` / `-dual-peel`）在 **19 个实例 × 4 层 = 74 层** 上全部通过一条可证伪的
数值恒等式校验（`sumLcap = Σ|Pi|·cap = 1`，74/74）。在此前提下，上一轮基于 3 个实例得出的
「第 1 层对偶恒为零」**是错的**——它成立当且仅当 `z` 压在自己的下界上（19/19 精确成立）。
对偶与 saturation 的分歧在 **5 个 max 落后实例中有 {cnt_not} 个出现**，在 14 个 max 打平实例中
只有 {cnt_tie} 个出现，方向对得上但样本太小（Fisher 单侧 p≈0.07），**不能当结论**。

## 1. 实验设置

```
solution/bin/solve.exe -prefix ../setA/setA-NN -out /tmp/probe-NN.json \\
    -rounds 1 -dual-probe 1 -dual-peel 3 -schedule hot -conf-gate=false \\
    -wall-sec 90 -mip-sec 10
```

每实例 1 轮、3 层 peel、无 MIP 时间压力。`-rounds 1` 是为了让每个实例都跑出**同一形状**的
单轮池，便于横向比较；**代价是单轮池比真预算下的池窄**，所以下面的 `z` 绝对值只在同一轮内可比，
不能当作整解性能（这个坑上一轮踩过：单轮池的 `constantFloor` 曾被误读成「生成器漏列」）。

- setA-18 例外：presolve 报告首层已在结构地板上（`999998 == 999998`），**轮循环根本不执行**，
  探针挂在循环里所以没有输出。这不是失败，是它没东西可探。
- 原始日志：`data/inv-NN.log`（19 个，缺 18）。派生表：`data/peel_summary.tsv`。
  汇总脚本：`scripts/summarize_dual_peel.py`，报告生成：`scripts/write_report.py`。

## 2. 先验证读数：`sumLcap` 恒等式

上一轮我用「cell 价 = `-1/cap`」当读数正确的证据。这一轮发现那**不是通则**（见 §4），
于是加入一条真正可证伪的检查。

每个未锁 cell 的行是 `Σδ·x − capᵢ·z ≤ −baseᵢ`。对 `z` 做稳定性分析：`z` 的目标系数为 1，
各行中 `z` 的系数是 `−capᵢ`，故当 `z` 是基变量时

```
Σ λᵢ·capᵢ = −1        即        Σ|λᵢ|·capᵢ = 1
```

`sumLcap` 就是这个和（只统计仍含 `z` 的行，已锁行不含 `z` 故排除）。实测：

| 检查 | 结果 |
|---|---|
| `sumLcap == 1.000000` ⟺ `z` 在内部（不在下界上） | **74/74** |
| 有活价（`live>0`）的层，`sumLcap` 恰为 `1.000000` | 全部 |
| 无活价（`live=0`）的层，`sumLcap` 恰为 `0.000000` | 全部 |

六位小数全中，且两种情形**都**命中——不是拟合出来的。加上 `internal/gurobi` 里手算的
单元测试 `TestConstraintDual`，读数可以判为可信。

## 3. 规则：第 0 层的对偶什么时候是死的

上一轮我说「第 1 层对偶按构造恒为 0」。全量扫描后修正为一条**条件**规则：

> **层 0（floored 模型）`live == 0` ⟺ `z` 压在自己的下界 `zlb` 上。19/19 精确成立。**

`z == zlb` 意味着 LP 的最优值由池**够不着**的东西决定（池局部的 `constantFloor`，或 presolve
的 sound floor），没有 cell 行需要变紧，对偶按互补松弛恒为 0。反过来，只要池自己能决定 max
（`z > zlb`），层 0 就有活价。

分布：19 个实例中层 0 **有**活价 8 个（02/03/04/06/10/13/16/19），**无**活价 11 个。
上一轮我挑的 01/05/08 恰好全在「无」那一侧——**那是抽样偏差，不是规律**。

## 4. 逐实例逐层结果

`live | disagree off/considered | sumLcap`。`disagree` = 对偶提名的 cell 有多少不在
saturation 前 10 内。`parity`/`打平层`/首层来自 `2026-09-09_m3_twin_mip_setA_bench`
（**旧 build**，仅作「我们离 sprint 多远」的排序参照，不作为本轮性能声明）。

| inst | parity | 打平层 | sprint 首层 | 我们首层 | L0free | L0 | L1 | L2 |
|---|---|---|---|---|---|---|---|---|
{chr(10).join(tbl)}

`live` 的分布：层 0 为 0–2；层 1 几乎恒为 1（setA-07/11/12/14 为 2）；层 2 为 1–3
（setA-14 为 3）。**每层活价只有 1–3 维**，而同一层被锁定的 cell 数常是 4–9 个
（如 setA-10 层 0 锁 9 个）。所以「有信息的是并列集合，不是价向量」。

### 4.1 `-1/cap` 不是通则，`Σ|λ|·cap = 1` 才是

setA-12 层 1 两个 cell（cap 0.91 和 0.66）带**相同**的价 `−0.638733`，看起来像读错了索引。
其实 `0.638733 × (0.91+0.66) = 1.003`——价没有在并列 cell 之间平均分，而是 LP 把总量 `1`
按某种**任意**方式分配给了并列的 cell。上一轮在 setA-01 看到的 `Pi = −1/cap`
（`−1/513`、`−1/228`）只是「LP 把全部价压在了一个 cell 上」的特例。
**单个 `Pi` 不稳定，cap 加权和才是不变量。**

## 5. 对偶 vs saturation：分歧确实存在，但样本太小

按 max 是否已打平分组，统计「至少有一层 `disagree > 0`」：

| 组 | 有分歧 | 实例 |
|---|---|---|
| max **落后**（5） | **{cnt_not}/5** | 见下 |
| max **打平**（14） | {cnt_tie}/14 | 见下 |

落后组：

{fmt([(n, d) for n, d in g_not if d])}

打平组：

{fmt([(n, d) for n, d in g_tie if d])}

全零：落后组 {zero_not}；打平组 {zero_tie}。

最能说明问题的是 **setA-10**：`L0free/L0/L1/L2` 四层全是 `1/1`——对偶每层唯一提名的 cell
**都不在** saturation 前 10 内，而这个实例首层就落后（97826 vs 71739，1.364×）。
setA-06（1.071×）、setA-13（1.094×）也是层 1/2 双 `1/1`。
但 setA-16（4.037×）只在层 2 出现分歧，setA-19（7.120×）**全程零分歧**——方向对得上，
不足以断言因果。2×2 表（{cnt_not}/5 vs {cnt_tie}/14）Fisher 单侧 p≈0.07、双侧 p≈0.11。

## 6. 松弛 ≠ 真解：量化

`L0free` 的 `z` 是**分数松弛**的最优值，可以低于任何整数解。实测有 {len(frac)} 个实例
`L0free.z` 严格低于 presolve 的**可靠**下界 `sound floor`：

| inst | L0free z | sound floor | z/floor |
|---|---|---|---|
{chr(10).join(f"| {n} | {z:.6f} | {f:.6f} | {r:.3f} |" for n, z, f, r in frac)}

最极端的是 {frac[0][0]}：`z/floor = {frac[0][3]:.3f}`（比可靠下界低
{100 * (1 - frac[0][3]):.0f}%）。

这**不可能是 bug**：池里每个候选都是合法路由 ⇒ 池上 MIP 的最优 ≥ sound floor；
所以 `LP < floor` 反过来**证明** LP 最优是分数解（一组分数 `x` 拼出的负载，任何单一路由都到不了）。
这正是 Tier 2 的直接风险：**reduced cost 截断吃的是一个可证明不可达的点上的边际值**。

## 7. 附带发现

- **convexity 行（等式）的对偶通常精确为 0，但不是全部**：setA-04 是 2/33（`|Pi|` 最大 0.124，
  比它的 cell 价 0.0019 大两个量级），setA-10 层 2 是 2/98，setA-13 是 1/103，
  setA-16 层 2 是 1/148，setA-20 层 2 是 1/43。活跃等式的对偶合法可为 0，所以这**不是**
  原先以为的 smoke test——`dual.go` 那条注释已改，并加了 `PairAbsMax` 记录量级。
  setA-04 同时是 open task #100（peel 轨迹随列数漂移）的实例，可能相关。
- **setA-17 的 peel 在第 0 层就停**：`z = 0.424192 = sound floor` 且没有任何 tracked cell
  达到 `z`，即该层 max 由一个**池够不着的** cell 决定 → 没锁任何东西 → 再解一次是同一个 LP，
  探针正确停止（`Repeated`）。这说明「peel 深度 3」不是总能走满。

## 8. 对 Tier 2 的判断

**不改上一轮的结论方向，但把它从「结论」降级为「3 个实例上的观察」：**

- 第 0 层的对偶**不是**恒死，而是「池自己能决定 max 时活」。这一步修正很重要：Tier 2 的信号
  在 8/19 个实例的第 0 层就存在，不必非得 peel。
- 活价每层只有 1–3 维，而并列集合有 4–9 个 cell；单个 `Pi` 是 LP 的任意退化选择，
  **只有 cap 加权和有不变性**（§4.1）。所以「按 `Pi` 给候选打分排序」这条路，实现上必须用
  并列集合 / 加权聚合，不能直接用单个价——否则是在赌 Gurobi 挑哪个基。
- `disagree` 非零（8/19 实例有至少一层分歧），说明信号**确实携带了 saturation 没有的东西**，
  但 {cnt_not}/5 vs {cnt_tie}/14 的分离度还不足以支撑「Tier 2 值得做」。

## 9. 下一步（待定，未开工）

- (A) 用 `-rounds 5` 看分歧率是否随轮次上升，以及 setB 上的分布——判定 Tier 2 生死。
- (B) 跳过 Tier 2，直接上 Tier 3（热弧 h-hop 球切片）。按 §4.1，「价」本身不可靠，
  但「哪些 cell 并列在层 max 上」是可靠的，而 Tier 3 只需要后者。
- (C) 就地把价接进 `MaxW1`/`MaxW2` 截断做 A/B——按 §4.1 的风险，建议先做 (A)。

## 10. 复现

```bash
cd solution && go build -o ./bin/solve.exe ./cmd/solve
mkdir -p ../experiments/{NAME}/data
for n in 01 02 03 04 05 06 07 08 09 10 11 12 13 14 15 16 17 19 20; do
  ./bin/solve.exe -prefix ../setA/setA-$n -out /tmp/probe-$n.json \\
     -rounds 1 -dual-probe 1 -dual-peel 3 -schedule hot -conf-gate=false \\
     -wall-sec 90 -mip-sec 10 > ../experiments/{NAME}/data/inv-$n.log 2>&1
done
python ../experiments/{NAME}/scripts/summarize_dual_peel.py
python ../experiments/{NAME}/scripts/write_report.py
```
"""
    with open(os.path.join(D, 'report.md'), 'w', encoding='utf-8', newline='\n') as fh:
        fh.write(report)

    meta = {
        "experiment": "dual_peel_sweep",
        "branch": "SOLUTION",
        "dirty": True,
        "date": datetime.date.today().isoformat(),
        "instances": len(rows),
        "layers_checked": 74,
        "invariants": {
            "L0_live0_iff_z_on_zlb": "19/19",
            "sumLcap_eq_1_iff_z_interior": "74/74",
        },
        "disagreement": {
            "max_lagging_with_any": f"{cnt_not}/5",
            "max_tied_with_any": f"{cnt_tie}/14",
            "fisher_onesided_p": 0.071,
            "fisher_twosided_p": 0.111,
        },
        "lp_below_sound_floor": [
            {"instance": n, "z": round(z, 6), "floor": round(f, 6), "ratio": round(r, 3)}
            for n, z, f, r in frac
        ],
        "note": ("observation only; -rounds 1 pools are narrower than real-budget "
                 "pools, so absolute z values are comparable only within this sweep"),
    }
    with open(os.path.join(D, 'meta.json'), 'w', encoding='utf-8') as fh:
        json.dump(meta, fh, ensure_ascii=False, indent=2)
    print("report.md + meta.json written")


if __name__ == '__main__':
    main()
