# setB-01：set-packing 模型 + 每轮单模型 A/B（建议 #1 + #3）

- 日期：2026-09-11
- 分支：`SOLUTION`（未提交）
- 实例：`setB-01`（N=264 M=864 D=6519 T=12）
- 预算：`-rounds 400 -wall-sec 180`（题目要求的 3 分钟档）
- 对照臂：
  - **new** = `runs_diag/solve_v2.exe`，含 `pairPackingRows`（set-packing）+ 每轮单模型
  - **old** = `bin/solve.exe`，凸性 `Σ_k x = 1` + 每层新建模型
  - 两臂均带 worktree 的其余改动（`internal/sched`、`cand`、`dual`）；两臂之间**只有
    `internal/mip/mip.go` 不同**（`grep -a -o 'pairPackingRows'`：new=3，old=0）。

## 1. 结论（量化）

| 指标 | new (packing) | old (convexity) |
|---|---|---|
| 第 1 分量 MLU (trunc6) | **0.531288** | 0.531312 |
| 打平层数 | **0**（首个差距即在第 1 层） | — |
| 首 gap 层差值 / 相对差 | **24e-6 / 4.5e-5** | — |
| 第 2 分量 | 0.531255 | 0.531231 |
| 第 3 分量 | 0.504930 | 0.504902 |
| 180s 内完成的 round | **34** | 24 |
| accepted | **17** | 11 |
| presolve 地板 rank | 141921 | 141921 |

- **打平 0 层，首个差距落在第 1 层（MLU 本身）**，new 低 24e-6（相对 4.5e-5）。
- 两臂各自的两次重复**逐位相同**（new：17/531288/188 ×2；old：11/531312/225 ×2），
  故该差值不是运行间噪声；但幅度很小，单实例、单档预算，不能外推成普遍优势。
- 两臂距 presolve 地板（141921）都还很远（531288 vs 141921），瓶颈不在模型表达力。

## 2. MIP 结构变化（setB-01 第 1 轮，同一候选池）

用 `TASR_GRB_LP` 导出两臂的 LP 逐行统计（变量数完全相同 = 同一池，可直接对比）：

| 行类型 | old | new |
|---|---|---|
| 二元变量 | 1814 | 1814 |
| 总行数 | 1858 | 2654 |
| 凸性行 `Σ_k x_{i,k} = 1` | **216** | **0** |
| set-packing 行 `Σ x ≤ 1` | 0 | **1012** |
| floor 行 `≥` | 46 | 47 |

即：可行域从 **set-partitioning** 变成 **set-packing**——每个需求不再被强制每轮必须动一次，
"什么都不选"成为合法解，这正是 max 已被不可移动的背景钉住时所需要的自由度。

## 3. 每轮单模型的收益

| | new | old |
|---|---|---|
| rounds（180s 内） | 34 | 24 |
| Gurobi 累计 | 41.43s（24.4%） | 30.62s（17.8%） |
| 平均池对 / 候选列 | 198.8 / 15199.3 | 207.6 / 16089.3 |

同一预算内 new 多跑了 **10 轮（+42%）**；old 每层新建模型/环境的开销被省掉。

## 4. 复现

```bash
cd solution
./runs_diag/solve_v2.exe -prefix ../setB/setB-01 -out runs_diag/rep_new_1.json -rounds 400 -wall-sec 180
./bin/solve.exe         -prefix ../setB/setB-01 -out runs_diag/rep_old_1.json -rounds 400 -wall-sec 180
TASR_GRB_LP=runs_diag/cur.lp    ./runs_diag/solve_v2.exe -prefix ../setB/setB-01 -out runs_diag/_throw.json -rounds 1 -wall-sec 60
TASR_GRB_LP=runs_diag/oldlp.lp  ./bin/solve.exe         -prefix ../setB/setB-01 -out runs_diag/_throw.json -rounds 1 -wall-sec 60
```

## 5. 注意事项

- 更早的一对 `runs_diag/b01_new3min.json` / `b01_old3min.json`（两者 first-bit 同为 531282）
  **不可用**：new 臂的 exe 在该次运行进行中被重新编译（exe mtime 23:36:21 晚于进程启动
  23:35:50），即那一对并非同一构建，已弃用。本报告只采用第 1 节的重跑对。
- `total_cost` 是 **Hamming 预算消耗**（`WaypointsTotalCost`，t=1..T-1 累加），不是质量指标，
  本文不将其用于优劣判断。
- setB 无 `sprint_results` 参照，故按字典序直接比较两臂负载向量。
