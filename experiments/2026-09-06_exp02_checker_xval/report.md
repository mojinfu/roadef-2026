# exp02 — Python ECMP/评估器 vs 官方 checker 交叉验证

- 日期：2026-09-06
- 代码版本：见 `meta.json`（HEAD commit `e93ef7e`，branch `main`）
- 校验对象：
  - Python 侧 `tasr/ecmp/atoms.py::compute_atom`（IGP ECMP 分流系数）
  - `tasr/eval/evaluator.py::Evaluator.saturations`（逐弧负载/饱和）
- 官方侧：`checker` C++ 静态二进制 v1.2.2（WSL Ubuntu-24.04 内构建，见
  `scripts/docker_checker.Dockerfile`），输出逐弧 `saturations` 与每槽 `mlu`
- 测试集：setA 全部 20 实例 × 2 种输入 = **40 次比较**
  - `baseline`：全部空 waypoint（纯 IGP 最短路径 + ECMP）
  - `solved`：`runs/<name>-srpaths.json`（v0.1 解，含 waypoints）
- 原始输出：`data/checker_xval.log`
- 复现命令（**必须** `MSYS_NO_PATHCONV=1`，否则 git-bash 会把 `/root/...` 改写成
  Windows 路径）：
  ```bash
  MSYS_NO_PATHCONV=1 python scripts/checker_validate.py \
      --checker /root/tasr-checker/checker-src/checker-v1.2.2-x86-64_linux \
      --solutions > experiments/2026-09-06_exp02_checker_xval/data/checker_xval.log
  ```

## 1. 为什么做这次验证

exp01 曾用"节点 id 反转"的实例（setA-05/08/12/14/17）把文件 id 当位置路由，出现
错误结果，甚至把 setA-12 报成"好于 sprint"。修复后需要一条独立于 Python 实现的
证据链，证明：

1. 我们的 ECMP 分流与官方完全一致（**不能**用容差判定 near-tie，官方用精确浮点
   `==`，见 `atoms.py` 注释）；
2. 我们对 waypoint 路由（segment 拼接、去重、跳过 u==v）的解释与官方一致；
3. 基线/求解输出的每一条逐弧饱和值都能被官方 checker 逐值复现。

## 2. 结果

**40/40 全部 PASS**，容差 `1e-6` 内，实际最大偏差 **~1e-12**（纯浮点舍入噪声，
比官方评分截断的 `1e-6` 低 6 个数量级），`n_over_tol=0`。Checker 对所有输入判定
`valid=true`（路径可达、段数合规、预算合规）。

| 项目 | 值 |
|---|---|
| 比较数 | 40（20 实例 × baseline/solved） |
| PASS | 40/40 |
| 总体最大逐值偏差 | ~1.00e-12 |
| 超过 1e-6 的值个数 | 0 |
| checker 有效性判定 | 全部 valid |

按实例汇总（maxdiff 单位 e-12，两行分别对应 baseline / solved）：

| inst | maxdiff | inst | maxdiff |
|---|---|---|---|
| setA-01 | 0.995 / 1.00 | setA-11 | 1.00 / 1.00 |
| setA-02 | 1.00 / 1.00 | setA-12 | 0.999 / 1.00 |
| setA-03 | 0.995 / 0.995 | setA-13 | 1.00 / 1.00 |
| setA-04 | 0.997 / 0.995 | setA-14 | 0.999 / 0.999 |
| setA-05 | 0.999 / 1.00 | setA-15 | 1.00 / 1.00 |
| setA-06 | 1.00 / 1.00 | setA-16 | 1.00 / 1.00 |
| setA-07 | 1.00 / 1.00 | setA-17 | 1.00 / 1.00 |
| setA-08 | 1.00 / 0.998 | setA-18 | 1.00 / 1.00 |
| setA-09 | 1.00 / 1.00 | setA-19 | 1.00 / 1.00 |
| setA-10 | 1.00 / 1.00 | setA-20 | 1.00 / 1.00 |

## 3. 结论与含义

1. **ECMP 核心正确**：两条独立实现（Python 动态规划 vs C++ networktools）逐值一致，
   包括精确浮点 `==` 的 near-tie 拓扑。
2. **评估器 = checker**：`Evaluator.saturations` 可直接代替官方 checker 作搜索内层
   目标评估；搜索过程中用增量 `sat` 而不用完整 recompute 也不会引入系统性偏差
   （增量与全量差异见 exp01/test_search.py 单测）。
3. **脚本/路径易错点记录**：Windows 下调用 Linux 二进制必须经 WSL，且 git-bash 的
   MSYS 路径转换会把 `/root/...` 改写为 `D:\apps\git\Git\root\...`，报
   `checker exited 127`。以 `MSYS_NO_PATHCONV=1` 前缀运行即可（本目录 `meta.json`
   与命令均记录了该坑）。
4. 前置 exp01 的结论因此可信：与 sprint 参照的差距是**搜索质量**问题，不是评估模型
   偏差问题。v0.2 把精力放在邻域表达（多段 twin + 协同移动）而非修评估，方向正确。
