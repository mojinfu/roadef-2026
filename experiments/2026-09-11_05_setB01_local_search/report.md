# 2026-09-11_05 — setB-01 上的 local search：开/关两臂（各 2 遍）

## 0. 结论（先行）

在 setB-01 上，**开 local search 的臂在第 1 层就输**，而且这个差异**不是运行间噪声**：
每臂各跑 2 遍，逐轮 ACCEPT / local 行完全一致（只有 LS 自身的 wall clock 有抖动）。
setA 上"LS 几乎免费"的结论**不适用于 setB**——单次调用从 3ms 涨到 1.5–1.9s。

据此把 `-local-search` 的**默认值从开改为关**（见 `solution/cmd/solve/main.go`），
flag 保留，`-local-search=true` 仍可打开。

## 1. 配置

```
-prefix setB/setB-01 -monitor=false -rounds 400 -wall-sec 180     # LS 开（默认值，未额外传 flag）
-prefix setB/setB-01 -monitor=false -rounds 400 -wall-sec 180 -local-search=false
```

每臂 2 遍：`data/b01_ls_on.log` / `b01_ls_on_r2.log`、`data/b01_ls_off.log` / `b01_ls_off_r2.log`。
setB 无 sprint 参照（`sprint_results/loads_vector.csv` 只有 setA），所以用
`cmd/diffsol` 在两臂解文件之间比。

## 2. 结果

| 臂 | 遍 | accepted | 完成轮数 | 最终首位 rank | 退出方式 |
|---|---|---|---|---|---|
| LS 开 | 1 | 5 | 13 | **704559** | 超 stopAt，被看门狗强杀 |
| LS 开 | 2 | 5 | 13 | **704559** | 同上（复现） |
| LS 关 | 1 | 12 | 22 | **531282** | 正常（wall-sec 180 到点） |
| LS 关 | 2 | 12 | 22 | **531282** | 正常（复现） |

`diffsol`：**LS 关的臂在第 1 层取胜**（531282 vs 704559），
整条向量 LS 关赢 3180 层 / LS 开赢 45 层。

复现性：两臂各自的 2 遍逐行一致（OFF 的 `RESULT` 行三字段全等；ON 的
r1/r1local/r2/r2local/r4/r10/r13 全等，仅 LS 秒数 1.917→1.963、1.485→1.647）。
所以 704559 vs 531282 **不是抽样噪声**，是这两套配置在该实例该 seed 上的确定性差异。

## 3. 分岔现场

```
LS 开: r1 ACCEPT 999998->914701 | local: 30 moves 1.9s -> 911033
       r2 ACCEPT  911033->743905 | local:  5 moves 1.5s -> 无变化
       r4 ACCEPT  743905->704776      r10 / r13 只动尾部           ← 卡住
LS 关: r1 ACCEPT 999998->914701
       r2 ACCEPT  914701->747042
       r4 ACCEPT  747042->531282   ← 大跳水，之后 12 次接受全在尾部
```

两条轨迹在 **r1 的 LS 之后**分开。r2 的接受方向已经不同（743905 vs 747042），
r4 差距拉大（704776 vs 531282）。

## 4. 成本

| 实例 | 规模 | 单次 LS 调用 | 占进程 elapsed |
|---|---|---|---|
| setA-01 / setA-11（见 `2026-09-11_04`） | 80–1000 弧 | 3–86 ms | 0.2–2.0 % |
| setB-01 | 864 弧 × 12 槽 / 6519 需求 | **1.5–1.9 s** | 未及汇总即被杀，未测到 |

成本随**需求数 × 面上 slot 数**走：`local.tasks` 每轮对每个需求在每个面上 slot
各跑一次 `UnitRoute` 求交，setB-01 是 6519×（1–3）≈ 0.7–2 万次/轮 × 3 轮/次调用。
setA 上这个乘法小到看不见，setB 上就变成秒级。

代价也是可观测的：同样 180s，关臂跑完 22 轮，开臂只跑到 13 轮。

## 5. 机制假设（**未验证**，勿当结论）

分岔点与 `local.go` 钩子里的 `dropMem()` 重合：LS 在 r1 把首位从 914701 改到 911033，
`bestFirst < before` 成立 → `dropMem()` 清空 `done`/`skipEpoch`/`softFrozen`/`seedMem`
并 `Reschedule()`。等于**搜索刚积累的记忆被 LS 自己清掉**，而关臂 r4 那个大跳水
（747042→531282）恰好发生在这段记忆本应还在的时候。

要否证或证实它，需要的是"LS 只做尾部改进、绝不动首位（即不触发 dropMem）"的对照臂，
而不是更多重复——现有的 4 次运行已经证明差异是确定性的。

## 6. 边界

- 单实例（setB-01）、单 seed（0）、单 wall budget（180s）。没有 setB 的 sprint 参照。
- 开臂两遍都是被强杀的：第 1 遍被 wall-sec 看门狗、第 2 遍被人工中断，
  因此**没有** `local: calls=... spent=...` 汇总行，LS 的累计占比在 setB 上仍未知。
- 因此本轮只支持"默认关"这一个决定，不支持"LS 在 setB 上有害"这个更强的主张。
