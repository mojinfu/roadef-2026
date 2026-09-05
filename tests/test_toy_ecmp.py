"""Ground-truth checks against the challenge subject's toy instance.

Table 2 gives split coefficients r(u, v, a, 0) for demands (v0,v5) and (v2,v5);
Figure 4 gives max loads for the trivial solution and the v0->v4->v5 solution.
"""
from __future__ import annotations

import numpy as np
import pytest

from tasr.ecmp import AtomCache, compute_atom
from tasr.eval import Evaluator
from tasr.model import DirectedGraph, Solution

from . import toy


def _endpoint_map(atom: np.ndarray, graph: DirectedGraph) -> dict:
    return {(a.frm, a.to): atom[a.id] for a in graph.arcs}


# Expected Table 2 values keyed by arc endpoints (from, to).
EXPECTED_D05 = {
    (0, 1): 1.0, (0, 2): 0.0, (1, 3): 0.5, (1, 4): 0.5,
    (2, 4): 0.0, (2, 6): 0.0, (3, 5): 0.75, (4, 3): 0.25,
    (4, 5): 0.0, (4, 6): 0.25, (6, 5): 0.25,
}
EXPECTED_D25 = {
    (0, 1): 0.0, (0, 2): 0.0, (1, 3): 0.0, (1, 4): 0.0,
    (2, 4): 1.0, (2, 6): 0.0, (3, 5): 0.5, (4, 3): 0.5,
    (4, 5): 0.0, (4, 6): 0.5, (6, 5): 0.5,
}


@pytest.mark.parametrize(
    "u,v,expected",
    [(0, 5, EXPECTED_D05), (2, 5, EXPECTED_D25)],
    ids=["d0-v0->v5", "d1-v2->v5"],
)
def test_split_coefficients_table2(u, v, expected):
    graph = toy.build_toy_graph()
    atom = compute_atom(graph, u, v, blocked=())  # t = 0, no intervention
    assert atom is not None
    assert atom.shape == (graph.m,)
    # Flow conservation: the unit leaves u and fully arrives at v.
    assert np.isclose(sum(atom[a.id] for a in graph.outs[u]), 1.0)
    assert np.isclose(sum(atom[a.id] for a in graph.ins[v]), 1.0)

    emap = _endpoint_map(atom, graph)
    for (endp, want) in expected.items():
        got = emap[endp]
        assert np.isclose(got, want, atol=1e-9), f"arc {endp}: r={got}, expected {want}"
    # All other arcs carry zero.
    for a in graph.arcs:
        if (a.frm, a.to) not in expected:
            assert emap[(a.frm, a.to)] == 0.0, f"arc {(a.frm, a.to)} should be 0"


def test_atom_under_intervention_t1():
    """At t=1 links 2 (1->3) and 3 (1->4) are down: v0 can no longer reach v5
    through v1; the ECMP path becomes v0->v2->v4->{v3,v6}->v5."""
    graph = toy.build_toy_graph()
    atom = compute_atom(graph, 0, 5, blocked={2, 3})
    assert atom is not None
    emap = _endpoint_map(atom, graph)
    # Flow conservation: unit leaves v0 and fully arrives at v5.
    assert np.isclose(sum(atom[a.id] for a in graph.outs[0]), 1.0)
    assert np.isclose(sum(atom[a.id] for a in graph.ins[5]), 1.0)
    assert np.isclose(emap[(0, 1)], 0.0)  # v1 is a dead end at t=1
    assert np.isclose(emap[(0, 2)], 1.0)
    assert np.isclose(emap[(4, 3)], 0.5)
    assert np.isclose(emap[(4, 6)], 0.5)


def test_trivial_solution_max_load_figure4():
    inst = toy.build_toy_instance()
    graph = DirectedGraph.build(inst)
    cache = AtomCache(graph, inst.scenario.interventions)
    ev = Evaluator(inst, graph, cache)

    sat = ev.saturations(toy.baseline_solution())
    assert sat.shape == (graph.m, inst.n_slots)
    # Worked example in the subject: load(a4,6, 0) = 0.3125
    assert np.isclose(sat[9, 0], 0.3125, atol=1e-9)  # arc 4->6 is id 9
    # Figure 4 (left): highest load of the trivial solution is 0.4375 on arc 3->5 at t=0
    assert np.isclose(sat.max(), 0.4375, atol=1e-9)
    assert np.isclose(sat[6, 0], 0.4375, atol=1e-9)  # arc 3->5 is id 6


def test_waypoint_solution_lowers_max_load_figure4():
    inst = toy.build_toy_instance()
    graph = DirectedGraph.build(inst)
    cache = AtomCache(graph, inst.scenario.interventions)
    ev = Evaluator(inst, graph, cache)

    sat = ev.saturations(toy.waypoint_solution())
    # Figure 4 (right): using segment path v0,v4,v5 at t=0 lowers the max to 0.3750.
    assert np.isclose(sat.max(), 0.3750, atol=1e-9)


def test_unreachable_returns_none():
    graph = toy.build_toy_graph()
    # Blocking arcs {2,3} (out of v1) still leaves v0->v2->v4->... reachable.
    assert compute_atom(graph, 0, 5, blocked={2, 3}) is not None
    # Blocking {2,3} plus node v2's out-arcs {4,5} isolates v0 entirely.
    assert compute_atom(graph, 0, 5, blocked={2, 3, 4, 5}) is None
    # Disconnect v0 from everything: block arcs 0->1 and 0->2.
    assert compute_atom(graph, 0, 5, blocked={0, 1}) is None
