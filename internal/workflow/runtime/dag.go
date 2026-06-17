package runtime

import (
	"errors"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// planDAG computes execution waves for the column schema using Kahn's
// algorithm over the depends_on edges. Each returned slice is a wave of
// columns that can run in parallel — wave[N] must wait for wave[N-1] to
// finish (because some column in wave[N] depends on a column in wave[N-1]).
//
// Errors:
//   - cycle detected (depends_on cycle)
//   - depends_on references an unknown column ID
func planDAG(cols []store.SheetWorkflowColumn) ([][]store.SheetWorkflowColumn, error) {
	if len(cols) == 0 {
		return nil, errors.New("no columns")
	}

	byID := make(map[string]store.SheetWorkflowColumn, len(cols))
	indeg := make(map[string]int, len(cols))
	for _, c := range cols {
		if c.ID == "" {
			return nil, fmt.Errorf("column %q missing id", c.Name)
		}
		if _, dup := byID[c.ID]; dup {
			return nil, fmt.Errorf("duplicate column id %q", c.ID)
		}
		byID[c.ID] = c
		indeg[c.ID] = 0
	}
	// Validate references + count in-degrees.
	for _, c := range cols {
		for _, dep := range c.DependsOn {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("column %q depends on unknown column id %q", c.Name, dep)
			}
			if dep == c.ID {
				return nil, fmt.Errorf("column %q depends on itself", c.Name)
			}
			indeg[c.ID]++
		}
	}

	var waves [][]store.SheetWorkflowColumn
	processed := 0
	for processed < len(cols) {
		var wave []store.SheetWorkflowColumn
		// Collect all zero-in-degree columns AT THIS POINT.
		for _, c := range cols {
			if indeg[c.ID] == 0 {
				wave = append(wave, c)
			}
		}
		if len(wave) == 0 {
			return nil, errors.New("dependency cycle detected in columns")
		}
		// Mark them as -1 so they aren't picked again, and decrement
		// in-degree of their dependents.
		for _, c := range wave {
			indeg[c.ID] = -1
			processed++
			for _, other := range cols {
				if containsString(other.DependsOn, c.ID) {
					indeg[other.ID]--
				}
			}
		}
		waves = append(waves, wave)
	}
	return waves, nil
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
