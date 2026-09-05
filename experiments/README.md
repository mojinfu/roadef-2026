# 实验报告目录 (experiments)

本目录记录 T-ASR 求解器每个实验阶段的**代码版本、模型说明、测试集、结果与结论**。
每个实验一个子目录，自包含：`report.md`（我撰写的主报告）+ `meta.json`（自动生成的
代码版本/环境快照，用 `scripts/snapshot_env.py` 生成）+ `data/`（原始输出/派生表）。

## 目录结构约定

```
experiments/
  README.md                                    # 本索引
  <YYYY-MM-DD>_<seq>_<short-name>/
    report.md                                  # 实验报告（中文，含表与结论）
    meta.json                                  # git branch/commit、dirty 文件、python 版本
    data/                                      # 原始与派生数据（checker json、csv 等）
```

生成/刷新某实验的版本快照：

```bash
python scripts/snapshot_env.py experiments/2026-09-06_exp02_checker_xval
```

## 实验索引

| 实验 | 日期 | 求解器 | 测试集 | 一句话结论 |
|---|---|---|---|---|
| [exp01 v0.1 setA 基准](2026-09-05_exp01_v01_setA_bench/report.md) | 2026-09-05 | v0.1 单 waypoint 局部搜索 | setA×20 | max 负载 9/20 与 sprint 第一名持平；整向量 19/20 落后；setA-12 出现"好于 sprint"但可信度存疑 |
| [exp02 checker 交叉验证](2026-09-06_exp02_checker_xval/report.md) | 2026-09-06 | v0.1 解 | setA×20 | 官方 checker 逐实例核对负载/MLU，定位并修复评估器偏差 |
| exp03 v0.2 setA 基准 | 2026-09-06 | v0.2 多段+协同 | setA×20 | （进行中） |

## 数据与脚本入口

- 实例数据：`setA/`、`setB/`（官方，git 已跟踪）
- 求解器源码：`tasr/`（`tasr/algos/search.py` 等）
- 官方校验：`checker/`（C++，`scripts/docker_checker.Dockerfile` 构建容器）
- 运行脚本：`scripts/bench_setA.py`、`scripts/checker_validate.py`
- 参照结果：`sprint_results/loads_vector.csv`（sprint 各实例最优解负载向量）
