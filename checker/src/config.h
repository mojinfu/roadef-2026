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
 * @file config.h
 * @brief Configuration header file for the EURO/ROADEF Challenge 2026 checker.
 *
 * @author Morgan Chopin (morgan.chopin@orange.com)
 */

#ifndef _CHECKER_CONFIG_H_
#define _CHECKER_CONFIG_H_

// Checker Configuration
#define CHECKER_NAME "EURO/ROADEF Challenge 2026 checker"
#define CHECKER_VERSION "1.0.1"
#define CHECKER_LOGFILE "checker.log"
#define CHECKER_COPYRIGHT                                                                             \
               "SPDX-FileCopyrightText: Copyright (c) 2026 Orange SA\n"                               \
               "SPDX-License-Identifier: MIT\n"                                                       \
               "This software is distributed under the MIT License.\n"                                \
               "See the LICENSE file for more details or visit: https://opensource.org/license/MIT\n" 

// JSON Attributes Declaration
#define NET_SRC_ATTR "from"
#define NET_DST_ATTR "to"
#define TM_SRC_ATTR "source"
#define TM_DST_ATTR "target"
#define WEIGHT_ATTR "metric"
#define CAPACITY_ATTR "capacity"
#define DELAY_ATTR "delay"
#define ROUTER_ATTR "name"
#define DEMAND_ATTR "demand"
#define ID_ATTR "id"

#define VALID_ATTR "valid"
#define TOTAL_COST_ATTR "total_cost"
#define TOTAL_SRPATHS_ATTR "total_srpaths"
#define TOTAL_SEGMENTS_ATTR "total_segments"
#define TIMESTEP_ATTR "t"
#define MLU_ATTR "mlu"
#define INV_LOAD_COST_ATTR "inv_load_cost"
#define JAIN_INDEX_ATTR "jain_index"
#define OBJECTIVES_ATTR "objectives"
#define SATURATIONS_ATTR "saturations"
#define SAT_ATTR "sat"

#define NUM_TIMESTEPS_ATTR "num_time_slots"
#define DEMANDS_ATTR "demands"
#define MAX_SEGMENTS_ATTR "max_segments"
#define BUDGET_ATTR "budget"
#define VALUE_ATTR "value"
#define INTERVENTIONS_ATTR "interventions"
#define LINKS_ATTR "links"
#define SRPATHS_ATTR "srpaths"

// Note: ENABLE_FTXUI and CHECKER_LANG are set via Makefile at compile time
// Use: make ENABLE_FTXUI=1 CHECKER_LANG=EN
#if defined(LANG_EN)
#  include "english.h"
#elif defined(LANG_FR)
#  include "french.h"
#else
#  error "Unsupported language. Please define via make CHECKER_LANG=EN or make CHECKER_LANG=FR"
#endif

#endif