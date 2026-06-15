<div align="center">
<img src="doc/challenge_banner.svg" height="350" />

[![DOI](https://zenodo.org/badge/DOI/10.5281/zenodo.19219524.svg)](https://doi.org/10.5281/zenodo.19219524)

</div>

# EURO/ROADEF 2026 Challenge

The **EURO/ROADEF 2026 Challenge** is an international optimization competition focused on solving the **T-Adaptive Segment Routing** problem that arise from the traffic engineering area.

### Documentation

- **[Problem Description](doc/Challenge_Orange_ROADEF_2026_Subject.pdf)** — Formal specification of the optimization problem
- **[Challenge Rules](doc/Challenge_Orange_ROADEF_2026_Rules.pdf)** — Official rules, submission requirements, and evaluation criteria
- **[Challenge Presentation](doc/Slides_ROADEF_2026.pdf)** — Slides presented at ROADEF 2026 


## Getting Started

1. **Read the documentation**: Start with the [Problem Description](doc/Challenge_Orange_ROADEF_2026_Subject.pdf) and [Challenge Rules](doc/Challenge_Orange_ROADEF_2026_Rules.pdf)
2. **Download the datasets**: The first dataset will be released on **March 6, 2026**
4. **Develop your solution**
3. **Use the checker tool**: Validate your solutions using the provided [checker](checker/README.md)
5. **Submit your results**: Follow the submission guidelines described in the [challenge rules](doc/Challenge_Orange_ROADEF_2026_Rules.pdf)

## Datasets

The challenge provides three datasets with increasing difficulty and complexity:

### 📦 Dataset A 
**Release Date**: March 6, 2026
**Purpose**: Initial "easy" problem instances with horizon value bounded by two

### 📦 Dataset B 
**Release Date**: June 15, 2026
**Purpose**: Intermediate difficulty instances, unbounded horizon value

### 📦 Dataset C 
**Release Date**: October 5, 2026
**Purpose**: Final evaluation instances with highest complexity

## Tools

The following tools are provided for your convenience. **There is no requirement to use them**

- **[checker](checker/README.md)** — Validates instance and solution file formats, computes objective function values, and checks solution feasibility
- **[networktools](https://gitlab.com/Orange-OpenSource/network-optimization-tools/networktools)** — C++20 network/graph optimization library

## Challenge Results

The status/results of each phase of the EURO/ROADEF 2026 Challenge can be found in the following table

| Phase | Status/Results | Date |
| :--- | :--- | :--- |
| **Sprint** | [Results available here](sprint_results) | June, 15th 2026 |
| **Qualification** | To be released | October 5, 2026 |
| **Final** | To be released | During ROADEF 2027 |

## Support

If you encounter any problems with the datasets, tools, or documentation then browse existing [issues](https://gitlab.com/Orange-OpenSource/network-optimization-tools/challenge-roadef-2026/-/issues) for answers first. Otherwise, report the issue [here](https://gitlab.com/Orange-OpenSource/network-optimization-tools/challenge-roadef-2026/-/issues/new?description_template=bug_report).
