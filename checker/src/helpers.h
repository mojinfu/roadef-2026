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
 * @file helpers.h
 * @brief Helper functions for the EURO/ROADEF Challenge 2026 checker.
 *
 * @author Morgan Chopin (morgan.chopin@orange.com)
 */

#ifndef _CHECKER_HELPERS_H_
#define _CHECKER_HELPERS_H_

#include <string>
#include <sstream>
#include <iomanip>
#include <algorithm>

#ifdef ENABLE_FTXUI
#  include "ftxui/dom/elements.hpp"
#  include "ftxui/screen/screen.hpp"
#endif

#include "config.h"
#include "types.h"
#include "core/json.h"
#include "graphs/tools.h"

// Forward declarations
struct ResultBuilder;
struct InterventionGuard;

// ============================================================================
// JSON Helper Functions
// ============================================================================

/**
 * @brief Validates and parses a JSON file.
 *
 * @param filepath Path to the JSON file to parse
 * @param doc Reference to the JSON document to populate
 * @return true if parsing was successful, false otherwise
 */
inline bool parseJsonFile(const char* filepath, nt::JSONDocument& doc) noexcept {
  if (!doc.ParseFile(filepath)) {
    LOG_F(ERROR, ERR_MSG_15, static_cast< int >(doc.GetParseError()), doc.GetErrorOffset());
    return false;
  }

  if (!doc.IsObject()) {
    LOG_F(ERROR, ERR_MSG_16);
    return false;
  }

  return true;
}

/**
 * @brief Retrieves an integer member from a JSON document/object with validation.
 *
 * @tparam T The JSON type (JSONDocument or JsonObject)
 * @param obj The JSON document/object to search
 * @param member_name Name of the member to find
 * @param out_value Reference to store the extracted integer value
 * @return true if member was found and is an integer, false otherwise
 */
template < typename T >
bool getIntMember(const T& obj, const char* member_name, int& out_value) noexcept {
  const auto iterator = obj.FindMember(member_name);
  if (iterator == obj.MemberEnd() || !iterator->value.IsInt()) {
    LOG_F(ERROR, ERR_MSG_17, member_name);
    return false;
  }
  out_value = iterator->value.GetInt();
  return true;
}

/**
 * @brief Retrieves an array member from a JSON document/object with validation.
 *
 * @tparam T The JSON type (JSONDocument or JsonObject)
 * @param obj The JSON document/object to search
 * @param member_name Name of the member to find
 * @return true if member was found and is an array, false otherwise
 */
template < typename T >
bool hasArrayMember(const T& obj, const char* member_name) noexcept {
  const auto iterator = obj.FindMember(member_name);
  if (iterator == obj.MemberEnd() || !iterator->value.IsArray()) {
    LOG_F(ERROR, ERR_MSG_18, member_name);
    return false;
  }
  return true;
}

/**
 * @brief Validates that an integer value is within a specified range.
 *
 * @param value The value to check
 * @param min_value Minimum allowed value (inclusive)
 * @param max_value Maximum allowed value (exclusive)
 * @param value_name Name of the value for error messages
 * @return true if value is in range, false otherwise
 */
inline bool validateRange(int value, int min_value, int max_value, const char* value_name) noexcept {
  if (value < min_value || value >= max_value) {
    LOG_F(ERROR, ERR_MSG_19, value_name, value, min_value, max_value);
    return false;
  }
  return true;
}

/**
 * @brief Validates whether an ID is within the valid range for a specific item type in a graph.
 *
 * This function checks if the given ID is non-negative and less than the total count
 * of items of the specified type in the graph.
 *
 * @tparam Graph The type of the graph structure
 * @tparam Item The type of item to validate against (e.g., Node, Arc, Edge)
 * @param g The graph instance to validate against
 * @param id The ID to validate
 * @return true if the ID is within valid bounds [0, count), false otherwise
 */
template < typename Graph, typename Item >
inline bool validateId(const Graph& g, Item item, const char* value_name) noexcept {
  return validateRange(g.id(item), 0, nt::graphs::countItems< Graph, Item >(g), value_name);
}

namespace details {
  inline void computeSrPathMask(const SrPathBit& srpathbit, nt::BitArray& mask) {
    assert(srpathbit._g);
    const Digraph& g = *srpathbit._g;

    const int n = g.nodeNum();
    mask.removeAll();
    mask.extendByBits(n * n);

    for (int i = 0; i < srpathbit.segmentNum() - 1; ++i) {
      const Node from = srpathbit[i].toNode();
      const Node to = srpathbit[i + 1].toNode();
      mask.setOneAt(g.id(from) * n + g.id(to));
    }
  }

  inline int distEx(const SrPathBit& srpathbit_a, const SrPathBit& srpathbit_b, nt::BitArray& mask_a, nt::BitArray& mask_b) {
    if (srpathbit_a.empty()) return srpathbit_b.segmentNum();
    if (srpathbit_b.empty()) return srpathbit_a.segmentNum();
    computeSrPathMask(srpathbit_a, mask_a);
    computeSrPathMask(srpathbit_b, mask_b);
    return mask_a.hamming(mask_b);
  }
}   // namespace details

/**
 * @brief Computes the distance between two SrPathBit objects.
 *
 * If either path is empty, returns the number of segments in the other path plus one.
 * Otherwise, returns the Hamming distance between the bitmasks of the two paths.
 *
 * @param srpathbit_a First SrPathBit object.
 * @param srpathbit_b Second SrPathBit object.
 * @return int The distance between the two paths. If one path is empty, returns the number of segments in the other path.
 */
inline int dist(const SrPathBit& srpathbit_a, const SrPathBit& srpathbit_b) {
  if (srpathbit_a.empty()) return srpathbit_b.segmentNum();
  if (srpathbit_b.empty()) return srpathbit_a.segmentNum();
  nt::BitArray mask_a;
  nt::BitArray mask_b;
  return details::distEx(srpathbit_a, srpathbit_b, mask_a, mask_b);
}

/**
 * @brief Checks the budget constraint at a specific time slot.
 *
 * @param rs The routing scheme to check.
 * @param inst The instance containing network and demand information.
 * @param scenario The scenario containing budget constraints.
 * @param t The time slot
 * @param i_cost The cost at the time slot
 * @return true if the budget constraint is satisfied, false otherwise
 */
inline bool checkBudgetConstraintAt(const RoutingScheme& rs, const Instance& inst, const Scenario& scenario, int t, int& i_cost) {
  i_cost = 0;
  if (t < 1) return true;
  const int    i_budget = scenario.budget[t];
  nt::BitArray mask_a;
  nt::BitArray mask_b;
  for (DemandArcIt demand_arc(inst.demand_graph); demand_arc != nt::INVALID; ++demand_arc) {
    i_cost += details::distEx(rs.getSrPath(t - 1, demand_arc), rs.getSrPath(t, demand_arc), mask_a, mask_b);
    if (i_cost > i_budget) {
      LOG_F(ERROR, ERR_MSG_4, i_cost, i_budget, t);
      return false;
    }
  }
  return true;
}

/**
 * @brief Checks the budget constraint for the routing scheme.
 *
 * For each demand and time slot, verifies that the distance between consecutive SR paths
 * does not exceed the allowed budget. If the constraint is violated, logs an error and returns false.
 *
 * @param rs The routing scheme to check.
 * @param inst The instance containing network and demand information.
 * @param scenario The scenario containing budget constraints.
 * @return true if all constraints are satisfied, false otherwise (with error logged).
 */
inline bool checkBudgetConstraint(const RoutingScheme& rs, const Instance& inst, const Scenario& scenario, int& i_total_cost) noexcept {
  i_total_cost = 0;
  for (int t = 1; t < inst.i_num_time_slots; ++t) {
    int i_cost = 0;
    if (!checkBudgetConstraintAt(rs, inst, scenario, t, i_cost)) return false;
    i_total_cost += i_cost;
  }
  return true;
}

#ifdef ENABLE_FTXUI
namespace ftxui_helpers {

  using namespace ftxui;
  using std::string;

  // Helper function to create text safely
  inline Element txt(const string& s) { return ftxui::text(s); }

  inline Element OrangeLogo() {
    Elements logo;
    logo.push_back(txt("██████") | color(Color::RGB(255, 140, 0)) | bold);
    logo.push_back(txt("██████") | color(Color::RGB(255, 140, 0)) | bold);
    logo.push_back(txt("██████") | color(Color::RGB(255, 140, 0)) | bold);
    return vbox(logo);
  }

  /**
   * @brief Creates a styled header element with Orange logo
   */
  inline Element Header(const string& title, const string& subtitle = "", bool b_display_logo = false) {
    Elements logo_and_title;
    if (b_display_logo) {
      logo_and_title.push_back(OrangeLogo());
      logo_and_title.push_back(txt("  "));
    }

    Elements title_block;
    title_block.push_back(txt(title) | bold | color(Color::Yellow));
    if (!subtitle.empty()) {
      // title_block.push_back(txt("  "));
      // Split subtitle by newlines and create separate elements
      std::istringstream ss(subtitle);
      std::string        line;
      while (std::getline(ss, line)) {
        title_block.push_back(txt(line) | color(Color::Cyan));
      }
    }

    logo_and_title.push_back(vbox(title_block));

    Elements full_header;
    full_header.push_back(txt(""));
    full_header.push_back(hbox(logo_and_title));
    full_header.push_back(txt(""));

    return vbox(full_header);
  }

  /**
   * @brief Creates a section title element
   */
  inline Element SectionTitle(const string& title) {
    Elements content;
    content.push_back(txt("> ") | color(Color::GreenLight) | bold);
    content.push_back(txt(title) | bold);
    return hbox(content);
  }

  /**
   * @brief Creates a success/error indicator
   */
  inline Element StatusIndicator(bool success, const string& message) {
    Elements content;
    if (success) {
      content.push_back(txt("[OK] ") | color(Color::GreenLight) | bold);
      content.push_back(txt(message));
    } else {
      content.push_back(txt("[ERROR] ") | color(Color::RedLight) | bold);
      content.push_back(txt(message));
    }
    return hbox(content);
  }

  /**
   * @brief Creates a key-value pair element
   */
  inline Element KeyValue(const string& key, const string& value) {
    Elements content;
    content.push_back(txt(key + ": ") | color(Color::Cyan));
    content.push_back(txt(value));
    return hbox(content);
  }

  /**
   * @brief Creates an ASCII bar chart for MLU distribution
   */
  inline Element createBarChart(const nt::DoubleDynamicArray& mlu_values, int max_width = 50) {
    if (mlu_values.empty()) { return txt("No MLU data available"); }

    double max_mlu = *std::max_element(mlu_values.begin(), mlu_values.end());
    if (max_mlu == 0.0) max_mlu = 1.0;

    Elements bars;
    bars.push_back(txt("MLU Distribution:") | bold | color(Color::Cyan));
    bars.push_back(separator());

    for (size_t t = 0; t < mlu_values.size(); ++t) {
      double mlu = mlu_values[t];
      int    bar_length = static_cast< int >((mlu / max_mlu) * max_width);
      bar_length = std::max(0, std::min(max_width, bar_length));

      std::ostringstream label;
      label << "t" << std::setw(2) << std::setfill('0') << t << " ";

      string bar(bar_length, '#');
      string space(max_width - bar_length, ' ');

      Color bar_color = Color::GreenLight;
      if (mlu > 0.9)
        bar_color = Color::RedLight;
      else if (mlu > 0.7)
        bar_color = Color::Yellow;

      std::ostringstream value_str;
      value_str << std::fixed << std::setprecision(4) << mlu;

      bars.push_back(hbox({txt(label.str()), txt(bar) | color(bar_color), txt(space), txt(" " + value_str.str())}));
    }

    return vbox(bars);
  }

  /**
   * @brief Creates a summary statistics box
   */
  inline Element createStatistics(const nt::DoubleDynamicArray& mlu_values, int total_srpaths, int total_segments, int total_cost) {
    Elements stats;
    stats.push_back(txt("Statistics:") | bold | color(Color::Cyan));
    stats.push_back(separator());

    if (!mlu_values.empty()) {
      double min_mlu = *std::min_element(mlu_values.begin(), mlu_values.end());
      double max_mlu = *std::max_element(mlu_values.begin(), mlu_values.end());
      double avg_mlu = 0.0;
      for (double mlu: mlu_values)
        avg_mlu += mlu;
      avg_mlu /= mlu_values.size();

      std::ostringstream oss;
      oss << std::fixed << std::setprecision(4) << min_mlu;
      stats.push_back(KeyValue("Min MLU", oss.str()));

      oss.str("");
      oss << std::fixed << std::setprecision(4) << max_mlu;
      stats.push_back(KeyValue("Max MLU", oss.str()));

      oss.str("");
      oss << std::fixed << std::setprecision(4) << avg_mlu;
      stats.push_back(KeyValue("Avg MLU", oss.str()));
      stats.push_back(txt(""));
    }

    stats.push_back(KeyValue("SR Paths", std::to_string(total_srpaths)));
    stats.push_back(KeyValue("Segments", std::to_string(total_segments)));
    stats.push_back(KeyValue("Cost", std::to_string(total_cost)));

    return vbox(stats) | border;
  }

  /**
   * @brief Displays an error message with FTXUI
   */
  inline void displayError(const string& title, const string& message) {
    Elements error_title;
    error_title.push_back(txt("[ERROR] ") | color(Color::RedLight) | bold);
    error_title.push_back(txt(title) | bold | color(Color::Red));

    Elements content;
    content.push_back(Header(CHECKER_NAME));
    content.push_back(txt(""));
    content.push_back(hbox(error_title));
    content.push_back(txt(""));
    content.push_back(txt(message));
    content.push_back(txt(""));

    auto document = vbox(content) | border;

    auto screen = Screen::Create(Dimension::Full(), Dimension::Fit(document, true));
    Render(screen, document);
    screen.Print();
    std::cout << std::endl;
  }

  void displayResults(const ResultBuilder&);
}   // namespace ftxui_helpers
#endif

// ============================================================================
// Helper Classes
// ============================================================================

/**
 * @brief RAII guard for temporarily modifying arc metrics during network interventions.
 *
 * This class provides a scoped mechanism to temporarily set arc metrics to infinity
 * for specified intervention arcs, and automatically restore their original values
 * when the guard goes out of scope.
 *
 * @details The guard stores references to the metrics map and saves the original
 * metric values of intervention arcs. Upon construction, it sets these arcs' metrics
 * to infinity. The destructor restores all modified metrics to their original values.
 *
 * This class is non-copyable and non-movable to ensure proper RAII semantics and
 * prevent issues with the stored references.
 *
 * @note The lifetime of the InterventionGuard must not exceed the lifetime of the
 * metrics map it references.
 */
struct InterventionGuard {
  Digraph::ArcMap< SegmentRouting::Metric >&                         _metrics;
  nt::TrivialDynamicArray< nt::Pair< Arc, SegmentRouting::Metric > > _old_metrics;

  InterventionGuard(const Digraph& network, Digraph::ArcMap< SegmentRouting::Metric >& metrics, const nt::IntDynamicArray& intervention_arc_ids) : _metrics(metrics) {
    _old_metrics.ensure(intervention_arc_ids.size());

    for (const int i_arc: intervention_arc_ids) {
      const Arc arc = network.arcFromId(i_arc);
      _old_metrics.add({arc, _metrics[arc]});
      _metrics[arc] = SegmentRouting::infinity;
    }
  }

  ~InterventionGuard() {
    // Restore original metrics
    for (const auto& p: _old_metrics) {
      _metrics[p.first] = p.second;
    }
  }

  // Disable copying and moving
  InterventionGuard(const InterventionGuard&) = delete;
  InterventionGuard& operator=(const InterventionGuard&) = delete;
  InterventionGuard(InterventionGuard&&) = delete;
  InterventionGuard& operator=(InterventionGuard&&) = delete;
};

/**
 * @brief A builder class for constructing JSON result documents.
 *
 * This class provides a fluent interface for building JSON documents that represent
 * computation results, including validity status, cost metrics, and time-series objectives.
 * The builder pattern ensures proper initialization and allows incremental construction
 * of the result document.
 *
 * Example usage:
 * @code
 *   ResultBuilder builder;
 *   builder.setValid(true);
 *   builder.addObjectiveValues(0, 0.75, 1.25, 0.4);
 *   builder.display();
 * @endcode
 */
struct ResultBuilder {
  const Instance&                                           _instance;
  bool                                                      _use_ftxui;
  bool                                                      _is_valid;
  nt::DoubleDynamicArray                                    _time_slots;
  nt::DoubleDynamicArray                                    _mlu_values;
  nt::DoubleDynamicArray                                    _inv_values;
  nt::DoubleDynamicArray                                    _jain_values;
  nt::TrivialDynamicArray< nt::Triple< int, Arc, double > > _sat_values;

  // Statistics
  int _i_num_interventions;
  int _i_max_segments;
  int _i_total_srpaths;
  int _i_total_segments;
  int _i_total_cost;

  std::string _net_file;
  std::string _tm_file;
  std::string _scenario_file;
  std::string _srpaths_file;

  ResultBuilder(const Instance& instance, bool use_ftxui = false) :
      _instance(instance), _use_ftxui(use_ftxui), _is_valid(false), _i_num_interventions(0), _i_max_segments(0), _i_total_srpaths(0), _i_total_segments(0), _i_total_cost(0) {}

  void setValid(bool is_valid) noexcept { _is_valid = is_valid; }

  void addObjectiveValues(int time_slot, double mlu, double inv_load_cost, double jain_index) noexcept {
    _time_slots.add(time_slot);
    _mlu_values.add(mlu);
    _inv_values.add(inv_load_cost);
    _jain_values.add(jain_index);
  }

  void addSaturationValue(int time_slot, Arc arc, double saturation) noexcept { _sat_values.add({time_slot, arc, saturation}); }

  void buildJsonDocument(nt::JSONDocument& doc) noexcept {
    doc.SetObject();
    doc.AddMember(VALID_ATTR, _is_valid, doc.GetAllocator());

    if (_is_valid) {
      doc.AddMember(TOTAL_COST_ATTR, _i_total_cost, doc.GetAllocator());
      doc.AddMember(TOTAL_SRPATHS_ATTR, _i_total_srpaths, doc.GetAllocator());
      doc.AddMember(TOTAL_SEGMENTS_ATTR, _i_total_segments, doc.GetAllocator());

      nt::JSONValue _objectives_array(nt::JSONValueType::ARRAY_TYPE);
      for (int i = 0; i < _time_slots.size(); ++i) {
        nt::JSONValue obj(nt::JSONValueType::OBJECT_TYPE);
        obj.AddMember(TIMESTEP_ATTR, _time_slots[i], doc.GetAllocator());
        obj.AddMember(MLU_ATTR, _mlu_values[i], doc.GetAllocator());
        obj.AddMember(INV_LOAD_COST_ATTR, _inv_values[i], doc.GetAllocator());
        obj.AddMember(JAIN_INDEX_ATTR, _jain_values[i], doc.GetAllocator());
        _objectives_array.PushBack(obj, doc.GetAllocator());
      }
      doc.AddMember(OBJECTIVES_ATTR, _objectives_array, doc.GetAllocator());

      nt::JSONValue _saturations_array(nt::JSONValueType::ARRAY_TYPE);
      nt::sort(_sat_values, [](const auto a, const auto b) { return a.third > b.third; });
      for (const auto& triplet: _sat_values) {
        nt::JSONValue obj(nt::JSONValueType::OBJECT_TYPE);
        obj.AddMember(TIMESTEP_ATTR, triplet.first, doc.GetAllocator());
        const Arc  arc = triplet.second;
        const Node src = _instance.network.source(arc);
        const Node dst = _instance.network.target(arc);
        obj.AddMember(NET_SRC_ATTR, _instance.node_hashes[src], doc.GetAllocator());
        obj.AddMember(NET_DST_ATTR, _instance.node_hashes[dst], doc.GetAllocator());
        obj.AddMember(SAT_ATTR, triplet.third, doc.GetAllocator());
        _saturations_array.PushBack(obj, doc.GetAllocator());
      }
      doc.AddMember(SATURATIONS_ATTR, _saturations_array, doc.GetAllocator());
    }
  }

  void display(int i_max_decimal_places = 12) noexcept {
    if (_use_ftxui) {
#ifdef ENABLE_FTXUI
      ftxui_helpers::displayResults(*this);
#else
      std::cout << ERR_MSG_57 << std::endl;
#endif
    } else {
      nt::JSONDocument doc;
      buildJsonDocument(doc);
      std::cout << nt::JSONDocument::toString(doc, i_max_decimal_places) << std::endl;
    }
  }
};

#ifdef ENABLE_FTXUI
namespace ftxui_helpers {
  /**
   * @brief Displays the result summary using FTXUI
   */
  inline void displayResults(const ResultBuilder& result_builder) {
    Elements content;

    // Header
    content.push_back(Header(CHECKER_NAME " v" CHECKER_VERSION, CHECKER_COPYRIGHT, true));

    // Files section
    content.push_back(SectionTitle("INPUT FILES"));
    Elements files;
    files.push_back(KeyValue("Network", result_builder._net_file));
    files.push_back(KeyValue("Traffic Matrix", result_builder._tm_file));
    files.push_back(KeyValue("Scenario", result_builder._scenario_file));
    files.push_back(KeyValue("SR Paths", result_builder._srpaths_file));
    content.push_back(vbox(files) | border);
    content.push_back(txt(""));

    // Instance info section
    content.push_back(SectionTitle("INSTANCE INFORMATION"));
    Elements instance;
    instance.push_back(KeyValue("Nodes", std::to_string(nt::graphs::countNodes(result_builder._instance.network))));
    instance.push_back(KeyValue("Links", std::to_string(nt::graphs::countArcs(result_builder._instance.network))));
    instance.push_back(KeyValue("Demands", std::to_string(nt::graphs::countArcs(result_builder._instance.demand_graph))));
    instance.push_back(KeyValue("Time Slots", std::to_string(result_builder._instance.i_num_time_slots)));
    instance.push_back(KeyValue("Interventions", std::to_string(result_builder._i_num_interventions)));
    instance.push_back(KeyValue("Max Segments", std::to_string(result_builder._i_max_segments)));
    content.push_back(vbox(instance) | border);
    content.push_back(txt(""));

    // Validation status
    content.push_back(SectionTitle("VALIDATION"));
    content.push_back(StatusIndicator(result_builder._is_valid, result_builder._is_valid ? "Input VALID" : "Input INVALID") | border);
    content.push_back(txt(""));

    // Results (only if valid)
    if (result_builder._is_valid && !result_builder._mlu_values.empty()) {
      content.push_back(SectionTitle("RESULTS"));
      content.push_back(txt(""));

      Elements results_row;
      results_row.push_back(createStatistics(result_builder._mlu_values, result_builder._i_total_srpaths, result_builder._i_total_segments, result_builder._i_total_cost));
      results_row.push_back(txt("  "));
      results_row.push_back(createBarChart(result_builder._mlu_values) | border);
      content.push_back(hbox(results_row));
    }

    auto document = vbox(content);

    auto screen = Screen::Create(Dimension::Full(), Dimension::Fit(document, true));
    Render(screen, document);
    screen.Print();
    std::cout << std::endl;
  }

}   // namespace ftxui_helpers
#endif

#endif   // _CHECKER_HELPERS_H_