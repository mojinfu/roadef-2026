/*
 * Software Name : EURO/ROADEF Challenge 2026 checker
 * SPDX-FileCopyrightText: Copyright (c) Orange SA
 * SPDX-License-Identifier: MIT
 *
 * This software is distributed under the MIT licence,
 * see the "LICENSE" file for more details or https://opensource.org/license/MIT
 *
 * Authors: see CONTRIBUTORS.md
 * Software description: checker for the EURO/ROADEF Challenge 2026
 */

/**
 * @file types.h
 * @brief Type definitions and data structures for the EURO/ROADEF Challenge 2026 checker.
 * @author Morgan Chopin (morgan.chopin@orange.com)
 * 
 * This file contains the core type definitions used throughout the checker code,
 * including graph representations, segment routing paths, routing schemes, scenarios,
 * and problem instances.
 */

#ifndef _CHECKER_TYPES_H_
#define _CHECKER_TYPES_H_

#include "graphs/list_graph.h"
#include "graphs/demand_graph.h"
#include "te/algorithms/segment_routing.h"
#include "graphs/io/networkx_io.h"


/**
 * @typedef Digraph
 * @brief Directed graph type
 */
using Digraph = nt::graphs::ListDigraph;
DIGRAPH_TYPEDEFS(Digraph);

/**
 * @typedef DemandGraph
 * @brief Demand graph structure representing traffic demands over the network topology.
 */
using DemandGraph = nt::graphs::DemandGraph< Digraph >;

/**
 * @typedef DemandArc
 * @brief Arc type in the demand graph representing a single demand.
 */
using DemandArc = DemandGraph::Arc;

/**
 * @typedef DemandArcIt
 * @brief Iterator for traversing demand arcs in the demand graph.
 */
using DemandArcIt = DemandGraph::ArcIt;

/**
 * @typedef SrPath
 * @brief Segment routing path representation in the network digraph.
 */
using SrPath = nt::te::SrPath< Digraph >;

/**
 * @typedef SegmentRouting
 * @brief Segment routing implementation with double-precision capacity and metric values.
 */
using SegmentRouting = nt::te::SegmentRouting< Digraph, double, double >;

/**
 * @typedef CapacityMap
 * @brief Arc map storing capacity values for network links.
 */
using CapacityMap = typename Digraph::template ArcMap< double >;

/**
 * @typedef MetricMap
 * @brief Arc map storing routing metric values for network links.
 */
using MetricMap = typename Digraph::template ArcMap< double >;

/**
 * @typedef DelayMap
 * @brief Arc map storing delay values for network links.
 */
using DelayMap = typename Digraph::template ArcMap< double >;

/**
 * @typedef NameMap
 * @brief Node map storing string names for network nodes.
 */
using NameMap = typename Digraph::template NodeMap< nt::String >;

/**
 * @typedef DemandValueMap
 * @brief Arc map storing traffic volume values for demands.
 */
using DemandValueMap = typename DemandGraph::template ArcMap< double >;

/**
 * @brief Extension of SrPath that maintains a bitmask representation of traversed segments.
 *
 * SrPathBit is used to efficiently compare segment routing paths by storing a bitmask
 * indicating which node-to-node segments are traversed in the path. This enables fast
 * computation of path differences (e.g., via Hamming distance) for use in constraints
 * such as budget checks between consecutive routing schemes.
 */
struct SrPathBit: SrPath {
  const Digraph* _g;

  SrPathBit() : _g(nullptr) {}

  // Move constructor
  SrPathBit(SrPathBit&& other) noexcept : SrPath(std::move(other)), _g(other._g) { other._g = nullptr; }

  // Move assignment
  SrPathBit& operator=(SrPathBit&& other) noexcept {
    if (this != &other) {
      SrPath::operator=(std::move(other));
      _g = other._g;
      other._g = nullptr;
    }
    return *this;
  }

  // Delete copy operations since BitArray has deleted copy
  SrPathBit(const SrPathBit&) = delete;
  SrPathBit& operator=(const SrPathBit&) = delete;

  void init(const Digraph& g, int n) noexcept {
    clear();
    _g = &g;
    reserve(n);
  }

  void init(const Digraph& g, const SrPath& srpath) noexcept {
    clear();
    SrPath::copyFrom(srpath);
    _g = &g;
  }

  void init(const Digraph& g, const DemandGraph& dg, DemandArc demand_arc) noexcept {
    clear();
    _g = &g;
    addSegment(dg.source(demand_arc));
    finalize(dg.target(demand_arc));
  }

  void addSegment(Node node, Node last) noexcept {
    SrPath::addSegment(node);
  }

  void addSegment(Node node) noexcept { SrPath::addSegment(node); }

  void finalize(Node target) noexcept {
    const Node last = SrPath::back().toNode();
    SrPath::addSegment(target);
  }

  void clear() noexcept {
    SrPath::clear();
    _g = nullptr;
  }

  void copyFrom(const SrPathBit& other) noexcept {
    clear();
    SrPath::copyFrom(other);
    _g = other._g;
  }
};

/**
 * @struct Scenario
 * @brief Represents a challenge scenario with budgets and maintenance interventions.
 *
 * Contains the operational constraints and maintenance schedule that must be
 * satisfied by a routing solution.
 *
 * @var Scenario::budget
 * Array of budget limits for each time slot, representing the maximum number
 * of routing changes allowed between consecutive time slots.
 *
 * @var Scenario::interventions
 * Two-dimensional array representing network interventions (maintenance events)
 * scheduled at specific time slots and affecting specific network elements.
 *
 * @var Scenario::i_max_segments
 * Maximum number of segments allowed in any segment routing path. Value of -1
 * indicates uninitialized or unlimited segments.
 */
struct Scenario {
  nt::IntDynamicArray                     budget;
  nt::DynamicArray< nt::IntDynamicArray > interventions;
  int                                     i_max_segments;
  Scenario() : i_max_segments(-1) {}
};

/**
 * @struct Instance
 * @brief Represents a complete problem instance including network topology and demands.
 *
 * Encapsulates all static and dynamic data defining the optimization problem:
 * the physical network, its characteristics (capacity, metrics, delays), traffic
 * demands over time, and temporal discretization.
 *
 * @var Instance::network
 * The underlying directed graph representing the network topology.
 *
 * @var Instance::capacities
 * Arc map storing the bandwidth capacity of each network link.
 *
 * @var Instance::metrics
 * Arc map storing the routing metric (e.g., IGP weight) of each network link.
 *
 * @var Instance::delays
 * Arc map storing the propagation delay of each network link.
 *
 * @var Instance::names
 * Node map storing human-readable identifiers for network nodes.
 *
 * @var Instance::demand_graph
 * Demand graph representing all traffic demands in the network.
 *
 * @var Instance::dvms
 * Dynamic array of demand value maps, one per time slot, storing traffic
 * volumes for each demand at each time slot.
 *
 * @var Instance::i_num_time_slots
 * Total number of time slots in the planning horizon. Value of -1 indicates
 * uninitialized instance.
 *
 * @var Instance::i_max_segments
 * Maximum number of segments allowed in segment routing paths (inherited from scenario).
 * Value of -1 indicates uninitialized or unlimited segments.
 */
struct Instance {
  using NetworkxIo = nt::graphs::NetworkxIo< Digraph >;
  using NodeHashMap = typename Digraph::template NodeMap< uint64_t >;

  Digraph     network;
  NetworkxIo  reader;
  NodeHashMap node_hashes;
  CapacityMap capacities;
  MetricMap   metrics;
  DelayMap    delays;
  NameMap     names;

  DemandGraph                        demand_graph;
  nt::DynamicArray< DemandValueMap > dvms;

  int i_num_time_slots;

  Instance() : reader(NET_SRC_ATTR, NET_DST_ATTR), node_hashes(network), capacities(network), metrics(network), delays(network), names(network), demand_graph(network), i_num_time_slots(-1) {}
};

/**
 * @struct RoutingScheme
 * @brief Represents a complete routing configuration across all time slots.
 *
 * Stores segment routing paths for each demand at each time slot. The structure
 * is organized as _td_srpath[time][demand_arc] for efficient time-based lookups.
 *
 * @var RoutingScheme::_dg
 * Reference to the demand graph this routing scheme operates on.
 *
 * @var RoutingScheme::_td_srpath
 * Time-indexed array of demand arc maps containing segment routing paths with bitmasks.
 * Indexed as [time_slot][demand_arc] to retrieve the SrPathBit for a specific demand at a specific time.
 */
struct RoutingScheme {
  nt::DynamicArray< DemandGraph::ArcMap< SrPathBit > > _td_srpath;   // RoutingScheme[time][demand_arc] = SrPathBit

  RoutingScheme(const Instance& inst) { _td_srpath.ensureAndFillEmplace(inst.i_num_time_slots, inst.demand_graph); }

  SrPathBit&       getSrPath(int time_slot, DemandGraph::Arc demand_arc) noexcept { return _td_srpath[time_slot][demand_arc]; }
  const SrPathBit& getSrPath(int time_slot, DemandGraph::Arc demand_arc) const noexcept { return _td_srpath[time_slot][demand_arc]; }

  void                                    setSrPathsAt(int time_slot, DemandGraph::ArcMap< SrPathBit >&& srpathbit) noexcept { _td_srpath[time_slot] = std::move(srpathbit); }
  DemandGraph::ArcMap< SrPathBit >&       getSrPathsAt(int time_slot) noexcept { return _td_srpath[time_slot]; }
  const DemandGraph::ArcMap< SrPathBit >& getSrPathsAt(int time_slot) const noexcept { return _td_srpath[time_slot]; }
};

#endif
