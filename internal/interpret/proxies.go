package interpret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Proxy is a literature-backed link between a readout neuron's activity and a behaviour
// (registry/readout_proxies.json). It is a fact about the cited work, never about a run.
type Proxy struct {
	ProxyID             string   `json:"proxy_id"`
	GroupID             string   `json:"group_id,omitempty"`
	NeuronIDs           []string `json:"neuron_ids"`
	BehaviourEn         string   `json:"behaviour_en"`
	BehaviourRu         string   `json:"behaviour_ru"`
	Evidence            string   `json:"evidence"`
	Limits              string   `json:"limits"`
	Reference           string   `json:"reference"`
	SecondaryReferences []string `json:"secondary_references,omitempty"`
}

// LoadProxies reads registry/readout_proxies.json. The file ships with the code, so a missing,
// unreadable or malformed file (unknown field, empty required field, malformed root id,
// duplicate proxy_id) is a startup error.
func LoadProxies(registryDir string) ([]Proxy, error) {
	path := filepath.Join(registryDir, "readout_proxies.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("readout proxies %s cannot be read: %w", path, err)
	}
	var doc struct {
		SchemaVersion string  `json:"schema_version"`
		Note          string  `json:"note"`
		Proxies       []Proxy `json:"proxies"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("readout proxies %s cannot be parsed: %w", path, err)
	}
	if doc.SchemaVersion != "1.0" {
		return nil, fmt.Errorf("readout proxies %s: schema_version %q is not 1.0", path, doc.SchemaVersion)
	}
	seen := map[string]bool{}
	for i, p := range doc.Proxies {
		var missing []string
		for name, v := range map[string]string{
			"proxy_id": p.ProxyID, "behaviour_en": p.BehaviourEn, "behaviour_ru": p.BehaviourRu,
			"evidence": p.Evidence, "limits": p.Limits, "reference": p.Reference,
		} {
			if strings.TrimSpace(v) == "" {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("readout proxies %s: proxies[%d] has empty %s", path, i, strings.Join(sortedCopy(missing), ", "))
		}
		if len(p.NeuronIDs) == 0 {
			return nil, fmt.Errorf("readout proxies %s: proxies[%d] (%s) has no neuron_ids", path, i, p.ProxyID)
		}
		for _, id := range p.NeuronIDs {
			if !isRootID(id) {
				return nil, fmt.Errorf("readout proxies %s: proxies[%d] (%s) neuron id %q is not a FlyWire root id", path, i, p.ProxyID, id)
			}
		}
		if seen[p.ProxyID] {
			return nil, fmt.Errorf("readout proxies %s: proxy_id %s appears twice", path, p.ProxyID)
		}
		seen[p.ProxyID] = true
	}
	return doc.Proxies, nil
}
