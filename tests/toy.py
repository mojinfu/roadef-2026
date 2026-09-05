"""Build the 'toy' instance of the challenge subject (§4-§5) as Python objects.

The subject gives the exact JSON of ``toy-net.json`` / ``toy-tm.json`` /
``toy-scenario.json``; reproducing it in code gives us a self-contained ground
truth for the ECMP and evaluation tests (Table 2 / Figure 4).
"""
from __future__ import annotations

from tasr.model import Arc, Demand, DirectedGraph, Instance, Scenario, Solution

NODE_NAMES = ["A", "B", "C", "D", "E", "F", "G"]  # v0..v6

# id, from, to, metric, capacity   (from subject toy-net.json)
_ARCS = [
    (0, 0, 1, 100, 400),
    (1, 0, 2, 200, 300),
    (2, 1, 3, 200, 200),
    (3, 1, 4, 100, 200),
    (4, 2, 4, 100, 400),
    (5, 2, 6, 300, 200),
    (6, 3, 5, 100, 200),
    (7, 3, 4, 100, 200),
    (8, 4, 5, 300, 200),
    (9, 4, 6, 100, 200),
    (10, 5, 6, 100, 200),
    (11, 1, 0, 100, 400),
    (12, 2, 0, 200, 300),
    (13, 3, 1, 200, 200),
    (14, 4, 1, 100, 200),
    (15, 4, 2, 100, 400),
    (16, 6, 2, 300, 200),
    (17, 5, 3, 100, 200),
    (18, 4, 3, 100, 200),
    (19, 5, 4, 300, 200),
    (20, 6, 4, 100, 200),
    (21, 6, 5, 100, 200),
]


def arcs():
    return tuple(Arc(id=i, frm=f, to=t, metric=float(m), capacity=float(c)) for (i, f, t, m, c) in _ARCS)


def build_toy_instance() -> Instance:
    demands = (
        Demand(source=0, target=5, volume=(50.0, 100.0)),  # d0 = (v0, v5)
        Demand(source=2, target=5, volume=(100.0, 50.0)),  # d1 = (v2, v5)
    )
    scenario = Scenario(
        max_segments=4,
        budget=(0, 20),
        interventions=(frozenset(), frozenset({2, 3})),  # t=1: links 1->3 and 1->4 down
    )
    return Instance(
        name="toy",
        node_names=NODE_NAMES,
        arcs=arcs(),
        n_slots=2,
        demands=demands,
        scenario=scenario,
    )


def build_toy_graph() -> DirectedGraph:
    return DirectedGraph(NODE_NAMES, arcs())


def baseline_solution() -> Solution:
    return Solution.empty(2, 2)


def waypoint_solution() -> Solution:
    """d0 at t=0 uses waypoint v4 (id 4); everything else stays empty."""
    sol = Solution.empty(2, 2)
    sol.set_waypoints(0, 0, (4,))
    return sol
