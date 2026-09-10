# 2026-09-10_03 候选生成器微基准（candbench）：候选集质量 + 跳数缓存

## 0. 一句话结论

把 MIP 从测量回路里拿掉、只测候选生成器之后，得到三件事：
**(1)** `cand` **本身是不确定的**——`crossedHotArcs` 遍历了一个 map，导致每轮被禁的热弧子集随机，
已修复并补了会失败的回归测试；
**(2)** 跳数缓存是**忠实的纯记忆化**（20/20 实例开关缓存候选集逐位相同），把 BFS 计算量降了
**34.7×**（40560 → 1168），但墙钟只快 **7.1%**——BFS 从来不是瓶颈；
**(3)** 候选集**召回率很低**：对全节点暴力枚举的 top-12 单waypoint，召回只有 **21.9%**，
且在 setA-04/10/13/16 上**丢掉了暴力枚举能找到的最好单点**（relief capture 0.92–0.98）。

## 1. 为什么要做这个基准

第 1 步的 A/B（`_pyref/cand_ab` 三臂）测不出候选层的好坏，原因是外层循环每轮要跑一次 Gurobi：
实测 setA-10 firstbit 在**完全相同的配置**下三次跑出 141304 / 152173 / 152173，即 **~7.7% 的运行间
噪声**；关掉 presolve（`-presolve off`）噪声仍在，说明来源是 Gurobi 自身的多线程 + 每层 peel 的
`TimeLimit`（`internal/mip/mip.go:362`），不是 presolve 的 15s 预算。
这个噪声比待测效应还大，所以那一轮 A/B 的结论不可用。

`cmd/candbench` 把问题还原成候选生成器唯一要回答的事：

> 输入一个题面（实例，或某个中间解的 snapshot）+ 一个（或几个）热弧，
> 输出候选人集合，并报告耗时、BFS 次数、以及相对暴力枚举丢了多少东西。

不碰 MIP、不碰目标函数，单实例秒级完成。用法：

```bash
# 实例 + 自动取全局最热的 6 个 cell，对比有/无跳数缓存
./_pyref/candbench.exe -prefix ../setA/setA-10 -hot-k 6 -recall-k 12 -repeat 6 -reps 3

# 指定一条弧（或一个 demand）——即"扔进来一段弧 / 一对 AB 点"
./_pyref/candbench.exe -prefix ../setA/setA-01 -slot 0 -arc 32 -demand 12 -dump 20

# 从中间解出发
./_pyref/candbench.exe -prefix ../setA/setA-10 -sol ../runs/x.json
```

三个臂：`on`、`on2`（on 的重复，用于把"缓存不纯"和"生成器不确定"两件事分开）、
`off`（`hops.Cache.SetEnabled(false)` 的未缓存对照）。`-repeat R` 把同一批 family 重放 R 遍，
模拟连续多轮的复用；`-reps N` 每个臂跑 N 遍取最快——**单遍墙钟在这台机器上没有参考价值**（见 §5）。

## 2. 发现 1：`cand` 不是确定性的（已修复）

**症状。** 第一版扫描里 setA-02（2/14 family）、setA-04（2/56）、setA-10（3/60）报出
`cache neutrality: FAIL`：同一个 family 在 `on` 和 `off` 两个臂下给出**不同的候选列表**。

**排查。** 缓存本身是干净的纯记忆化（`hops.go` 的 `Forward/Reverse/Undirected` 只是
`graph.HopCounts` 的 memo，从不改写返回切片）。真正的问题在
`internal/cand/strategies.go` 的 `crossedHotArcs`：

```go
want := map[int]bool{}          // 由热弧构造的集合
for _, e := range arcs { want[e] = true }
...
for e := range want { ... }     // ← 遍历 map：Go 每次迭代随机化顺序
```

`bottleneck` 拿到这个有序列表后按顺序禁弧，并且**在 `MaxBans`（默认 3）处截断**。
所以当一条 demand 穿过的热弧多于 3 条时，**被禁的是哪 3 条是随机的**，随之 detour 节点集、
成对候选、以及最终进 MIP 的列都随机。

**影响面。** 这解释了：上面 3 个实例的 neutrality FAIL；以及此前整解 A/B 里 B 与 C 在
setA-10/setA-19 上的差异（此前归因为 Gurobi 噪声，其中至少有一部分是这里）。
它同时是**静默**的——表现为"同样配置两次跑结果不同"，很容易被误读成调参结果。

**修复。** 改为遍历确定性的 `arcs` 切片（`seen` 仍然去重），集合语义不变：

```go
for _, e := range arcs {
    if seen[e] { continue }
    ...
}
```

**回归测试。** `internal/cand/cand_test.go::TestBuildIsRepeatableUnderMapOrderRandomisation`
用 `ladderInstance()`（两条并行的 3 段路径，6 条弧全部 tight）配 `MaxBans=2`，
连续调用 `Build` 64 次并比较候选序列。已验证它**在修复前失败、修复后通过**：

```
修复前: Build call 1 differs from call 1:
         got "b|c|d|bb|bc|cc|"   want "b|c|d|e|bd|be|cd|ce|"
修复后: ok  tasr/internal/cand
```

修复后 20/20 实例 `generator determinism: PASS` 且 `cache neutrality: PASS`。

> 顺带核查：`strategies.go:140` 也遍历 map（`for c := range centers`），但那里是取 **min**、
> 且随后用的是 `sort.SliceStable`，结果与顺序无关，是良性的；`cand.go` 里其余 `range` 都是切片。

## 3. 发现 2：跳数缓存很值钱，但值钱的不是时间

`-repeat 6 -reps 3`，20 个 setA 实例（每实例 60 个 family 封顶），修复后：

| 指标 | 缓存开 | 缓存关 | 比值 |
|---|---|---|---|
| BFS 计算次数 | **1168** | **40560** | **34.7×** |
| 跳数查询次数 | 40680 | 40560 | 1.00×（同一批查询） |
| 墙钟合计（best-of-3） | 6052 ms | 6484 ms | **1.07×（缓存快 7.1%）** |

查询数完全一样（同一次运行里两个臂跑同一批 family），缓存把 97.1% 的 BFS 变成 map 查表，
但墙钟只快 7%。**BFS 在这条路径上从来不是瓶颈**——单次 BFS 在 n≤400 的图上只要几微秒，
而每个 family 真正的大头是 ~25 次 `UnitRoute`（候选真实重路由）和 `recall` 的全节点扫描。

结论：缓存的正确理由是**避免 3.4 万次重复 BFS**（这在大 setB 实例上会随实例数放大），
而不是"让候选生成变快"。它不值得为它牺牲任何正确性，也不该指望靠它提速。

## 4. 发现 3：候选集质量——召回率 21.9%，4 个实例丢失最优单点

`-recall-k 12` 对每个 family 重跑一遍**暴力枚举**（除 AB 两端外所有节点，逐个真实路由，
保留严格降低某热弧的），取 relief 最高的 12 个作为参照，衡量 `cand` 的单waypoint 输出覆盖率。
注意这是**下界**：参照只含单waypoint，`cand` 的两waypoint 候选（占输出 79%）不参与比较。

| instance | n | families | 候选/族 | w1/族 | w2/族 | 空族 | recall@12 | relief capture |
|---|---|---|---|---|---|---|---|---|
| setA-01 | 20 | 22 | 28.6 | 5.9 | 22.7 | 3 | 76.9% | 1.0000 |
| setA-02 | 30 | 14 | 46.0 | 14.0 | 32.0 | 0 | **77.4%** | 1.0000 |
| setA-03 | 50 | 6 | 46.2 | 14.2 | 32.0 | 0 | 51.4% | 1.0000 |
| setA-04 | 50 | 56 | 25.8 | 5.1 | 20.8 | 4 | 45.3% | **0.9381** |
| setA-05 | 100 | 8 | 36.1 | 12.1 | 24.0 | 2 | 27.8% | 1.0000 |
| setA-06 | 100 | 48 | 39.6 | 8.6 | 31.0 | 0 | 26.0% | 1.0000 |
| setA-07 | 100 | 60 | 31.5 | 7.4 | 24.1 | 12 | 21.7% | 1.0000 |
| setA-08 | 150 | 8 | 47.6 | 15.6 | 32.0 | 0 | 28.1% | 1.0000 |
| setA-09 | 150 | 20 | 17.6 | 4.8 | 12.8 | 12 | 17.7% | 1.0000 |
| setA-10 | 150 | 60 | 32.0 | 5.4 | 26.6 | 0 | 16.1% | **0.9797** |
| setA-11 | 200 | 25 | 20.7 | 3.8 | 16.9 | 10 | 15.6% | 1.0000 |
| setA-12 | 200 | 14 | 17.8 | 3.7 | 14.1 | 4 | 41.7% | 1.0000 |
| setA-13 | 200 | 60 | 29.6 | 5.5 | 24.0 | 4 | 15.5% | **0.9371** |
| setA-14 | 250 | 11 | 44.4 | 12.4 | 32.0 | 0 | 15.2% | 1.0000 |
| setA-15 | 250 | 38 | **6.5** | **1.1** | 5.4 | 30 | **3.1%** | 1.0000 |
| setA-16 | 250 | 60 | 28.5 | 5.4 | 23.1 | 3 | 12.7% | **0.9205** |
| setA-17 | 300 | 35 | **6.7** | 2.1 | 4.6 | 30 | 10.0% | 1.0000 |
| setA-18 | 300 | 60 | **0.0** | 0.0 | 0.0 | **60** | 0.0% | 0.0000 |
| setA-19 | 300 | 60 | 35.6 | 7.2 | 28.4 | 0 | **5.6%** | 1.0000 |
| setA-20 | 400 | 60 | **12.2** | **1.1** | 11.1 | 32 | 11.3% | 1.0000 |

合计：**725 family / 17822 个候选（24.6/族）**，其中单waypoint 3744（21.0%）、
两waypoint 14078（79.0%）；tag 分布 hot_center 1753 / od_scan 1408 / bottleneck 333 /
offhot 250 / 两waypoint 14078。**206/725（28.4%）个 family 返回空集**。

三条要读出来的东西：

1. **recall@12 全局 21.9%（1261/5746）**，最好 77.4%（setA-02），最差 3.1%（setA-15）。
   生成器只覆盖了暴力枚举前排的一小部分。这是设计取舍（24/32 的预算 + hop ball 限制），
   但它意味着 MIP 在每个 family 上看到的备选面比历史版本窄。
2. **relief capture < 1 的 4 个实例**（setA-04 0.938、setA-10 0.980、setA-13 0.937、
   setA-16 0.921）说明这些实例上 `cand` **找不到暴力枚举能找的最好单点**——
   不只是"覆盖少"，是"最好的那个根本没进池子"。setA-10/13/16 正是整解 A/B 里
   表现较差或打平的实例，**这是此前 setA-10 回退（97826 → ~15 万）最可疑的机制**。
3. **退化的 family 很集中**：setA-15/17/20 的 w1/family 只有 1.1/2.1/1.1，
   空族 30/38、30/35、32/60。这些实例的 hop ball 在 `MaxExtraHop=4` 下几乎没有可用节点，
   候选层实际只在靠两waypoint 和 offhot 撑着。
   **setA-18 是 0/60，全部为空**——这不是 bug：它的最热弧是 arc 163（sat 0.999998765），
   与 `2026-09-10_02` 里 presolve 证明的 class-1 bridge 是同一条，单waypoint 无解是正确结论
   （该实例首bit在起点即最优）。

## 5. 测量方法上的坑（必须记录）

**单遍墙钟不可用。** 同一配置（setA-20，`-repeat 4`）连跑 5 次：

```
on2 :  570.8  505.8  479.3  539.0  516.7   ms
off :  630.1  591.3  536.2  555.5  570.4   ms
```

也就是 479–571 ms、536–630 ms 的散布（~19%），而且 5 对**全部**是 on2 < off（符号检验 p≈3%），
所以配对取最快是能测出 7–9% 的效应的。但更早一次**单遍**扫描（`runs_candbench2`）
给出的是 on=2625 ms vs off=1038 ms——即"缓存慢 2.5×"的假象，来自同机其他负载。
**任何 30% 以内的墙钟差异都必须 best-of-N 配对测量，BFS 计数才是无噪声的硬指标。**

因此本报告所有结论只用：BFS 计数（确定性，精确）+ best-of-3 配对墙钟（只在 §3 用了一次）。
`runs_candbench2`（单遍、无内重复）保留在 `data/` 旁作反例，不作为结论依据。

**recall 是下界。** 参照只枚举单waypoint；`cand` 输出里 79% 是两waypoint，不参与 recall 比较。
所以 21.9% 不能读作"丢了 78% 的 MIP 备选"，只能读作"暴力枚举前排的单点里覆盖了 21.9%"。
relief capture 没有这个问题——它是同类（单waypoint 最优值）的直接对比。

## 6. 结论与后续

**已落地**
- 修复 `crossedHotArcs` 的 map 遍历（`internal/cand/strategies.go`），加回归测试并验证了
  该测试在修复前会失败。这是本次唯一动到的生产代码。
- `cmd/candbench` + `scripts/tabulate_candbench.py`：候选层可以脱离 MIP 秒级回归，
  避免再次拿 7.7% 噪声的整解 A/B 去调候选参数。

**建议的下一步（按性价比排序）**
1. **针对 relief capture < 1 的 4 个实例**加一条"全局 relief 兜底"策略：暴力枚举的两个成本
   （O(n) 单点路由、C(n,2) 配对）里，单点是便宜的（setA-20 n=400、60 族全量 recall 只要 ~1 s）。
   把"全节点按 relief 排序取 top-K 单点"直接并进池子，就能把这 4 个实例的 capture 拉回 1.0。
2. **hop ball 参数**：setA-15/17/20 的 w1/family 塌到 1–2，`MaxExtraHop=4` 对它们太紧；
   用一个随实例规模或 ball 填充率自适应的额外跳数上限（注意 §12 Q7 现在禁止 size-adaptive 默认，
   需要先改那条设计约束）。
3. 不要在候选层继续投性能预算：BFS 已不是瓶颈，缓存带来的 7% 不足以支撑更多复杂度。

> **后续（2026-09-10_04）**：上面第 1 条建议已落地并测过。用逐 family 数据重算后，
> 「丢掉最优单点」的准确形状是 **14/521 = 2.69% 的 live family 上一个可用单点都没有**
> （不是「差」，是零），影响 4/19 实例；`-global-k 8` 把 capture 拉回 **19/19 = 1.0000**，
> 代价 +0.2~1.1 ms/family。同时发现打开兜底后 **`recall@12` 变成循环指标**
> （兜底取 top-K、参照也是 top-K，地板 8/12=66.7%）——别再拿它横向比较策略。
> 详见 `experiments/2026-09-10_04_cand_global_relief/report.md`。

**数据文件**
- `data/raw/setA-*.txt`：20 个实例的 candbench 完整输出（`-repeat 6 -reps 3`，修复后）
- `data/cand_metrics.tsv`：上表的机器可读版（`scripts/tabulate_candbench.py` 生成）
- `data/tsv/setA-*.tsv`：逐 family 明细（demand/slot/hot 数/候选数/top_rel/耗时/recall 命中）
- `data/main.go`、`data/tabulate_candbench.py`：测量工具源码快照
- `data/strategies.go.postfix`：修复后的 `crossedHotArcs` 源码快照
- 注：`data/raw` 里的输出是字符串修正前的一次运行，唯一差异是 `relieving nodes` 行尾不再打印
  无意义的 `(0 skipped)`；所有数值不受影响。
- 反例数据（不作为结论依据）：`solution/runs_candbench2/`（单遍、无内重复，墙钟被同机负载污染）
