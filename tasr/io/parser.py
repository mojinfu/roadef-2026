"""Read the three input JSON files of a T-ASR instance into :mod:`tasr.model`.

File formats are described in the challenge subject, section 5:
  * ``*-net.json``      : NetworkX-compatible directed graph (nodes + links)
  * ``*-tm.json``       : ``num_time_slots`` + ``demands`` list
  * ``*-scenario.json`` : ``max_segments`` + ``budget`` + ``interventions``
"""
from __future__ import annotations

import json
import os
from typing import Sequence

from ..model import Arc, Demand, Instance, Scenario

# ---------------------------------------------------------------------------


def _load_json(path: str):
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def load_network_file(net_path: str):
    """Parse a ``*-net.json`` file.

    Returns ``(node_names, arcs)`` where ``node_names[i]`` is the label of node
    ``i`` and ``arcs`` is a list of :class:`Arc` in id order.
    """
    doc = _load_json(net_path)

    if "nodes" not in doc or "links" not in doc:
        raise ValueError(f"{net_path}: missing 'nodes' or 'links' section")

    node_names: Sequence[str] = []
    id_to_pos = {}
    for nd in doc["nodes"]:
        nid = int(nd["id"])
        if nid < 0:
            raise ValueError(f"{net_path}: negative node id {nid}")
        if nid in id_to_pos:
            raise ValueError(f"{net_path}: duplicate node id {nid}")
        id_to_pos[nid] = len(node_names)
        node_names.append(str(nd.get("name", str(nid))))
    n_nodes = len(node_names)

    arcs = []
    for ln in doc["links"]:
        aid = int(ln["id"])
        frm = int(ln["from"])
        to = int(ln["to"])
        if aid != len(arcs):
            raise ValueError(f"{net_path}: links must be listed with contiguous ids (got {aid})")
        if frm not in id_to_pos or to not in id_to_pos:
            raise ValueError(f"{net_path}: link {aid} references unknown node")
        arcs.append(
            Arc(
                id=aid,
                frm=id_to_pos[frm],
                to=id_to_pos[to],
                metric=float(ln["metric"]),
                capacity=float(ln["capacity"]),
            )
        )

    if not arcs:
        raise ValueError(f"{net_path}: empty network")
    if n_nodes == 0:
        raise ValueError(f"{net_path}: no nodes")
    return node_names, arcs


def load_traffic_matrix_file(tm_path: str):
    """Parse a ``*-tm.json`` file.

    Returns ``(n_slots, demands)``.  Demand id == its position in the array.
    """
    doc = _load_json(tm_path)

    n_slots = int(doc["num_time_slots"])
    if n_slots < 1:
        raise ValueError(f"{tm_path}: num_time_slots must be >= 1")

    demands = []
    for obj in doc["demands"]:
        v = obj["v"]
        if len(v) != n_slots:
            raise ValueError(
                f"{tm_path}: demand has {len(v)} volumes but num_time_slots={n_slots}"
            )
        demands.append(
            Demand(source=int(obj["s"]), target=int(obj["t"]), volume=tuple(float(x) for x in v))
        )
    return n_slots, demands


def load_scenario_file(scenario_path: str, n_slots: int):
    """Parse a ``*-scenario.json`` file into a :class:`Scenario`.

    Time step 0 never appears in the file (nominal situation).  Budget for a
    missing step defaults to 0.
    """
    doc = _load_json(scenario_path)

    max_segments = int(doc["max_segments"])
    if max_segments < 0:
        raise ValueError(f"{scenario_path}: max_segments must be >= 0")

    budget = [0] * n_slots
    for b in doc.get("budget", []):
        t = int(b["t"])
        if not (1 <= t < n_slots):
            raise ValueError(f"{scenario_path}: budget time step {t} out of range [1,{n_slots-1}]")
        budget[t] = int(b["value"])

    interventions: list = [frozenset() for _ in range(n_slots)]
    for iv in doc.get("interventions", []):
        t = int(iv["t"])
        if not (1 <= t < n_slots):
            raise ValueError(f"{scenario_path}: intervention time step {t} out of range")
        interventions[t] = frozenset(int(x) for x in iv["links"])

    return Scenario(
        max_segments=max_segments, budget=tuple(budget), interventions=tuple(interventions)
    )


def load_instance(net_path: str, tm_path: str, scenario_path: str, name: str | None = None) -> Instance:
    """Load the three files of an instance and cross-check the references."""
    node_names, arcs = load_network_file(net_path)
    n_slots, demands = load_traffic_matrix_file(tm_path)
    scenario = load_scenario_file(scenario_path, n_slots)

    node_pos = {i for i in range(len(node_names))}
    for i, d in enumerate(demands):
        if d.source not in node_pos or d.target not in node_pos:
            raise ValueError(f"{tm_path}: demand {i} references unknown node")
        if d.source == d.target:
            raise ValueError(f"{tm_path}: demand {i} is degenerate (s == t)")

    max_arc_id = len(arcs)
    for t in range(n_slots):
        for aid in scenario.interventions[t]:
            if aid < 0 or aid >= max_arc_id:
                raise ValueError(
                    f"{scenario_path}: intervention link {aid} at t={t} out of range"
                )

    if name is None:
        base = os.path.basename(net_path)
        name = base[: -len("-net.json")] if base.endswith("-net.json") else base

    return Instance(
        name=name,
        node_names=tuple(node_names),
        arcs=tuple(arcs),
        n_slots=n_slots,
        demands=tuple(demands),
        scenario=scenario,
    )
