"""Synthesise a feasible multi-waypoint solution for parity testing.

For every (demand, slot) with positive volume picks one intermediate node that
is connected to both endpoints once the slot's down arcs are removed, so every
segment is ECMP-routable.  Written in the official srpaths format (network-file
node ids).
"""
import sys
sys.path.insert(0, __file__.rsplit("/", 1)[0] if "/" in __file__ else ".")

import json

from tasr.io.parser import load_instance
from tasr.model.graph import DirectedGraph
from tasr.ecmp import distances_from, distances_to


def main():
    prefix = sys.argv[1]
    out_path = sys.argv[2]
    k = int(sys.argv[3]) if len(sys.argv) > 3 else 1  # waypoints per pair

    inst = load_instance(f"{prefix}-net.json", f"{prefix}-tm.json", f"{prefix}-scenario.json")
    graph = DirectedGraph.build(inst)
    entries = []
    for d, dem in enumerate(inst.demands):
        for t in range(inst.n_slots):
            if dem.volume[t] == 0.0:
                continue
            blocked = inst.scenario.interventions[t]
            df = distances_from(graph, dem.source, blocked)
            dt = distances_to(graph, dem.target, blocked)
            mids = []
            for w in range(graph.n_nodes):
                if w == dem.source or w == dem.target:
                    continue
                if w in mids:
                    continue
                import math
                if not math.isinf(df[w]) and not math.isinf(dt[w]):
                    mids.append(inst.node_ids[w])
                if len(mids) == k:
                    break
            if mids:
                entries.append({"d": d, "t": t, "w": mids})

    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump({"srpaths": entries}, fh, ensure_ascii=False, separators=(",", ":"))
    print(f"wrote {len(entries)} entries -> {out_path}")


if __name__ == "__main__":
    main()
