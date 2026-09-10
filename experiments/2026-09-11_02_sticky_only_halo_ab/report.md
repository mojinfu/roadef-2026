# 2026-09-11_02 sticky-only 模型 + halo 机制 A/B

## 0. 一句话结论

删掉 twin 模型（sticky 成为唯一池构建器）后，把 halo 从 `BuildCells` 移植到
`BuildSticky`，并对比三种配置：**无 halo** / **每轮 30% 触发** / **每个 demand 独立
30%**。实测 **每轮 30% 触发明显最好**（setA 上 3 胜 4 平 0 负）；per-demand 版本
大面积退化成"等于没有 halo"。但 halo 的代价是**每轮开销 1.5–9×**，在固定 wall 预算
下会吃掉轮数，这是它当前最大的问题。

## 1. 背景与动机

### 1.1 删 twin

`-model twin`（`pool.go::BuildCells`）被判为劣势模式，删除。现在 `BuildSticky`
是唯一的候选池构建器，`cmd/solve`、`cmd/bench` 都不再有 `-model` 旗标，
`cmd/mipround` 改用 `BuildSticky(ht, [{ht,ha}], 1)`。

sticky 语义（用户确认）：一次决策锚定在 seed slot `t`，选中的 waypoint 列表
**逐字前向拷贝**到 `t+1 … t+span`（遇首个零流量 slot 停止），run 内 inter-slot
Hamming 恒为 0；新决策**覆盖**旧路由，这正是"零预算合并两种 regime"的表达。

### 1.2 halo

动机（用户原话）："我们只解冻了热 arc 的相关 demand 需求……绕路后会占用其他 arc
的容量，把这些 arc 上的相关需求也解冻，让 MIP 调整。"

实现：`BuildSticky` 在 `appendMove` 里累加 `radGain[(slot,arc)] += newBlk - incumbent`，
即本轮候选**新增**加载的弧；`Pool.Radiation` 按 gain 降序定序。`ExpandHalo` 以
`pool.Radiation` 为 hot 建第二个 sticky 池，`ForceSlots = pool.Slots` 保证两池的
Load 块布局一致，再按 demand 合并进同一个 MIP（共享 z，因此 MIP 能看出"压下一个
热点会挤爆另一个"）。

预算：`limit = mult × len(pool.Pairs)`（`mult=3`，即"非光环预算的 3 倍"），
**辐射单元数与新增 demand 数各自**受此钳制。随机种子固定 0。

### 1.3 两种触发粒度

| 配置 | 含义 | 旗标 |
|---|---|---|
| 无 halo（控制臂） | 永不建光环池 | `-halo=false` |
| **每轮 30% 触发** | 每轮掷一次 `(seed, round)` 哈希，触发则建**整池** | `-halo=true`（默认，`-halo-prob 0.30`，`-halo-admit 1.0`） |
| 每个 demand 独立 30% | 每轮都建，池内每个 demand 独立掷 `(seed, round, demand)` | `-halo-admit 0.30` |

两个哈希都是 (seed, round[, demand]) 的 splitmix64，不是种子流，所以第 r 轮的判定
不依赖前面跑了多少轮（wall-clock 下轮数会变，用流会让特性不可复现）。

## 2. 方法

- 二进制：`solve_sticky.exe`（每轮版）/ `solve_pd.exe`（per-demand 版）/ `solve_pr.exe`（回退版）
- 命令：`-prefix ../setA/<inst> -sprint ../sprint_results/loads_vector.csv -rounds 400 -wall-sec 60 -monitor=false`
- seed = 0，单次运行（用户选定"就固定 seed=0 复现"，因此以下都是**一条可复现轨迹**，不是期望值）
- 度量：`compareSprint` 输出的**打平层数**与**首分差所在层 / 该层 ours vs sprint**。
  这是截断到 6 位小数的字典序比较，`total_cost` **不是**质量指标（本表一律不引用它）。

### 2.1 回归前提（必须先过）

| 检查 | 结果 |
|---|---|
| halo 关闭，重构后二进制 vs 重构前 sticky 基线 | **字节一致**（sha256 `f988d1e9e0ed5c60…`） |
| twin 删除后 `-halo=false` 行为 | 未变（同上一条） |
| 回退到每轮 30% 后，默认臂 vs 先前实测的每轮臂 | **字节一致**（`fired on 26/92 rounds, +424 pairs`，L2 583674） |

## 3. 结果：三臂对比（setA，60s，seed 0）

打平层数 / 首分差层 / 该层 ours vs sprint：

| 实例 | 无 halo | **每轮 30%** | 每-demand 30% |
|---|---|---|---|
| setA-01 | 1/160, L2 591378 vs 551952 | 1/160, L2 **583674** vs 551952 | 1/160, L2 **603083** vs 551952 |
| setA-02 | 5/300, L6 480551 vs 478554 | **6/300**, L7 465630 vs 461319 | **6/300**, L7 465630 vs 461319 |
| setA-03 | 1/500, L2 630137 vs 621622 | 1/500, L2 630137 | 1/500, L2 630137 |
| setA-05 | 8/792, L9 91845 vs 91045 | **10/792**, L11 83847 vs 81929 | 8/792, L9 91845 vs 91045 |
| setA-08 | 1/1308, L2 306868 vs 286410 | **8/1308**, L9 210028 vs 195125 | 1/1308, L2 306868 vs 286410 |
| setA-11 | 1/2000, L2 644600 vs 613543 | 1/2000, L2 644600 | 1/2000, L2 644600 |
| setA-15 | 13/2500, L14 440400 vs 440081 | 13/2500, L14 440400 | 13/2500, L14 440400 |
| setA-17 | 26/2540, L27 75818 vs 72370 | 15/2540, L16 140380 vs 119336 ‡ | 25/2540, L26 85020 vs 76736 |

‡ **setA-17 这一格是假象**，见 §4.1。

净账（每轮 30% vs 无 halo）：**+10 层 / -11 层 / 6 项持平**，但唯一的那 -11 是
轮数混淆造成的（§4.1 等轮数下两臂**字节一致**）。

### 3.1 setB-01（无 sprint 参照，两臂直接比字典序向量）

`solve_sticky.exe`，180s，两臂：

| 层 | 每轮 30% on | off |
|---|---|---|
| 1 | 531282 | 531282 |
| 2 | 531261 | 531261 |
| **3** | **504905** | **504904** ← off 优 1e-6 |
| 4 | 504883 | 504884 |
| 5 | 499361 | 499376 |
| 6 | 499355 | 499340 |

**打平 2/10368 层，首分差落在第 3 层，off 优 1 个 rank 单位（1e-6，即 6 位截断的
分辨率下限）**。两臂 firstbit 完全相同（531282）。

但这一格同样是 wall-clock 混淆的：on 跑了 **26 轮**，off 跑了 **234 轮**（9×）。
等轮数（各 26 轮，`-wall-sec 0`）复跑：on 用 **181s**，off 用 **26s**，**两臂 firstbit
仍同为 531282**。所以 halo 臂用 11% 的轮数打到了 1e-6 之差的结局。

## 4. 关键发现

### 4.1 wall-clock 混淆：halo 让每轮贵 1.5–9×

固定 60s 时，halo 臂能跑的轮数：

| 实例 | off 轮数 | 每轮 30% 轮数 | off 轮数 → on 轮数 |
|---|---|---|---|
| setA-01 | 94 (3.8s) | 92 (14.9s) | 已收敛，非 wall 限制 |
| setA-05 | 394 (13.6s) | 394 (23.3s) | 两者都收敛 |
| setA-08 | 398 (29.0s) | **273** (50.3s) | 398 → 273 |
| setA-11 | 389 (30.1s) | 303 (50.1s) | 389 → 303 |
| setA-15 | 385 (41.6s) | **168** (50.0s) | 385 → 168 |
| setA-17 | 193 (50.0s) | **41** (52.0s) | 193 → **41** |

**等轮数对照（`-rounds 40 -wall-sec 0`）**：

| 实例 | off | 每轮 30% | 每轮耗时比 |
|---|---|---|---|
| setA-01 | 1/160, L2 591378 | 1/160, L2 **583674** | 3× |
| setA-05 | 8/792, L9 96721 | **10/792**, L11 83847 | 2× |
| setA-08 | 1/1308, L2 306868 | **8/1308**, L9 210028 | 1.5× |
| setA-17 | 15/2540, L16 140380 | 15/2540, L16 140380 → **完全相同** | **4.1×** |

结论：等轮数下 3 胜 1 平；setA-17 的"变差 11 层"**撤回**。

### 4.2 per-demand 30% 大面积退化成"无 halo"

setA-05、setA-08、setA-11、setA-15 的 per-demand 结果与 off **数值完全相同**
（91845 / 306868 / 644600 / 440400）。解释：把 30% 的 demand 撒进**每一轮**，是给每次
求解加列但从不给出一个决定性的替代解，剥层（greedy peel）于是选中同一个解。

同时它是**更差**的：setA-01 从 591378 恶化到 603083（加列会改变 greedy peel 先
commit 哪个最优解），setA-17 从 26/2540 掉到 25/2540。

而且它**并不更省**：halo 开销摊在 100% 的轮次上，setA-08 仍是 398→273 轮、
setA-15 385→177 轮。

### 4.3 开销集中在候选生成，不在 MIP

setB-01 每轮均值：

| | 每轮 30% on | off | 倍数 |
|---|---|---|---|
| pool pairs | 306.2 | 119.6 | 2.6× |
| candidates | 9712 | 3442.9 | 2.8× |
| **hops 查询** | **244k** | **1.8k** | **135×** |

池子只大 2.6×，背后的 hop 查询却大 135×。所以瓶颈不是变大的 MIP，而是
`cand.Build` 被要求在 3× 的辐射集上重跑一遍候选生成（`Hots` 密集 → bottleneck /
OD 扫描成倍放大）。`-halo-mult` 不是唯一的旋钮，真正该压的是"一次让候选生成服务
多少个辐射单元"。

### 4.4 一处结构性限制

halo 在 sticky 下锚定 base 轮自己的 `seedT`，而 `BuildSticky` 只接纳在锚点 slot
上流量非零的 demand。一个坐在**落点弧**上、但在 `seedT` 无流量的 demand，对 halo
不可见 —— 而它恰恰是被挤的那个。要拓宽得让 halo 以"最早的辐射 slot"为锚。

## 5. 结论与下一步

1. **sticky 是唯一模型**；twin 已删除，`-halo=false` 与重构前字节一致，无回归。
2. **触发粒度定为每轮 30%**（`-halo-prob 0.30`）。per-demand 保留为 `-halo-admit`
   旗标（默认 1.0 = 不采样），其 2026-09-11 的实测结论留在这里备查。
3. **halo 目前是"等轮数下正收益、固定 wall 下近似抵消"**。它的真正病根是每轮
   135× 的 hop 查询（§4.3），不是在模型质量上。
4. 下一步优先级建议：
   - **压开销**：限制一次传给 `cand.Build` 的辐射单元数（比如按 gain 取 top-K，
     K 与 base 池需求数同阶），再重扫 `-halo-mult`。
   - 之后再考虑 §4.4 的锚点拓宽。
   - 所有后续 A/B 必须报告**轮数**，否则又会得到 §4.1 那样的假象。

## 6. 复现

```bash
cd solution && go build -o bin/solve.exe ./cmd/solve
# 控制臂
./bin/solve.exe -prefix ../setA/setA-17 -sprint ../sprint_results/loads_vector.csv \
  -out /tmp/off.json -rounds 400 -wall-sec 60 -monitor=false -halo=false
# 处理臂（默认即每轮 30%）
./bin/solve.exe -prefix ../setA/setA-17 -sprint ../sprint_results/loads_vector.csv \
  -out /tmp/on.json -rounds 400 -wall-sec 60 -monitor=false
# 等轮数对照（消掉轮数混淆）
./bin/solve.exe -prefix ../setA/setA-17 -out /tmp/eq.json -rounds 40 -wall-sec 0 -monitor=false
```

数据：`data/`（`ab_*` = 每轮臂，`pd_*` = per-demand 臂，`pr_*` = 回退验证，
`b01_*` / `eqB01_*` = setB-01 两臂与等轮数复跑，`sticky_base_a01.*` = 重构前基线）。
