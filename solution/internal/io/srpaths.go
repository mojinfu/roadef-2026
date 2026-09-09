package io

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"tasr/internal/model"
)

// solutionEntry is one srpaths record.  Field order (d, t, w) and the JSON key
// names match the official writer (python json.dump with compact separators).
type solutionEntry struct {
	D int   `json:"d"`
	T int   `json:"t"`
	W []int `json:"w"`
}

type solutionDoc struct {
	Srpaths []solutionEntry `json:"srpaths"`
}

// SolutionToDict serialises a solution: only (d, t) pairs with a non-empty
// waypoint list are written (an omitted entry == direct ECMP path).  Waypoints
// (internal positions) are mapped back to network-file node ids.
func SolutionToDict(sol *model.Solution, inst *model.Instance) solutionDoc {
	doc := solutionDoc{Srpaths: []solutionEntry{}}
	for d := 0; d < sol.NDemands; d++ {
		for t := 0; t < sol.NSlots; t++ {
			wps := sol.Waypoints[d][t]
			if len(wps) == 0 {
				continue
			}
			w := make([]int, len(wps))
			for i, p := range wps {
				w[i] = inst.NodeIDs[p]
			}
			doc.Srpaths = append(doc.Srpaths, solutionEntry{D: d, T: t, W: w})
		}
	}
	return doc
}

// WriteSolution writes the solution file with the official compact JSON
// formatting (no whitespace, ':' and ',' separators).
func WriteSolution(path string, sol *model.Solution, inst *model.Instance) error {
	doc := SolutionToDict(sol, inst)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	data := buf.Bytes()
	// encoding/json adds a trailing newline; strip it to match the reference
	// json.dump output.
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	return os.WriteFile(path, data, 0o644)
}

// ReadSolution parses a solution file back into a Solution.  Entries may be
// omitted (== direct path) and keys are matched by first letter (d/t/w), like
// the official checker.
func ReadSolution(path string, inst *model.Instance) (*model.Solution, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadSolutionFromBytes(data, inst)
}

// LoadSolutionFromBytes parses an srpaths JSON document.
func LoadSolutionFromBytes(data []byte, inst *model.Instance) (*model.Solution, error) {
	var raw struct {
		Srpaths []map[string]json.RawMessage `json:"srpaths"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("srpaths: %w", err)
	}
	posOfID := inst.NodeIndex()
	sol := model.EmptySolution(inst.NDemands(), inst.NSlots)
	for _, entry := range raw.Srpaths {
		var d, t *int
		var wps []int
		for key, val := range entry {
			switch key[:1] {
			case "d":
				var v int
				if err := json.Unmarshal(val, &v); err != nil {
					return nil, fmt.Errorf("srpaths d: %w", err)
				}
				d = &v
			case "t":
				var v int
				if err := json.Unmarshal(val, &v); err != nil {
					return nil, fmt.Errorf("srpaths t: %w", err)
				}
				t = &v
			case "w":
				if err := json.Unmarshal(val, &wps); err != nil {
					return nil, fmt.Errorf("srpaths w: %w", err)
				}
			}
		}
		if d == nil || t == nil {
			return nil, fmt.Errorf("srpaths entry missing d/t: %v", entry)
		}
		if *d < 0 || *d >= inst.NDemands() {
			return nil, fmt.Errorf("demand id %d out of range", *d)
		}
		if *t < 0 || *t >= inst.NSlots {
			return nil, fmt.Errorf("time slot %d out of range", *t)
		}
		positions := make([]int, len(wps))
		for i, w := range wps {
			p, ok := posOfID[w]
			if !ok {
				return nil, fmt.Errorf("waypoint %d is not a node id of %s", w, inst.Name)
			}
			positions[i] = p
		}
		sol.Set(*d, *t, positions)
	}
	return sol, nil
}
