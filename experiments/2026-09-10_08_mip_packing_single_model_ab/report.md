# set-packing 模型 + 每轮单模型（建议 #1 + #3）落地与 setB-01 3 分钟 A/B

- 日期：2026-09-10 夜（运行跨到 09-11 凌晨）
- 分支：SOLUTION，基线 commit `c432181`，工作区 dirty（改动未提交）
- 实例：`setB/setB-01`（m=864 T=12 D=6519），命令统一
  `solve.exe -prefix ../setB/setB-01 -out <json> -rounds 400 -wall-sec 180`
- 双臂二进制（用符号表复核过，不是靠 mtime 推断）：
  - **new** = `runs_diag/solve_v2.exe`，含 `mip.pairPackingRows` ×3 符号
  - **old** = `bin/solve.exe`，`pairPackingRows` 出现 **0** 次
- 原始数据：`data/`（各次运行的完整日志、热单元 dump、`firstbit_all_runs.tsv`）

---

## 1. 改了什么

| | old | new |
|---|---|---|
| 每 pair 的约束 | **凸性** `Σ_k x_{d,k} = 1`（set-partitioning，每轮每个需求必须恰好一次原子移动） | **set-packing** `Σ_{moves 触及 tt-1 或 tt} x ≤ 1`（每 transition 最多一个移动端点，什么都不选合法） |
| 每轮模型数 | 每个 peel 层新建一个 Gurobi env+model | 一个模型贯穿整轮 peel（锁定的 cell 撤软行、加硬上界行） |

set-packing 是关键：它保证每个 transition 最多只有一个移动端点，因此**预计算的标量 delta 仍然精确且按时隙可加**，
同时把「本轮必须动」这个人为约束拿掉——当最大值已被背景（不可移动的）负载钉住时，「什么都不选」才是正确答案。
按 (需求,时隙) 去重是承重逻辑：twin 移动会同时命中它跨越的两个 transition，重复计入会让行变成 `2*x ≤ 1`，
**静默**禁掉池里所有 twin。`internal/mip/mip_test.go` 三个测试专门守这条。

## 2. 结构验证（LP dump，不是断言）

对同一轮池（两臂**都是 19982 个二元列**）dump 出的 `runs_diag/oldlp.lp` / `runs_diag/cur.lp`：

| arm | `=` 行 | `<=` 行 | `>=` 行 | 总行 | 二元列 |
|---|---|---|---|---|---|
| old | **216**（凸性） | 1596 | 46（floor） | 3187 | 19982 |
| new | **0** | 2608 | 46（floor） | 4881 | 19982 |

即：凸性行归零、每 transition 的 packing 行 0→1012 条，floor 行数不变。
池完全相同（列数一致），差异**纯粹**来自行结构 —— 这是 #1 按设计生效的直接证据。

## 3. Gurobi 是否真的被用起来了

new 在本轮 180s 内 `gurobi=31.22s / 41.49s`，占 elapsed 的 **18.4% / 24.4%**（`solve` 每轮打印）。
作为对照，改前的模型被记录为**每层约 90ms 就返回 status=2（证最优）**——LP 松弛等于整数解，Gurobi 无事可做。
set-packing 把松弛显著削弱，求解器第一次有了分支空间。这是本次改动对「MIP 太小 / 用不上 Gurobi」的正面回答，
也是唯一一个**不受 wall-clock 噪声影响**的结论。

## 4. setB-01 3 分钟 A/B（质量口径 = 字典序饱和向量，与官方 checker 同义）

`setB` 无 sprint 参照，故只做两臂相对比较。**不以 `total_cost` 论优劣**——它是 t=1..T-1 的 Hamming 总距离，
即预算消耗量（`snap.TotalCost()` → `WaypointsTotalCost`），随接受次数单调增长，不是质量指标。

全部 runs（`data/firstbit_all_runs.tsv`）：

| arm | run | exit | first-bit rank | accepted | total_cost |
|---|---|---|---|---|---|
| new | b01_new3min | 0 | **531282** | 22 | 266 |
| new | rep1 | 0 | 531288 | 17 | 188 |
| new | rep2 | 0 | 531288 | 17 | 188 |
| old | b01_old3min | 0 | **531282** | 21 | 250 |
| old | rep1 | 0 | 531312 | 11 | 225 |
| old | rep2 | **1** | 第 18 轮中断，无输出 | — | — |

### 4.1 打平层数 / 首分差层

用 **rep1 对 rep1**（同一次会话、同一 flag、当前二进制）比：

```
层   new        old        delta
 1   0.531288   0.531312   new 胜 2.4e-5  (相对 4.5e-5)   <- 首个分差
 2   0.531255   0.531231   old 胜 2.4e-5
 3   0.504930   0.504902   old 胜 2.8e-5
 5   0.499397   0.499384
```

**打平 0 层，首个分差落在第 1 层（MLU）**，方向 new 更优，量级 2.4e-5。
两解的热点结构完全一致：arc 42/43（v1↔v19）、308/309（v16↔v130），全部压在 slot 9。

### 4.2 但这个 A/B 分不出胜负

old **自身**两次运行就从 531282 摆到 531312，摆幅 **30 个 rank 单位（相对 5.6e-5）**，
比它和 new 之间的首层差（2.4e-5）**更大**。new 那 2.4e-5 的优势完全落在 old 的 run-to-run 噪声带里。

结论：**在 setB-01 上跑 180s，本次改动对解质量的提升不可判定**（既不能说变好，也不能说变坏）。

### 4.3 附带观察

- new 两次 repeat **逐字节相同**（`rep_new_1.json` 与 `rep_new_2.json` 一致）→ 当前构建在同一 wall 预算下确定。
- old 的 repeat 有一次 **exit=1、第 18 轮无输出中断**（日志无 panic/error 文本）。
  old 每层新建一个 Gurobi env，日志里 `Set parameter LicenseID` 每轮刷十几行，疑似 env 泄漏；
  未深挖，但 old 的 3 分钟基线本身不稳。
- 一次**流程失误**：`b01_new3min` 启动于 23:35:50，而 `solve_v2.exe` 在 23:36:21 被重建，
  该次运行用的是被覆盖前的镜像（其 `solves=243 / avg cands=3342` 与 rep 的 `33 / 15199` 差一个量级，
  说明确非同一构建）。第 4 节表格里 `b01_*` 两行因此**不是干净基线**，只有 `rep_*` 行可比。

## 5. 结论与下一步

1. #1（set-packing）与 #3（每轮单模型）**已落地并通过结构验证**：凸性行 216→0，packing 行 +1012，池 19982 列不变。
2. Gurobi 参与度从「90ms 证最优」变为「占 wall 的 18–24%」，模型不再平凡 —— 这部分目标达成。
3. **解质量在 setB-01/180s 上尚不可判定**，差距小于 old 自身的噪声带。
4. 要把「模型变强」翻译成「解变好」，下一步应做**多实例 × 多 repeat**的配对比较（每实例 ≥3 次、看首分差层的符号一致性），
   而不是继续加长单实例时长 —— 180s 的噪声带已经很宽。
5. 待办：old 侧 `exit=1` 的根因未查（低优先级，基线不再使用）。
