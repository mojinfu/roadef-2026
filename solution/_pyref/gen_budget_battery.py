"""Generate within-budget setA-01 srpaths solutions that exercise each branch of
the checker's Hamming budget metric.

setA-01 has 2 slots and budget[1]=51, so any *valid* solution has
total_cost = cost(t=1) <= 51.  We select small demand subsets per scenario and
only ever write feasible chains (every consecutive segment connected at its
slot), so every emitted file should be accepted by the official checker.

Scenarios:
  empty_to_1wp   : M demands take a 1-waypoint path at t1, nothing at t0.
                   per-demand cost = 1+2 = 3 (node-count branch).
  empty_to_2wp   : M demands take a 2-waypoint path at t1, nothing at t0.
                   per-demand cost = 2+2 = 4.
  1wp_to_empty   : M demands take a 1-waypoint path at t0, nothing at t1.
                   per-demand cost = 3.
  1wp_to_2wp     : M demands take 1-wp at t0 (head w1), 2-wp at t1 (w1, w2).
                   mask grows: cost = 3 per demand.
  identical_2wp  : M demands take the *same* 2-wp chain at both slots.
                   cost = 0 (both explicit, equal masks).
  2wp_maydiff    : M demands take a 2-wp chain at both slots; at t1 the chain
                   is reversed for roughly half -> cost 0 or 6.
  boundary_17    : 17 x empty_to_1wp  -> total cost exactly 51 (valid edge).
  boundary_18    : 18 x empty_to_1wp  -> total cost 54 > 51 (invalid edge).
"""
import json
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0] if "/" in __file__ else ".")

from tasr.io.parser import load_instance
from tasr.model.graph import DirectedGraph
from tasr.ecmp import distances_from, distances_to


def chain(inst, graph, dem, t, k, rev=False):
    blocked = inst.scenario.interventions[t]
    df0 = distances_from(graph, dem.source, blocked)
    dt = distances_to(graph, dem.target, blocked)
    pool = [
        w for w in range(graph.n_nodes)
        if w not in (dem.source, dem.target)
        and df0[w] != float("inf") and dt[w] != float("inf")
    ]
    if rev:
        pool.reverse()
    out, prev = [], dem.source
    for w in pool:
        if len(out) >= k:
            break
        if prev == dem.source:
            ok = df0[w] != float("inf")
        else:
            dp = distances_from(graph, prev, blocked)
            ok = dp[w] != float("inf")
        if ok:
            out.append(inst.node_ids[w])
            prev = w
    return out


def main():
    prefix = sys.argv[1]
    out_dir = sys.argv[2]
    seed = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    rng = __import__("random").Random(seed)

    inst = load_instance(f"{prefix}-net.json", f"{prefix}-tm.json", f"{prefix}-scenario.json")
    graph = DirectedGraph.build(inst)
    dems = inst.demands
    both = [d for d, dem in enumerate(dems) if dem.volume[0] > 0 and dem.volume[1] > 0]

    def chain_at(d, t, k, rev=False):
        c = chain(inst, graph, dems[d], t, k, rev)
        return c if len(c) >= k else None

    c1_t0 = {d: chain_at(d, 0, 1) for d in both}
    c1_t1 = {d: chain_at(d, 1, 1) for d in both}
    c2_t0 = {d: chain_at(d, 0, 2) for d in both}
    c2_t1 = {d: chain_at(d, 1, 2) for d in both}
    c2_t1r = {d: chain_at(d, 1, 2, rev=True) for d in both}

    def ids_with(pool, M):
        return [d for d in both if pool[d]][:M]

    M = 8
    scenarios = {}
    scenarios["empty_to_1wp"] = {d: {1: c1_t1[d]} for d in ids_with(c1_t1, M)}
    scenarios["empty_to_2wp"] = {d: {1: c2_t1[d]} for d in ids_with(c2_t1, M)}
    scenarios["1wp_to_empty"] = {d: {0: c1_t0[d]} for d in ids_with(c1_t0, M)}

    # 1-wp at t0 and 2-wp at t1 with the SAME head waypoint -> mask grows by 3.
    grow = [d for d in both if c1_t0[d] and c2_t1[d] and c2_t1[d][0] == c1_t0[d][0]]
    scenarios["1wp_to_2wp"] = {d: {0: c1_t0[d], 1: c2_t1[d]} for d in grow[:M]}

    # Same 2-wp chain at both slots (t1-feasible => t0-feasible): cost 0.
    scenarios["identical_2wp"] = {d: {0: c2_t1[d], 1: c2_t1[d]} for d in ids_with(c2_t1, M)}

    # 2-wp at both; roughly half reverse the t1 chain -> cost 6 vs 0.
    can_diff = [d for d in both if c2_t0[d] and c2_t1[d] and c2_t1r[d] and c2_t1r[d] != c2_t1[d]]
    scenarios["2wp_maydiff"] = {}
    for i, d in enumerate(can_diff[:M]):
        t1 = c2_t1r[d] if i % 2 == 0 else c2_t1[d]
        scenarios["2wp_maydiff"][d] = {0: c2_t0[d], 1: t1}

    # Boundary tests: exact budget vs one demand over.
    scenarios["boundary_17"] = {d: {1: c1_t1[d]} for d in ids_with(c1_t1, 17)}
    scenarios["boundary_18"] = {d: {1: c1_t1[d]} for d in ids_with(c1_t1, 18)}

    for name, changes in scenarios.items():
        entries = [{"d": d, "t": t, "w": changes[d][t]} for d in sorted(changes) for t in sorted(changes[d])]
        path = f"{out_dir}/{name}.json"
        with open(path, "w", encoding="utf-8") as fh:
            json.dump({"srpaths": entries}, fh, ensure_ascii=False, separators=(",", ":"))
        print(f"{name:18s} entries={len(entries):3d}")


if __name__ == "__main__":
    main()
