// Package io parses the three T-ASR input JSON files into a model.Instance and
// serialises/deserialises *-srpaths.json solution files.
package io

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tasr/internal/model"
)

// Numeric fields are decoded as RawMessage and converted with the same
// tolerance the reference Python parser had (int()/float() accept both bare
// numbers and quoted strings).

func intRaw(b json.RawMessage) (int, error) {
	s := strings.TrimSpace(string(bytes.TrimSpace(b)))
	s = strings.Trim(s, `"`)
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %q", string(b))
	}
	return int(i), nil
}

func floatRaw(b json.RawMessage) (float64, error) {
	s := strings.TrimSpace(string(bytes.TrimSpace(b)))
	s = strings.Trim(s, `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a float: %q", string(b))
	}
	return f, nil
}

type netDoc struct {
	Nodes []struct {
		Name string          `json:"name"`
		ID   json.RawMessage `json:"id"`
	} `json:"nodes"`
	Links []struct {
		ID       json.RawMessage `json:"id"`
		From     json.RawMessage `json:"from"`
		To       json.RawMessage `json:"to"`
		Metric   json.RawMessage `json:"metric"`
		Capacity json.RawMessage `json:"capacity"`
	} `json:"links"`
}

// loadNetworkFile parses a *-net.json file.  Returns node names / ids in the
// order listed and arcs whose endpoints are node positions.
func loadNetworkFile(path string) ([]string, []int, []model.Arc, error) {
	doc := netDoc{}
	if err := readJSON(path, &doc); err != nil {
		return nil, nil, nil, err
	}
	if doc.Nodes == nil && doc.Links == nil {
		return nil, nil, nil, fmt.Errorf("%s: missing 'nodes' or 'links' section", path)
	}

	idToPos := map[int]int{}
	nodeNames := make([]string, 0, len(doc.Nodes))
	nodeIDs := make([]int, 0, len(doc.Nodes))
	for _, nd := range doc.Nodes {
		nid, err := intRaw(nd.ID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: node id: %w", path, err)
		}
		if nid < 0 {
			return nil, nil, nil, fmt.Errorf("%s: negative node id %d", path, nid)
		}
		if _, dup := idToPos[nid]; dup {
			return nil, nil, nil, fmt.Errorf("%s: duplicate node id %d", path, nid)
		}
		idToPos[nid] = len(nodeNames)
		name := nd.Name
		if name == "" {
			name = strconv.Itoa(nid)
		}
		nodeNames = append(nodeNames, name)
		nodeIDs = append(nodeIDs, nid)
	}
	if len(nodeNames) == 0 {
		return nil, nil, nil, fmt.Errorf("%s: no nodes", path)
	}

	arcs := make([]model.Arc, 0, len(doc.Links))
	for ln := range doc.Links {
		link := &doc.Links[ln]
		aid, err := intRaw(link.ID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: link id: %w", path, err)
		}
		if aid != len(arcs) {
			return nil, nil, nil, fmt.Errorf("%s: links must be listed with contiguous ids (got %d)", path, aid)
		}
		frm, err := intRaw(link.From)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: link %d from: %w", path, aid, err)
		}
		to, err := intRaw(link.To)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: link %d to: %w", path, aid, err)
		}
		fp, okF := idToPos[frm]
		tp, okT := idToPos[to]
		if !okF || !okT {
			return nil, nil, nil, fmt.Errorf("%s: link %d references unknown node", path, aid)
		}
		metric, err := floatRaw(link.Metric)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: link %d metric: %w", path, aid, err)
		}
		cap, err := floatRaw(link.Capacity)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: link %d capacity: %w", path, aid, err)
		}
		arcs = append(arcs, model.Arc{ID: aid, From: fp, To: tp, Metric: metric, Capacity: cap})
	}
	if len(arcs) == 0 {
		return nil, nil, nil, fmt.Errorf("%s: empty network", path)
	}
	return nodeNames, nodeIDs, arcs, nil
}

type tmDoc struct {
	NumTimeSlots json.RawMessage `json:"num_time_slots"`
	Demands      []struct {
		V []json.RawMessage `json:"v"`
		S json.RawMessage   `json:"s"`
		T json.RawMessage   `json:"t"`
	} `json:"demands"`
}

// loadTrafficMatrixFile parses a *-tm.json file.  Demand id == array index.
func loadTrafficMatrixFile(path string) (int, []model.Demand, error) {
	doc := tmDoc{}
	if err := readJSON(path, &doc); err != nil {
		return 0, nil, err
	}
	nSlots, err := intRaw(doc.NumTimeSlots)
	if err != nil || nSlots < 1 {
		return 0, nil, fmt.Errorf("%s: num_time_slots must be a positive integer", path)
	}
	demands := make([]model.Demand, 0, len(doc.Demands))
	for i := range doc.Demands {
		dm := &doc.Demands[i]
		if len(dm.V) != nSlots {
			return 0, nil, fmt.Errorf("%s: demand %d has %d volumes but num_time_slots=%d",
				path, i, len(dm.V), nSlots)
		}
		vol := make([]float64, nSlots)
		for k, v := range dm.V {
			f, err := floatRaw(v)
			if err != nil {
				return 0, nil, fmt.Errorf("%s: demand %d volume %d: %w", path, i, k, err)
			}
			vol[k] = f
		}
		s, err := intRaw(dm.S)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: demand %d s: %w", path, i, err)
		}
		t, err := intRaw(dm.T)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: demand %d t: %w", path, i, err)
		}
		demands = append(demands, model.Demand{Source: s, Target: t, Volume: vol})
	}
	return nSlots, demands, nil
}

type scnDoc struct {
	MaxSegments json.RawMessage `json:"max_segments"`
	Budget      []struct {
		T     json.RawMessage `json:"t"`
		Value json.RawMessage `json:"value"`
	} `json:"budget"`
	Interventions []struct {
		T     json.RawMessage   `json:"t"`
		Links []json.RawMessage `json:"links"`
	} `json:"interventions"`
}

// loadScenarioFile parses a *-scenario.json file.  Time step 0 never appears
// (nominal situation); the budget of a missing step defaults to 0.
func loadScenarioFile(path string, nSlots, nArcs int) (model.Scenario, error) {
	doc := scnDoc{}
	if err := readJSON(path, &doc); err != nil {
		return model.Scenario{}, err
	}
	maxSeg, err := intRaw(doc.MaxSegments)
	if err != nil || maxSeg < 0 {
		return model.Scenario{}, fmt.Errorf("%s: max_segments must be >= 0", path)
	}
	budget := make([]int, nSlots)
	for _, b := range doc.Budget {
		t, err := intRaw(b.T)
		if err != nil {
			return model.Scenario{}, fmt.Errorf("%s: budget t: %w", path, err)
		}
		if t < 1 || t >= nSlots {
			return model.Scenario{}, fmt.Errorf("%s: budget time step %d out of range [1,%d]",
				path, t, nSlots-1)
		}
		val, err := intRaw(b.Value)
		if err != nil {
			return model.Scenario{}, fmt.Errorf("%s: budget value: %w", path, err)
		}
		budget[t] = val
	}
	blocked := make([][]bool, nSlots)
	for t := range blocked {
		blocked[t] = make([]bool, nArcs)
	}
	for _, iv := range doc.Interventions {
		t, err := intRaw(iv.T)
		if err != nil {
			return model.Scenario{}, fmt.Errorf("%s: intervention t: %w", path, err)
		}
		if t < 1 || t >= nSlots {
			return model.Scenario{}, fmt.Errorf("%s: intervention time step %d out of range", path, t)
		}
		for _, raw := range iv.Links {
			aid, err := intRaw(raw)
			if err != nil {
				return model.Scenario{}, fmt.Errorf("%s: intervention link: %w", path, err)
			}
			if aid < 0 || aid >= nArcs {
				return model.Scenario{}, fmt.Errorf("%s: intervention link %d at t=%d out of range", path, aid, t)
			}
			blocked[t][aid] = true
		}
	}
	return model.Scenario{MaxSegments: maxSeg, Budget: budget, Blocked: blocked}, nil
}

// LoadInstanceFiles reads the three official input files.
func LoadInstanceFiles(netPath, tmPath, scnPath string) (*model.Instance, error) {
	nodeNames, nodeIDs, arcs, err := loadNetworkFile(netPath)
	if err != nil {
		return nil, err
	}
	nSlots, demands, err := loadTrafficMatrixFile(tmPath)
	if err != nil {
		return nil, err
	}
	idToPos := map[int]int{}
	for i, id := range nodeIDs {
		idToPos[id] = i
	}
	for i, d := range demands {
		s, okS := idToPos[d.Source]
		t, okT := idToPos[d.Target]
		if !okS || !okT {
			return nil, fmt.Errorf("%s: demand %d references unknown node", tmPath, i)
		}
		if s == t {
			return nil, fmt.Errorf("%s: demand %d is degenerate (s == t)", tmPath, i)
		}
		demands[i].Source = s
		demands[i].Target = t
	}

	scn, err := loadScenarioFile(scnPath, nSlots, len(arcs))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(filepath.Base(netPath), "-net.json")
	return &model.Instance{
		Name:      name,
		NodeNames: nodeNames,
		NodeIDs:   nodeIDs,
		Arcs:      arcs,
		NSlots:    nSlots,
		Demands:   demands,
		Scenario:  scn,
	}, nil
}

// LoadInstance reads the three files of an instance from a common prefix,
// e.g. "setA/setA-01" resolves setA/setA-01-net.json + -tm.json + -scenario.json.
func LoadInstance(prefix string) (*model.Instance, error) {
	return LoadInstanceFiles(prefix+"-net.json", prefix+"-tm.json", prefix+"-scenario.json")
}

func readJSON(path string, out interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
