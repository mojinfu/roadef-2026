# T-ASR 求解器项目框架设计（v0.1）

- 日期：2026-09-05
- 目标竞赛：EURO/ROADEF 2026 Challenge — "Keep the Flow!"（T-Adaptive Segment Routing）
- 阶段：Qualification（约 2026-10-05 截止，每算例限时 10 分钟）
- 状态：架构设计稿，未编码

---

## 1. 背景与目标

为 ROADEF 2026 的 T-ASR 问题开发一套求解程序。输入一个算例的三个 JSON
（`*-net.json` / `*-tm.json` / `*-scenario.json`），输出 `*-srpaths.json` 解文件，
使该解在所有弧 × 时间步上的负载降序向量（字典序）尽量小，同时满足：

- 每个 (demand, 时间步) 一条 segment path，段数 ≤ `max_segments`；
- 相邻时间步之间全部 demand 的段集合改动总数 ≤ `budget[t]`；
- 在发生 intervention（链路下线）的时间步，路径在残余图 `G_t` 中逐段连通。

### 设计原则

1. **职责分离**：数据 / 评估 / 算法 / 求解器 / 编排互不耦合，各自可独立测试。
2. **求解器可插拔**：同一个数学模型可跑 HiGHS（本地免费）、Gurobi（官方平台）、CBC（兜底）。
3. **性能预算驱动**：以 setB（最多 ~1263 节点 / ~5000 弧 / ~15000 demand / 12 时段）为基准，
   先保证"快评估器 + delta 更新"足够快，再谈复杂算法。
4. **与官方 checker 语义一致**：评估器输出要与 checker 对齐；比较解用"降序负载向量字典序"。

---

## 2. 总体架构

```
                    ┌────────────────────────────────────────────┐
                    │                engine.py                    │
                    │  单算例编排：阶段调度 / 时间预算 / 结果汇总   │
                    └───────┬──────────────────┬─────────────────┘
                            │                  │
               ┌────────────▼──────┐   ┌───────▼──────────────────┐
               │   algos/ 算法层    │   │   eval/ 评估层            │
               │  baseline / LNS / │──▶│  Evaluator(delta)         │
               │  decompose / GA   │   │  Objective(字典序/凸代理)  │
               └────────┬──────────┘   │  Budget(改路距离)          │
                        │              └───────────┬──────────────┘
                        │                          │
               ┌────────▼──────────┐   ┌───────────▼──────────────┐
               │  model/ 领域模型   │   │  ecmp/ 原子预计算          │
               │  Instance/Solution│◀──│  AtomCache (u,v,t)→负载   │
               └────────┬──────────┘   └──────────────────────────┘
                        │
               ┌────────▼──────────┐        ┌─────────────────────┐
               │  solvers/ 求解器层 │        │  io/ 输入输出         │
               │  ModelSpec(Pyomo) │        │  parser / writer     │
               │  HiGHS/Gurobi/CBC │        └─────────────────────┘
               └───────────────────┘
```

调用方向（自上而下依赖）：`engine → algos → {model, eval, solvers}`；
`solvers` 只认识 `model` 产出的数学规格，不认识任何算法；算法层只依赖
`model` 数据结构和 `eval` 评估接口，不直接 import 具体求解器。

---

## 3. 目录结构（目标形态）

```
repo/
├── tasr/                     # 主包
│   ├── __init__.py
│   ├── config.py             # 全局配置：路径、solver 默认参数、性能预算
│   ├── engine.py             # 单算例编排（见 §9）
│   ├── cli.py                # 命令行入口（对齐 run.sh 4 参数）
│   │
│   ├── io/                   # 输入输出，不掺逻辑
│   │   ├── __init__.py
│   │   ├── parser.py         # net/tm/scenario JSON -> model.Instance
│   │   └── writer.py         # model.Solution -> srpaths.json
│   │
│   ├── model/                # 纯数据 + 纯函数，无求解器依赖
│   │   ├── __init__.py
│   │   ├── graph.py          # 有向图 / 弧索引 / 连通查询(逐时段)
│   │   ├── instance.py       # Instance dataclass + 基本统计
│   │   ├── scenario.py       # Scenario: max_segments/budget/interventions
│   │   └── solution.py       # Solution: paths[demand][slot] -> waypoints
│   │
│   ├── ecmp/                 # ECMP 原子计算（评估地基层）
│   │   ├── __init__.py
│   │   └── atoms.py          # AtomCache: (u,v,t) -> np.ndarray(弧分摊)
│   │
│   ├── eval/                 # 评估层：一切"这个解好不好"都在这
│   │   ├── __init__.py
│   │   ├── evaluator.py      # 全量评估 + delta 更新（核心热路径）
│   │   ├── objective.py      # 字典序比较 / FT 凸代理 / 分层目标工具
│   │   └── budget.py         # 相邻时段改路距离、预算可行性
│   │
│   ├── solvers/              # 求解器层（本设计重点，见 §5）
│   │   ├── __init__.py       # solve(spec, backend=...) 门面
│   │   ├── base.py           # SolverBackend / SolveResult / 配置
│   │   ├── spec.py           # ModelSpec：与求解器无关的数学规格
│   │   ├── pyomo_backend.py  # Pyomo 翻译器：HiGHS / Gurobi / CBC
│   │   └── gurobi_native.py  # 【预留】原生 Gurobi 快速通道（回调/热启动）
│   │
│   ├── algos/                # 算法层
│   │   ├── __init__.py
│   │   ├── baseline.py       # 纯最短路径解 / 随机 / 贪心
│   │   ├── ga.py             # 遗传算法（既有实验代码收敛进来）
│   │   └── decompose/        # 瓶颈驱动的 fix-and-optimize
│   │       ├── __init__.py
│   │       ├── bottleneck.py # 定位最拥塞 (弧 a*, 时间 t*)
│   │       ├── active_set.py # 选出"贡献大"的活跃 demand 集合
│   │       ├── candidates.py # 全网络候选 segment path 池生成
│   │       ├── subproblem.py # 把活跃集合 + 背景流量 -> ModelSpec
│   │       └── improve.py    # LNS 主循环：分解-求解-合回
│   │
│   └── tools/
│       ├── __init__.py
│       └── check_alignment.py  # 与官方 checker 对拍工具
│
├── tests/                    # 单测 + 回归
│   ├── test_ecmp_atoms.py    # 用 Subject 的 toy 网络 Table 2 作 ground truth
│   ├── test_budget.py
│   ├── test_evaluator.py     # 与 checker 输出对齐的冒烟测试
│   └── data/                 # 复制的少量小实例/toy fixture
│
├── docs/                     # 设计文档（本文件所在目录）
├── run.sh                    # 【提交时】包装脚本（对齐 templates/）
├── Dockerfile                # 【提交时】容器定义（基于 templates/ 改）
└── requirements.txt          # python 依赖
```

> 说明：`templates/`、`setA/`、`setB/`、`checker/`、`sprint_results/` 为仓库自带的
> 官方内容，**不放入主包**，仅作输入数据与校验参考。

---

## 4. 核心领域模型（`model/`）

只放数据结构和纯查询函数，任何模块不得在 `model/` 里 import 求解器或算法。

### Instance

```python
@dataclass(frozen=True)
class Instance:
    nodes: tuple[str, ...]                 # 节点名，下标即 id
    arcs: tuple[Arc, ...]                  # Arc(id, from, to, metric, capacity)
    n_slots: int
    demands: tuple[Demand, ...]            # Demand(s, t, volume: tuple[float,...])
    scenario: Scenario                     # 见下

@dataclass(frozen=True)
class Scenario:
    max_segments: int
    budget: tuple[int, ...]                # 下标 t，budget[t] 为 t-1→t 允许改动数（t≥1）
    interventions: tuple[frozenset[int], ...]  # interventions[t] = 下线弧 id 集合
```

要点：
- 一切访问用 `arc.id` / 节点下标，不要字符串。
- 预构建 `adjacency`、`metric_map`、`capacity_map`、`arc_index` 等为 numpy / dict，供图算法与评估层直接消费。
- `G_t` 的下线弧通过"临时把 metric 置 ∞"或"可达性查询带掩码"两种方式提供（`graph.py`）。

### Solution

```python
@dataclass
class Solution:
    # waypoints[d][t]：tuple[int,...]；() 表示纯最短路径(无 waypoint)
    # 只存 v[d]>0 的 (d,t)；其余按"与上一步一致"规则由 writer 补全
    waypoints: list[list[tuple[int, ...]]]
```

约束统一写在 `eval/budget.py` 与 `model/graph.py`，不允许散落各处。

---

## 5. 求解器兼容设计（`solvers/`，本设计重点）

### 5.1 目标

同一份数学模型，能在以下后端切换，仅改一个配置项：

| 后端 | 用途 | 说明 |
|---|---|---|
| `highs` | 本地开发/CI | `pip install highspy`，开源免费 |
| `gurobi` | 官方评估平台 | 容器内已预装 gurobipy + 组委会授权 |
| `cbc` | 兜底 | 极少用 |
| `auto` | 默认 | 按 import 可用性自动选（有 gurobipy 用 Gurobi，否则 HiGHS） |

### 5.2 核心抽象：`ModelSpec` + 翻译器

不用"算法代码里到处 `SolverFactory`"，而是算法产出**与求解器无关的数学规格**：

```python
# solvers/spec.py
@dataclass
class ModelSpec:
    variables: list[VarSpec]     # VarSpec(name, lb, ub, vtype: BIN/INT/CONT)
    constraints: list[ConstrSpec]  # ConstrSpec(name, expr_str 或 结构化 expr, sense, rhs)
    objective: ObjSpec           # 线性或凸分段线性(见 §6.2)
    sense: str                   # "min" / "max"

    # 结构化表达式（可选，性能关键时用）
    # 设计上支持两种表达：a) 通用线性表达式；b) 稀疏矩阵形式
```

翻译器：

```python
# solvers/pyomo_backend.py
def to_pyomo(spec: ModelSpec) -> ConcreteModel: ...
def solve_pyomo(model, backend: str, cfg: SolverConfig) -> SolveResult: ...
```

- HiGHS：`pyomo.contrib.appsi.solvers.Highs`（需 `highspy`）
- Gurobi：`pyomo.contrib.appsi.solvers.Gurobi`（需 `gurobipy`）
- CBC：`SolverFactory('cbc')`

门面：

```python
# solvers/__init__.py
def solve(spec: ModelSpec, *, backend="auto", cfg=SolverConfig()) -> SolveResult
```

### 5.3 为什么第一版选 Pyomo 作交换格式

- 一次建模，多后端可跑，正好满足"临时用 HiGHS，正式用 Gurobi"；
- 与模板 Dockerfile 一致（已预装 pyomo + gurobipy）；
- 子 MIP 规模可控（候选路径选择问题，决策变量 ≈ 活跃 demand × 候选数），Pyomo 建模型开销可接受。

### 5.4 已知代价与预留

- Pyomo 在建大模型/需要回调时有额外开销 → 预留 `gurobi_native.py`：
  若后续 LNS 主循环或列生成需要 Gurobi 原生 API（lazy callback / 热启动 / 更紧控制），
  用**同一份 `ModelSpec`** 再写一个 Gurobi 翻译器，算法层不变。
- 决策记录：**不在算法层出现 `import gurobipy`**；所有求解细节收敛在 `solvers/`。

---

## 6. 评估层（`eval/`）

### 6.1 ECMP 原子预计算（`ecmp/atoms.py`）

核心事实：segment path 相邻 waypoint (u,v) 之间流量沿 `G_t` 的 ECMP 最短路径转发，
**与其它 demand 无关**。故每个"段端点对 (u,v,t)"的弧分摊是固定的、可预计算。

```python
class AtomCache:
    def atom(self, u: int, v: int, t: int) -> np.ndarray:  # shape=(n_arcs,)，单位流量分摊
    # 惰性计算 + LRU；只对"候选/当前解真正用到的 (u,v,t)"计算
```

计算方式（每个 t 一次预处理）：
1. 对每个源 u 跑 Dijkstra 得到 `dist[u][*]`（用下线掩码处理 `G_t`）；
2. 在最短路径 DAG 上按拓扑序做 ECMP 前推，得到单位流分摊到弧的向量。

正确性基准：用 Subject 文档 "toy" 实例 Table 2 的 `r(u,v,a,t)` 作为单元测试真值。

### 6.2 Evaluator（热路径）

```python
class Evaluator:
    def __init__(self, inst: Instance, cache: AtomCache): ...

    def full(self, sol: Solution) -> LoadMatrix      # shape=(n_arcs, n_slots)
    def delta(self, sol: Solution, d, t, new_path) -> LoadMatrix
        # 等价于 full(sol') - full(sol)，只重算 demand d 在 t 的贡献
```

设计目标（setB 上限规模）：
- `delta` 亚毫秒级（numpy 数组加减）；
- `full` 只用于初始解与最终验证；
- 负载 = 背景常量 + 各 demand 选中 atom 的线性叠加，天然向量化。

### 6.3 Objective（`eval/objective.py`）

- `lex_compare(x, y) -> -1/0/1`：把**全部弧 × 全部时间步**的负载统一降序排成一个向量做字典序比较
  （与 `sprint_results/loads_vector.csv` 的语义一致，该 CSV 每行正是"最优解的全弧×全时段负载降序串"，最多 4000 列）。
  - **比较精度（2026-09-05 与用户确认）**：每个负载值只取小数后 6 位，第 7 位起**直接舍弃（截断，非四舍五入）**；
    对截断后的值做降序字典序比较；截断后相等即视为并列（不算 strictly better）。
    实现：`trunc6(x) = floor(x * 1e6 + 1e-9) / 1e6`（x≥0），所有内部"是否更优"判定统一走此函数。
    由于截断是逐元素单调的，先全向量计算→逐元素截断→降序排序→字典序比较 与"先排序再截断"等价，实现取前者。
  - 保留 `compare.rounding: "trunc" | "round"` 配置开关，留待对拍校准（见 §10 R1）。
- 内层 MIP 目标代理（两档）：
  1. `cap(U)`：以"所有负载 ≤ U，最小化 U"为主的可行化/二分；
  2. `ft_cost`：Fortz–Thorup 型凸分段线性代价（利用率过 1/3,2/3,9/10,1,11/10
     斜率 1,3,10,70,500,5000），用于子 MIP 里近似字典序。
- 分层精修工具：给定已固化前缀层（某些弧负载被 pin 到 ≤ U*），
  对剩余层继续压——对应"饱和弧固定法"。

### 6.4 Budget（`eval/budget.py`）

- `path_segments(path) -> frozenset[(i,j)]`：waypoint 序列 → 段集合；
- `dist_between(path_a, path_b)`：段集合对称差大小（与 checker `distEx` 对齐，含空路径特例）；
- `budget_feasible(sol) -> bool` / `budget_cost(sol) -> int`；
- **特别注意**：省略 (d,t) 记录与显式 `w: []` 在 checker 内部距离计算里可能差 1，
  本包约定 writer 一律显式输出全部 (d,t)（空则 `w: []`），并在对拍时验证。

---

## 7. 算法层（`algos/`）

### 7.1 总体策略（对应设计讨论结论）

1. **Phase 0 构建**：读实例 → 预热 AtomCache（只热当前解用到的键）；
2. **Phase 1 构造**：纯最短路径解（全空 waypoint）作为基线；
   可选 GA/贪心先压低第 1 层；
3. **Phase 2 改进（LNS / fix-and-optimize）**：见下；
4. **Phase 3 尾部精修**：按字典序分层，用饱和弧固定法压第 2/3/... 层；
5. **收尾**：writer 输出，checker 验证，时间预算检查。

### 7.2 LNS 主循环（`decompose/`）

```
repeat until (时间耗尽 or 无改进):
  1. bottleneck.locate(evaluator)      # 全局最拥塞 (弧 a*, 时间 t*)
  2. active_set.select(...)            # 对 a* 在 t* 有正贡献的 demand，按贡献排序取前 K
  3. candidates.build(active, a*, t*)  # 全网络生成绕行候选（允许出"局部子网"外绕）
  4. subproblem.build(...)             # 冻结 demand 流量作背景常量 -> ModelSpec
  5. solvers.solve(spec, backend)      # 只动活跃 demand 的 t*（或小时间窗）决策
  6. 合回主解 -> 重算 -> 若改进则接受，否则回滚/扰动
```

要点：
- **背景流量是常量**：冻结 demand 的贡献在 subproblem 里以常量形式进弧负载；
- **候选出子网**：候选生成在全局图上做，不只在你提取的局部子网里；
- **预算**：只改 t*（或窗内几步），冻结项改路成本 = 0，子 MIP 里预算即窗口两侧原始 budget；
- **目标**：子 MIP 用 `cap(U)` / `ft_cost` 代理，配合 §6.3 分层精修。

### 7.3 GA 的去留

既有 GA 实验先收进 `algos/ga.py` 作 Phase 1 的探索手段与随机重启源；
主攻方向仍是 LNS + 精确子问题。后续以数据为准决定是否保留。

---

## 8. 输入输出与 CLI

### 8.1 读（`io/parser.py`）
- 校验 JSON 结构（可与 checker schema 语义对齐）；
- 建 `Instance`；断言 arc 双向成对、demand 不重复、`v` 长度 = n_slots。

### 8.2 写（`io/writer.py`）
- 输出 `{"srpaths":[{"d","t","w"}, ...]}`；
- **约定：显式输出全部 (d,t)**；无 waypoint 写 `"w": []`；
- 零流量 (d,t) 一律沿用上一时段路径（不消耗预算）。

### 8.3 CLI 与 run.sh 契约

```bash
# run.sh（提交形态）
python -m tasr.cli --net $1 --tm $2 --scenario $3 --out $4
```

本地等价：`python -m tasr.cli --instance setB-01`（自动拼四个文件）。

---

## 9. 编排与时间预算（`engine.py`）

Qualification 每算例 10 分钟墙钟。Engine 职责：

- 解析参数、加载实例、预热；
- 把总时间切成阶段预算（构造 / LNS / 精修），带剩余时间倒计时；
- 记录每一阶段接受解的质量（降序负载向量的字典序），随时可产出"当前最优"；
- 多进程随机重启（评测机 8 核）：进程间只共享"当前全局最优"用于提前收敛。

时间预算参数放 `config.py`，用 setB 实测标定。

---

## 10. 风险与待确认项（ADR 式）

| # | 风险 / 不确定 | 影响 | 处置 |
|---|---|---|---|
| R1 | 官方排名比较精度 | 本地自评需与官方一致 | **已确认（用户）**：逐负载截断到 6 位小数（第 7 位起舍弃，不四舍五入）；比较器按 trunc6 实现，保留 round 开关备校准 |
| R2 | "省略 (d,t)" vs `w:[]` 的距离语义差 | 预算计算可能偏差 | writer 一律显式输出；对拍验证 |
| R3 | ECMP 预计算量：setB 上需要多少 (u,v,t) 原子 | 预处理时间/内存 | 惰性缓存 + 候选池上限；实测后设预算 |
| R4 | Pyomo 建子 MIP 开销 | LNS 单轮变慢 | 子 MIP 规模控制；必要时走 gurobi_native |
| R5 | HiGHS 与 Gurobi 求解行为差异 | 本地判定"改进"可能在 Gurobi 下不同 | 本地 HiGHS 只做逻辑验证，正式调参用 Gurobi（学术/试用授权） |
| R6 | 运行环境（容器内）无 HiGHS | 提交代码必须落到 Gurobi 路径 | `auto` 后端 + 容器内预装 gurobipy 已覆盖 |

---

## 11. 开发里程碑（对齐 10/05 截止）

| 周次 | 里程碑 | 产出 |
|---|---|---|
| 第 1 周（本周末前） | 数据层 + ECMP 原子 + 评估器 + 对拍 checker | 能对 setA 算"纯最短路径解"并和 checker 对上 |
| 第 1–2 周 | solver 抽象（HiGHS 跑通）+ baseline + writer | 端到端出合法解；tests 绿 |
| 第 2–3 周 | LNS 分解骨架在 setA 中大规模上跑通，验证第 2 层改进 | 复现/超过既有 Gurobi 全模型在 setA-01 的 2 层结果 |
| 第 3 周 | setB 规模性能标定（评估器/候选/子 MIP） | 单算例 ≤ 10 分钟的预算分配表 |
| 第 4 周 | Gurobi 本地授权到位，正式后端调优 + 多随机重启 | 容器内全流程验收（build + run.sh + 限时） |
| 缓冲 | 尾部精修 + 文档（2 页） | 提交 zip |

---

## 12. 后续动作（下一步做哪些）

1. 搭建 `tasr/io + model + ecmp + eval` 最小闭环，先用 Subject 的 toy 网络 + setA-01
   把 ECMP 原子与 checker 输出对拍上（R1/R2 一并验证）；
2. 写 `solvers/base + pyomo_backend`，用 HiGHS 解一个"候选路径选择"小模型冒烟；
3. 之后才进入 `algos/decompose` 的正式实现。

> 本设计为 v0.1，随对拍结果与实测数据迭代；改动需更新本文件并注明日期。
