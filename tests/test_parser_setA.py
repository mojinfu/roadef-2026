"""Parser smoke test on the real setA-01 instance files."""
from __future__ import annotations

from pathlib import Path

import pytest

from tasr.io import load_instance
from tasr.model import DirectedGraph

REPO = Path(__file__).resolve().parents[1]
PREFIX = REPO / "setA" / "setA-01"


def test_load_setA01():
    inst = load_instance(f"{PREFIX}-net.json", f"{PREFIX}-tm.json", f"{PREFIX}-scenario.json")
    assert inst.name == "setA-01"
    assert inst.n_slots == 2
    assert inst.n_nodes == 20
    assert inst.n_arcs == 80
    assert inst.n_demands >= 1
    for d in inst.demands:
        assert len(d.volume) == 2
    assert inst.scenario.budget[1] == 51
    assert inst.scenario.interventions[1] == frozenset({22})
    assert inst.scenario.interventions[0] == frozenset()


def test_graph_build():
    inst = load_instance(f"{PREFIX}-net.json", f"{PREFIX}-tm.json", f"{PREFIX}-scenario.json")
    g = DirectedGraph.build(inst)
    assert g.m == inst.n_arcs
    assert sum(len(o) for o in g.outs) == g.m
    assert sum(len(i) for i in g.ins) == g.m
    # every node reachable both ways on the base graph (bidirected instance)
    for node in range(g.n_nodes):
        assert len(g.outs[node]) >= 1 or g.n_nodes == 1


def test_demand_references_exist():
    inst = load_instance(f"{PREFIX}-net.json", f"{PREFIX}-tm.json", f"{PREFIX}-scenario.json")
    for d in inst.demands:
        assert 0 <= d.source < inst.n_nodes
        assert 0 <= d.target < inst.n_nodes
        assert d.source != d.target


REV12 = REPO / "setA" / "setA-12"


def test_reversed_node_ids_setA12():
    """setA-12 lists its 200 nodes in reverse id order (199..0).

    Demand endpoints in the file are node *ids*; internally they must be
    positions.  This regression guards against the position/id confusion that
    silently routed every demand between wrong nodes on such files.
    """
    inst = load_instance(f"{REV12}-net.json", f"{REV12}-tm.json", f"{REV12}-scenario.json")
    assert inst.n_nodes == 200
    assert inst.node_ids[0] == 199 and inst.node_ids[199] == 0
    assert inst.node_index[199] == 0 and inst.node_index[0] == 199

    # first file demand is {'v':[0.2538,0.0363],'s':106,'t':178}
    d0 = inst.demands[0]
    assert d0.source == 199 - 106  # position of id 106
    assert d0.target == 199 - 178  # position of id 178


def test_writer_roundtrip_reversed_ids():
    from tasr.io import load_solution_from_dict, solution_to_dict
    from tasr.model import Solution

    inst = load_instance(f"{REV12}-net.json", f"{REV12}-tm.json", f"{REV12}-scenario.json")
    sol = Solution.empty(inst.n_demands, inst.n_slots)
    # put a waypoint on the position of file-id 106 for demand 0 slot 0
    pos = inst.node_index[106]
    sol.set_waypoints(0, 0, (pos,))
    doc = solution_to_dict(sol, inst)
    entry = next(e for e in doc["srpaths"] if e["d"] == 0 and e["t"] == 0)
    assert entry["w"] == [106]  # file speaks ids
    back = load_solution_from_dict(doc, inst)
    assert back.waypoints[0][0] == (pos,)
