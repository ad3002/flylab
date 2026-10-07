// Package interpret turns a finished run into a deterministic digest (facts, numbers,
// annotations, coverage) and asks Claude for hypotheses about it (contract v3).
package interpret

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
)

// AnnotationColumns is the exact header of data/annotations_630.tsv (derived by
// scripts/setup_data.sh from the FlyWire annotations of Schlegel et al. 2024).
var AnnotationColumns = []string{
	"root_id", "flow", "super_class", "cell_class", "cell_sub_class", "cell_type",
	"hemibrain_type", "top_nt", "top_nt_conf", "side", "nerve",
}

// AnnotationsCitation is the source of the annotation table, quoted in every digest.
const AnnotationsCitation = "Schlegel et al., Nature 2024. doi:10.1038/s41586-024-07686-5 (FlyWire neuron annotations, flyconnectome/flywire_annotations)"

// Annotation is one neuron's community annotation. Empty strings mean "not annotated in that
// field" in the source table; TopNTConf is nil when the source has no confidence value.
type Annotation struct {
	Flow          string   `json:"flow"`
	SuperClass    string   `json:"super_class"`
	CellClass     string   `json:"cell_class"`
	CellSubClass  string   `json:"cell_sub_class"`
	CellType      string   `json:"cell_type"`
	HemibrainType string   `json:"hemibrain_type"`
	TopNT         string   `json:"top_nt"`
	TopNTConf     *float64 `json:"top_nt_conf"`
	Side          string   `json:"side"`
	Nerve         string   `json:"nerve"`
}

// Annotations is the loaded table. Ready is false when the file does not exist (expected
// absence: the server runs and every digest reports coverage 0 with a warning).
type Annotations struct {
	Ready  bool
	Path   string
	byRoot map[string]*Annotation
}

// Lookup returns the annotation of a root id, or nil when the neuron is not annotated.
func (a *Annotations) Lookup(rootID string) *Annotation {
	if a == nil || !a.Ready {
		return nil
	}
	return a.byRoot[rootID]
}

// Count is the number of annotated neurons.
func (a *Annotations) Count() int {
	if a == nil {
		return 0
	}
	return len(a.byRoot)
}

// LoadAnnotations reads annotations_630.tsv. A missing file returns Ready=false and no error;
// a file that exists but cannot be read, has a different header, a row with the wrong number
// of columns, a non-numeric or duplicate root_id, or a malformed top_nt_conf is an error
// naming the line (a startup error: nothing is silently skipped).
func LoadAnnotations(path string) (*Annotations, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Annotations{Ready: false, Path: path, byRoot: map[string]*Annotation{}}, nil
		}
		return nil, fmt.Errorf("annotations %s cannot be opened: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	out := &Annotations{Ready: true, Path: path, byRoot: map[string]*Annotation{}}
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		if line == 1 {
			if text != strings.Join(AnnotationColumns, "\t") {
				return nil, fmt.Errorf("annotations %s line 1 is %q, expected the header %q",
					path, text, strings.Join(AnnotationColumns, "\t"))
			}
			continue
		}
		if text == "" {
			continue
		}
		cols := strings.Split(text, "\t")
		if len(cols) != len(AnnotationColumns) {
			return nil, fmt.Errorf("annotations %s line %d has %d columns, expected %d", path, line, len(cols), len(AnnotationColumns))
		}
		id := cols[0]
		if !isRootID(id) {
			return nil, fmt.Errorf("annotations %s line %d: root_id %q is not a FlyWire root id", path, line, id)
		}
		if _, dup := out.byRoot[id]; dup {
			return nil, fmt.Errorf("annotations %s line %d: root_id %s appears twice", path, line, id)
		}
		a := &Annotation{
			Flow: cols[1], SuperClass: cols[2], CellClass: cols[3], CellSubClass: cols[4], CellType: cols[5],
			HemibrainType: cols[6], TopNT: cols[7], Side: cols[9], Nerve: cols[10],
		}
		if cols[8] != "" {
			v, err := strconv.ParseFloat(cols[8], 64)
			if err != nil || math.IsNaN(v) || v < 0 || v > 1 {
				return nil, fmt.Errorf("annotations %s line %d: top_nt_conf %q is not a number in [0,1]", path, line, cols[8])
			}
			v = math.Round(v*1000) / 1000
			a.TopNTConf = &v
		}
		out.byRoot[id] = a
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("annotations %s cannot be read after line %d: %w", path, line, err)
	}
	if line == 0 {
		return nil, fmt.Errorf("annotations %s is empty (no header)", path)
	}
	return out, nil
}

func isRootID(s string) bool {
	if len(s) < 15 || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
