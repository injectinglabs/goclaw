package runtime

import (
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func col(id string, deps ...string) store.SheetWorkflowColumn {
	return store.SheetWorkflowColumn{ID: id, Name: id, Type: "text", DependsOn: deps}
}

func TestPlanDAG_NoDeps_SingleWave(t *testing.T) {
	waves, err := planDAG([]store.SheetWorkflowColumn{
		col("a"), col("b"), col("c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 1 {
		t.Fatalf("want 1 wave, got %d", len(waves))
	}
	if len(waves[0]) != 3 {
		t.Fatalf("want 3 cols in wave, got %d", len(waves[0]))
	}
}

func TestPlanDAG_Linear(t *testing.T) {
	// a → b → c (each depends on previous)
	waves, err := planDAG([]store.SheetWorkflowColumn{
		col("a"),
		col("b", "a"),
		col("c", "b"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 3 {
		t.Fatalf("want 3 waves, got %d", len(waves))
	}
	for i, w := range waves {
		if len(w) != 1 {
			t.Errorf("wave %d: want 1 col, got %d", i, len(w))
		}
	}
	if waves[0][0].ID != "a" || waves[1][0].ID != "b" || waves[2][0].ID != "c" {
		t.Errorf("wrong order: %s, %s, %s", waves[0][0].ID, waves[1][0].ID, waves[2][0].ID)
	}
}

func TestPlanDAG_Diamond(t *testing.T) {
	// a → {b, c} → d
	waves, err := planDAG([]store.SheetWorkflowColumn{
		col("a"),
		col("b", "a"),
		col("c", "a"),
		col("d", "b", "c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 3 {
		t.Fatalf("want 3 waves, got %d", len(waves))
	}
	if len(waves[0]) != 1 || waves[0][0].ID != "a" {
		t.Errorf("wave 0 should be [a], got %+v", waveIDs(waves[0]))
	}
	if len(waves[1]) != 2 {
		t.Errorf("wave 1 should have 2 cols (b, c), got %+v", waveIDs(waves[1]))
	}
	if len(waves[2]) != 1 || waves[2][0].ID != "d" {
		t.Errorf("wave 2 should be [d], got %+v", waveIDs(waves[2]))
	}
}

func TestPlanDAG_Cycle(t *testing.T) {
	_, err := planDAG([]store.SheetWorkflowColumn{
		col("a", "b"),
		col("b", "a"),
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestPlanDAG_SelfRef(t *testing.T) {
	_, err := planDAG([]store.SheetWorkflowColumn{
		col("a", "a"),
	})
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("expected self-ref error, got %v", err)
	}
}

func TestPlanDAG_UnknownDep(t *testing.T) {
	_, err := planDAG([]store.SheetWorkflowColumn{
		col("a", "ghost"),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("expected unknown-dep error, got %v", err)
	}
}

func TestPlanDAG_Empty(t *testing.T) {
	_, err := planDAG(nil)
	if err == nil {
		t.Fatal("expected error on empty columns")
	}
}

func waveIDs(cols []store.SheetWorkflowColumn) []string {
	ids := make([]string, len(cols))
	for i, c := range cols {
		ids[i] = c.ID
	}
	return ids
}
