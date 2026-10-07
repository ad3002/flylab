package contracts

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// NeuronIDSet is the set of v630 root ids of the connectome the simulator runs (the
// completeness table named in data/dataset_manifest.json). Contract v4 section 5.
type NeuronIDSet struct {
	ids  map[string]struct{}
	Path string
}

var rootIDPattern = regexp.MustCompile(`^[0-9]{15,20}$`)

// Has reports whether id is a neuron of the connectome.
func (s *NeuronIDSet) Has(id string) bool {
	_, ok := s.ids[id]
	return ok
}

// Len is the number of neurons.
func (s *NeuronIDSet) Len() int { return len(s.ids) }

// LoadNeuronIDs reads the completeness CSV named by files.completeness in
// <dataDir>/dataset_manifest.json (resolved like flysim does: <dataDir>/<filename>). The file
// is required, as the simulator cannot run without it: a missing or unreadable file, a SHA-256
// or row count that differs from the manifest, a malformed root id or a duplicate is an error
// (startup error), never a partial set.
func LoadNeuronIDs(dataDir string) (*NeuronIDSet, error) {
	manifestPath := filepath.Join(dataDir, "dataset_manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("dataset manifest %s cannot be read: %w", manifestPath, err)
	}
	var manifest struct {
		Files map[string]struct {
			Path     string `json:"path"`
			Filename string `json:"filename"`
			SHA256   string `json:"sha256"`
			Rows     int    `json:"rows"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("dataset manifest %s cannot be parsed: %w", manifestPath, err)
	}
	comp, ok := manifest.Files["completeness"]
	if !ok {
		return nil, fmt.Errorf("dataset manifest %s has no files.completeness entry", manifestPath)
	}
	name := comp.Filename
	if name == "" {
		name = filepath.Base(comp.Path)
	}
	if name == "" || name == "." {
		return nil, fmt.Errorf("dataset manifest %s: files.completeness names no file", manifestPath)
	}
	path := filepath.Join(dataDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("completeness table %s cannot be read (run scripts/setup_data.sh): %w", path, err)
	}
	if comp.SHA256 != "" {
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, comp.SHA256) {
			return nil, fmt.Errorf("completeness table %s has SHA-256 %s, the manifest expects %s", path, got, comp.SHA256)
		}
	}
	set, err := parseCompleteness(bytes.NewReader(data), path)
	if err != nil {
		return nil, err
	}
	if comp.Rows > 0 && set.Len() != comp.Rows {
		return nil, fmt.Errorf("completeness table %s has %d neurons, the manifest says %d", path, set.Len(), comp.Rows)
	}
	return set, nil
}

// parseCompleteness reads the root id from the first column of every data row (the first line
// is the header), like flysim's graph builder.
func parseCompleteness(r io.Reader, path string) (*NeuronIDSet, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	set := &NeuronIDSet{ids: map[string]struct{}{}, Path: path}
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if line == 1 || text == "" {
			continue
		}
		id := strings.TrimSpace(strings.SplitN(text, ",", 2)[0])
		if !rootIDPattern.MatchString(id) {
			return nil, fmt.Errorf("completeness table %s line %d: %q is not a FlyWire root id", path, line, id)
		}
		if _, dup := set.ids[id]; dup {
			return nil, fmt.Errorf("completeness table %s line %d: root id %s appears twice", path, line, id)
		}
		set.ids[id] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("completeness table %s cannot be read: %w", path, err)
	}
	if line == 0 {
		return nil, fmt.Errorf("completeness table %s is empty", path)
	}
	if set.Len() == 0 {
		return nil, fmt.Errorf("completeness table %s has a header but no neurons", path)
	}
	return set, nil
}

// NewNeuronIDSet builds a set from ids (tests and tools).
func NewNeuronIDSet(ids []string) *NeuronIDSet {
	s := &NeuronIDSet{ids: make(map[string]struct{}, len(ids)), Path: "(in memory)"}
	for _, id := range ids {
		s.ids[id] = struct{}{}
	}
	return s
}

// MaxReportedUnknownIDs is how many unknown ids error details list (contract v4: first 20).
const MaxReportedUnknownIDs = 20

// UnknownNeuronField lists the unknown ids of one selector, e.g. Path "activation[0].selector".
type UnknownNeuronField struct {
	Path string   `json:"path"`
	IDs  []string `json:"ids"`
}

// UnknownNeuronsError means explicit neuron_ids of a plan are not neurons of the connectome
// (HTTP 422 VALIDATION_FAILED with details.unknown_neuron_ids; the planner turns it into a
// needs_input question).
type UnknownNeuronsError struct {
	Fields []UnknownNeuronField
	// IDs is every distinct unknown id, in plan order.
	IDs []string
}

func (e *UnknownNeuronsError) Error() string {
	var parts []string
	for _, f := range e.Fields {
		ids := f.IDs
		more := ""
		if len(ids) > 5 {
			more = fmt.Sprintf(" and %d more", len(ids)-5)
			ids = ids[:5]
		}
		parts = append(parts, fmt.Sprintf("%s.neuron_ids: %s%s", f.Path, strings.Join(ids, ", "), more))
	}
	return fmt.Sprintf("%d neuron id(s) are not neurons of the flywire_630 connectome: %s", len(e.IDs), strings.Join(parts, "; "))
}

// ReportedIDs is the first MaxReportedUnknownIDs unknown ids.
func (e *UnknownNeuronsError) ReportedIDs() []string {
	if len(e.IDs) > MaxReportedUnknownIDs {
		return e.IDs[:MaxReportedUnknownIDs]
	}
	return e.IDs
}

// Paths returns "<path>.neuron_ids" for each selector with unknown ids.
func (e *UnknownNeuronsError) Paths() []string {
	out := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		out[i] = f.Path + ".neuron_ids"
	}
	return out
}
