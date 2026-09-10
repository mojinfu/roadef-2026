# M4c 量化报告：sticky 池在 setA(T=2) 上的无回归检验 vs twin/mixed 池

日期：2026-09-10 ｜ 分支 `SOLUTION` ｜ 未提交工作区（`main.go` sticky 自适应 anchor、`pool_sticky.go` override 语义、bench 驱动）

## 1. 目的

迁移计划要求 sticky 生成**在 setA T=2 上退化成现在的 twin 覆盖（不回归）**，才能让 sticky 成为
T=12（setB）的引擎而不丢 setA。本实验在**完全同等的墙钟与并发条件下**对比两种池模型：

- `-model twin`（`BuildCells`）：单槽 single 家族 + 相邻槽 twin 家族，MIP 在一个池内自由混合免费
  twin 与付费 single；
- `-model sticky`（`BuildSticky`，自适应 anchor）：每轮只在一个 seed 槽做决策，waypoint 列前向
  拷贝到 `[seed, seed+span]`，T=2 + span=1 时 seed=0 的决策恒写两槽（免费 twin），seed=1 的决策只写
  槽 1（付费 divergence）。偶数轮且剩余预算 ≥8 锚最热槽，否则锚最早热槽（见 §5 诊断）。

判据：逐实例「打平几层 / 首差层 / 该层数值」全套量化 + 首分量逐实例对比。

## 2. 配置

| 项 | 值 |
|---|---|
| 求解器 | `cmd/solve` 外层循环，`hot-k=6`，`peel=6`，`MaxCandPerDemand=12`，2-wp 超 `Wp2Cap=8000` 跨步抽样 |
| 预算模式 | `full`（每轮可用全部剩余预算） |
| rounds | 150；wall 每实例 90s（求解器内 `-wall-sec`） |
| 并发 | **twin 与 sticky 两个 bench 驱动同时跑**（互相同等 CPU 争抢），保证同墙钟对比公平；大实例实际墙钟 90–130s |
| 二进制 | twin = `solve.exe`（M3 同款）；sticky = `solve_ad.exe`（自适应 anchor，当前 `main.go` 源码构建） |

主数据：`data/twin_90s.tsv`、`data/sticky_ad_90s.tsv`（bench 逐实例 RESULT + sprint compare），
派生对比：`data/sticky_vs_twin.tsv`。参照尺子 = `sprint_results/loads_vector.csv`。

## 3. 同墙钟 sticky vs twin（90s，同并发）

`fb_delta` 为 sticky−twin 的首位秩差（正 = sticky 变差）。tie 层只在首位打平 sprint 时有参照意义。

| 实例 | budget | twin 首位 | sticky 首位 | Δ | 判定 | twin tie | sticky tie | M3 已知最好 twin(180s) |
|---|---|---|---|---|---|---|---|---|
| setA-01 | 51 | 929383 | 929383 | 0 | = | 1 | 1 | |
| setA-02 | 63 | 903074 | 903074 | 0 | = | 6 | 6 | |
| setA-03 | 53 | 943543 | 943543 | 0 | = | 1 | 1 | |
| setA-04 | 44 | 581237 | 581237 | 0 | = | 1 | 1 | |
| setA-05 | 1 | 204985 | 204985 | 0 | = | 8 | 8 | |
| setA-06 | 13 | **105633** | **119718** | +14085 | **回归** | 0 | 0 | 105633 |
| setA-07 | 90 | 907989 | 907989 | 0 | = | 11 | 11 | |
| setA-08 | 13 | 318903 | 318903 | 0 | = | 1 | 1 | |
| setA-09 | 18 | 849650 | 849650 | 0 | = | 4 | 4 | |
| setA-10 | 1 | 108695* | **86956** | −21739 | **改善** | 0 | 0 | 97826 |
| setA-11 | 89 | 785788 | 785788 | 0 | = | 1 | 1 | |
| setA-12 | 13 | 879872 | 879872 | 0 | = | 8 | 8 | |
| setA-13 | 12 | 66239* | 52991 | −13248 | 改善* | 0 | 0 | **44871** |
| setA-14 | 13 | 517621 | 517621 | 0 | = | 5 | 5 | |
| setA-15 | 54 | 898695 | 898695 | 0 | =（尾部差） | **18** | **13** | |
| setA-16 | 13 | 271721* | 204917 | −66804 | 改善* | 0 | 0 | **178688** |
| setA-17 | 1 | 424192 | 424192 | 0 | = | 24 | 24 | |
| setA-18 | 89 | 999998 | 999998 | 0 | =（尾部差） | 12 | **14** | |
| setA-19 | 13 | 562060* | **333489** | −228571 | **改善** | 0 | 0 | 333489 |
| setA-20 | 90 | **991312** | **999999** | +8687 | **回归** | 3 | 0 | |

（\* = 该 twin 行在同并发 90s 下明显弱于 M3 已知最好（`setA-10 97826 / 13 44871 / 16 178688 / 19 333489`，
均为 M3 180s 独立跑）；`sticky_ad` 这些行反而与 M3 最好同量级甚至更优。因此与**已知最好 twin 状态**
比对时，真实结论为：setA-10 **真改善**（86956 < 97826）、setA-19 打平最好（333489 = 333489）、
setA-13/16 仍落后 M3-180s（52991 > 44871、204917 > 178688）。）

## 4. 量化结论

1. **首位打平 sprint 的实例：两模型一致**（01 02 03 04 05 07 08 09 11 12 14 15 17 18 20，其中 15 仅
   twin 打平 18 层、sticky 少 5 层）。两模型在 max 打平 sprint 的集合上无差异，除 setA-15 尾部。
2. **sticky 相对 twin 的首位回归：setA-06（+13.3%，119718 vs 105633；sprint 98591）与
   setA-20（999999 vs 991312=sprint 打平，sticky 0-accept 卡死）**。对已知最好 twin 状态（180s），
   06 的 +13.3% 回归与 20 的打平丢失仍成立。
3. **sticky 相对 twin 的首位改善：setA-10（86956，比 M3 最好 twin 97826 低 11%）为真改善**；13/16/19
   的同并发行好于 twin 同并发行，但其中 13/16 未超过 M3-180s 的已知最好、19 打平已知最好 → 这些是
   **sticky 大实例样本效率高**的体现（受争抢的 90s 做到 180s 独立同款），不是对已知最好状态的超越。
4. **尾部差异**：setA-15 sticky 打平 13 层 < twin 18 层（尾部回归 5 层）；setA-18 sticky 14 > twin 12
   （尾部改善 2 层）。其余 max 打平实例 tie 层两模型完全一致。

**净判定：sticky（单 anchor，span≥1）在 T=2 上不能通过无回归门。** 相对同墙钟 twin，首分量在 06/20
回归；相对已知最好 twin 状态，唯一真改善是 setA-10，而 06/20 的回归不受墙钟影响（06 结构性、20 为
0-accept 卡死）。结论见 §5。

## 5. 回归根因（为什么 T=2 上 sticky 不是 twin 的超集）

1. **前向拷贝不能表达「只改槽 0」**：T=2 上 seed=0 的决策必须把 waypoint 拷到槽 1（span 覆盖到
   T-1）。因此「槽 0 独走、槽 1 保持」这类 divergence 在 sticky 里不可达，而 `BuildCells` 的 single-0
   家族可达。setA-06 的 max 缺口（105633）恰好需要这种表达（budget=13 允许付费 divergence），sticky
   卡在 112676（min-anchor，不花钱）以上。
2. **单 anchor 轮不能在一个 MIP 里混合免费 twin 与付费 override**：twin 池每轮让 MIP 按预算为每个需求
   自由选 single-0 / single-1 / twin 三者之一；sticky 每轮只提供「全 twin」或「全槽-1 divergence」。
   自适应 anchor（rem≥8 偶数轮锚最热槽）在 setA-06 上**过早烧预算**：花了 6 预算反而停在 119718，
   比完全不花的 min-anchor（112676）更差。单轮 MIP 单调不劣（layer-1 z 有 constantFloor 下界且
   candidate 0 恒可行），但轮序改变可达盆地，贪心收敛到更差的局部最优。
3. **setA-20 是规模墙，不是 sticky 特有**：m=2000、需求 6000，池构建 ~22s/轮，peel-6 MIP 在 ~8.6s
   时间帽内连第一个可行解都拿不到（root LP 未出）→ sticky 0-accept；twin 同实例 90s 也只接受 2 次。
   M3 §6 已记录该规模墙。sticky 因每轮还多付 routeAll 双槽验证，首轮更吃亏。
4. **setA-15 尾部**：sticky 把 `[seed..reach]` 全窗口热弧揉进一个统一 relief 排名，top-12 全被
   max-tier 缓解占满，牺牲了 tail-tier 的形状；twin 按家族分别截断保留了更多尾部候选。

## 6. 决策与落地

- **setA（T=2）生产求解器保持 `-model twin`（`BuildCells`）**：它已包含 sticky 在 T=2 可表达的全部
  形状（免费 twin + 槽-1 divergence）并额外支持槽-0 divergence 与池内混合，是 sticky 在 T=2 的
  **退化上确界**；换 sticky 会让 06/20/15 变差而只换回 setA-10 一个改善。
- **sticky（`-model sticky`，span>1 + first_half 预算）作为 setB（T=12）引擎**：多槽前向拷贝是紧
  预算下唯一能同时缓解多个后续槽热单元的形状。setB-01 已验证 sticky-first_half @600s =
  **531319** vs twin 时代基线 **531288**（+0.006%，见任务 #38/#39）。
- 若最终提交想拿 setA-10 的 sticky 增益而不丢 06/20：可在 10 min/实例预算内**双模型各自跑一段、
  保留更优**（如 twin 4min + sticky 4min），作为收尾调参项。

## 数据

- `data/twin_90s.tsv` — twin/mixed 池 90s 同并发逐实例（RESULT + sprint compare）
- `data/sticky_ad_90s.tsv` — sticky 自适应 anchor 90s 同并发逐实例
- `data/sticky_vs_twin.tsv` — 逐实例预算 / 首位差 / 判定 / tie 层派生对比（含 M3-180s 已知最好参照）
