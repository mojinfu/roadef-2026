# T-ASR (ROADEF 2026 "Keep the Flow!") 求解器

资格赛截止 ~2026-10-05，官方 checker 10 min/实例。仓库含官方数据 `setA/` `setB/`
与参照 `sprint_results/`，代码在 `main` 分支。求解器正确性已与官方 checker 逐值对齐
（见 exp02/exp03），剩余工作都是**搜索质量**。

## 代码结构
- `tasr/model/` `tasr/io/`：实例/解 解析与写出
- `tasr/ecmp/`：IGP ECMP 分流系数 `compute_atom`（**精确浮点 `==`**，与官方 networktools
  同款；勿改成容差）+ LRU `AtomCache`
- `tasr/eval/`：`saturations`、目标函数（字典序 trunc6）
- `tasr/algos/search.py`：v0.1（单 waypoint 局部搜索）
- `tasr/algos/search_v2.py`：v0.2（多段 twin/single/shift + 瓶颈优先候选）
- `scripts/`：`bench_setA.py`、`checker_validate.py`、`snapshot_env.py`、Dockerfile
- `experiments/`：实验报告（自包含 `report.md` + `meta.json` + `data/`）
- `tests/`：pytest；跑法 `.venv/Scripts/python.exe -m pytest tests/ -q`

## 目标函数（必须与官方一致的理解）
- **负载向量** = 所有弧 × 2 时隙的负载矩阵拍平后**降序**的向量；
  逐项**截断到 6 位小数**（第 7 位起直接丢，不四舍五入）后做**字典序**比较。
- 第 1 分量 = max 负载 (MLU)；第 1 分量相同再看第 2 分量……依此类推。
- `tasr/eval/objective.py::lex_compare_sorted` 与官方 checker 一致（40/40 逐值验证）。
- **参照尺子**：`sprint_results/loads_vector.csv` = sprint 每实例第一名队伍的负载向量
  （目前已知最好，非理论最优；资格赛 checker 也用同一字典序比较提交）。

## 预算语义（已与 checker 核对）
- inter-slot 预算 = 各需求 `dist(path_{t-1}, path_t)` 之和。
- 双槽同路径（twin）距离 0 → 紧预算实例（budget 1/13）几乎只能做 twin。
- `search.py::path_distance` 数值上等于 checker dist（多段也一致）。

## 当前量化状态（2026-09-06，commit 09f51dc，best-of v1/v2 对 setA×20 vs sprint 参照）
- **第 1 层（max，trunc6）打平 13/20**：setA-01 02 03 05 07 08 09 11 12 15 17 18 20
- **整条向量打平 0/20**：全部输在 max 之后的尾部。打平层数最多的是 setA-15（前 13 层）。
- **7 个 max 落后的实例**（首分差落在第 1 层，相对差）：setA-04 +0.7%、setA-06 +11%、
  setA-10 +29%、setA-13 +229%、setA-14 +3%、setA-16 +789%、setA-19 +1105%。
- 逐实例「打平几层 / 首分差落在第几层 / 该层差值」：`experiments/2026-09-06_exp03_v02_setA_bench/data/sprint_gap_by_layer.tsv`，
  完整结论见 `.../exp03/report.md` §3。
- 对已打平 max 的实例，gap 通常落在第 2 层（如 setA-01 第 2 大负载 0.730 vs 参照 0.552），
  即**尾部均衡**是当前最大短板 → 下一版求解器应主攻尾部均衡而非继续压 max。

## 实验报告约定
- 每实验一个目录 `experiments/<YYYY-MM-DD>_<seq>_<short>/`：
  `report.md`（中文，含表与结论）+ `meta.json`（`scripts/snapshot_env.py` 生成，
  记录 branch/commit/dirty）+ `data/`（原始输出与派生表）。
- **报告结论必须量化**：与参照对比时，给出「打平几层」；若未完全打平，写明
  gap 落在第几层、该层差值/相对差量级。不允许只说「max 打平 X/20」而不说明整向量 0/20。

## 常用命令
- 求解基准：`python scripts/bench_setA.py --solver v1|v2 --time 60 --restarts 1 --outdir runs_v2`
- checker 交叉验证（二进制在 WSL Ubuntu-24.04）：在 git-bash 里必须
  `MSYS_NO_PATHCONV=1`（否则 `/root/...` 会被 MSYS 改写而 exit 127）：
  ```bash
  MSYS_NO_PATHCONV=1 python scripts/checker_validate.py \
      --checker /root/tasr-checker/checker-src/checker-v1.2.2-x86-64_linux \
      --runs runs --solutions
  ```
- 实验快照：`python scripts/snapshot_env.py experiments/<目录>`
