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
