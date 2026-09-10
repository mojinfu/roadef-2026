# 2026-09-11_04 — local search 独立模块的调用开销实测

> **后续（同日）**：本报告测的是 setA 两实例的**成本**，当时 `-local-search` 默认开。
> 当天晚些的 setB-01 对拍（`experiments/2026-09-11_05_setB01_local_search`）显示
> setB 上单次调用涨到 1.5–1.9s 且开臂明显更差，**默认值已改为关**；
> 本报告的数字仍然有效，只是不再对应默认配置。

## 0. 这是测量，不是 A/B

本轮**不是**优化效果 A/B，只量一件事：新加的 `internal/local` 局部搜索模块
**每次被调用到底花多少时间**。按要求（"独立功能模块，不要跑很长的优化 AB"）
只跑单实例、单条轨迹，3 次运行，总耗时 < 90s。

模块本身、触发条件、开关见 `solution/internal/local/local.go` 与
`solution/cmd/solve/main.go` 的 `-local-search*` 一组 flag。

## 1. 运行配置

| 运行 | 实例 | wall-sec | LS | 日志 |
|---|---|---|---|---|
| a01_on | setA-01 | 30 | 开（当时的默认；现已改为关） | `data/a01_ls_on.log` |
| a01_off | setA-01 | 30 | `-local-search=false` | `data/a01_ls_off.log` |
| a11_on | setA-11 | 45 | 开（当时的默认；现已改为关） | `data/a11_ls_on.log` |

公共 flag：`-monitor=false -sprint sprint_results/loads_vector.csv`，
LS 参数值全部未改（rounds=3、call 上限 3s、sat_min=0.01、top_cells=20、probes=5、
全局预算 = 0.20 × wall-sec）。

运行完成后 `solve` 自报一行汇总：

```
local: calls=13 rounds=20 moves=24 spent=0.03s (0.2% of elapsed) top_cells=20 sat_min=0.0100 probes=5 call_cap=3.0s budget=6.0s
```

## 2. 结果

| 实例 | 接受轮数 |= LS 调用数 | LS 轮数 | 接受的 move 数 | LS 累计耗时 | 占进程 elapsed | 单次调用最大耗时 | 单次调用上限 |
|---|---|---|---|---|---|---|---|
| setA-01 | 13 | 20 | 24 | **0.03 s** | **0.2 %** | 0.005 s | 3.0 s |
| setA-11 | 33 | 38 | 49 | **0.69 s** | **2.0 %** | 0.086 s | 3.0 s |

结论（三条，都是实测数）：

1. **3s 的 per-call 上限从来没有被逼近过**：实测单次调用 3–86 ms，比上限低
   1.5–3 个数量级。上限是兜底，不是预期成本。
2. **LS 占整个进程 wall clock 的 0.2%–2.0%**。setA-11 是本批里接受轮数最多
   （33 次调用）的实例，也只有 2.0%，而该次运行 84% 的时间在 Gurobi 之外
   （gurobi 仅 13.6%）。也就是说 LS **没有**和 MIP 抢预算。
3. 触发的全是**第 2 类接受**：每一条 `round N local:` 行的
   `first-bit rank X -> X` 都相等（setA-01 = 929383，setA-11 = 785788），
   即 LS 只在"首位不变、后面某位字典序变好"之后被调用，且单轮 move 数为
   13/6/2/3 和 42/5/1/1，都是尾部层的收益。

## 3. setA-01：开 / 关两臂的最终向量

| 臂 | accepted | 最终首位 rank | 向量对比（vs sprint 参照） |
|---|---|---|---|
| LS 开 | 13 | 929383 | tie **1/160** 层；首个分叉在第 2 层（583674 vs 551952） |
| LS 关 | 18 | 929383 | tie **1/160** 层；首个分叉在第 2 层（583674 vs 551952） |

两臂**收敛到完全相同的字典序状态**（tie 层数、首个分叉层、该层两个数值逐位一致），
且各自独立重跑一次复现同样结果。差异只在 accepted 数（13 vs 18）与
`total_cost`（46 vs 50）——后者按仓库约定**不是指标**，这里只说明 LS 臂留下的
预算余量更大（Hamming 代价更低），并不构成质量优劣。

**这条只能说明"没有回归"，不能说明"有收益"**：单实例、单轨迹、n=1，
且要判收益需要跑多实例多重复的 A/B —— 那正是本轮不做的事。

## 4. 已知边界

- 单条可复现轨迹，不是期望值；Gurobi 带 wall-clock 限制时本身有噪声带
  （见 memory `tasr-seta-benchmark-status`）。
- 未测 close 到 budget 上限的场景：本轮 `budget=6.0s/9.0s`，实际只用 0.03/0.69s，
  所以"撞上全局 LS 预算兜底后行为如何"在这批数据里没有被覆盖。
  代码路径上撞上限即 `Skip["deadline"]` 并结束本轮（`local.go` 的 round 循环）。
