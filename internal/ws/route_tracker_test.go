package ws

import (
	"sync"
	"testing"
)

func TestRouteTracker_Append(t *testing.T) {
	rt := newRouteTracker()

	coords := rt.append("v-1", -122.4194, 37.7749)

	if len(coords) != 1 {
		t.Fatalf("expected 1 coordinate, got %d", len(coords))
	}
	if coords[0][0] != -122.4194 || coords[0][1] != 37.7749 {
		t.Fatalf("expected [-122.4194, 37.7749], got %v", coords[0])
	}
}

func TestRouteTracker_Append_AccumulatesPoints(t *testing.T) {
	rt := newRouteTracker()

	rt.append("v-1", -122.4194, 37.7749)
	rt.append("v-1", -122.4180, 37.7760)
	coords := rt.append("v-1", -122.4170, 37.7770)

	if len(coords) != 3 {
		t.Fatalf("expected 3 coordinates, got %d", len(coords))
	}
	// Verify order is preserved.
	if coords[0][0] != -122.4194 {
		t.Fatalf("first point lng: expected -122.4194, got %v", coords[0][0])
	}
	if coords[2][0] != -122.4170 {
		t.Fatalf("third point lng: expected -122.4170, got %v", coords[2][0])
	}
}

func TestRouteTracker_Append_IsolatesVehicles(t *testing.T) {
	rt := newRouteTracker()

	rt.append("v-1", -122.4194, 37.7749)
	rt.append("v-1", -122.4180, 37.7760)
	rt.append("v-2", -73.9857, 40.7484)

	v1 := rt.get("v-1")
	v2 := rt.get("v-2")

	if len(v1) != 2 {
		t.Fatalf("v-1: expected 2 coordinates, got %d", len(v1))
	}
	if len(v2) != 1 {
		t.Fatalf("v-2: expected 1 coordinate, got %d", len(v2))
	}
}

func TestRouteTracker_Clear(t *testing.T) {
	rt := newRouteTracker()

	rt.append("v-1", -122.4194, 37.7749)
	rt.append("v-1", -122.4180, 37.7760)
	rt.clear("v-1")

	coords := rt.get("v-1")
	if coords != nil {
		t.Fatalf("expected nil after clear, got %v", coords)
	}
}

func TestRouteTracker_Clear_DoesNotAffectOtherVehicles(t *testing.T) {
	rt := newRouteTracker()

	rt.append("v-1", -122.4194, 37.7749)
	rt.append("v-2", -73.9857, 40.7484)
	rt.clear("v-1")

	if coords := rt.get("v-2"); len(coords) != 1 {
		t.Fatalf("v-2 should be unaffected, got %d coordinates", len(coords))
	}
}

func TestRouteTracker_Clear_NonexistentVehicle(t *testing.T) {
	rt := newRouteTracker()
	// Should not panic.
	rt.clear("nonexistent")
}

func TestRouteTracker_Get_Empty(t *testing.T) {
	rt := newRouteTracker()

	coords := rt.get("v-1")
	if coords != nil {
		t.Fatalf("expected nil for unknown vehicle, got %v", coords)
	}
}

func TestRouteTracker_Snapshot_IsDeepCopy(t *testing.T) {
	rt := newRouteTracker()

	rt.append("v-1", -122.4194, 37.7749)
	coords := rt.get("v-1")

	// Mutate the returned slice — should not affect the tracker.
	coords[0][0] = 999.0

	fresh := rt.get("v-1")
	if fresh[0][0] != -122.4194 {
		t.Fatalf("snapshot was not a deep copy: got lng=%v", fresh[0][0])
	}
}

func TestRouteTracker_ConcurrentAccess(t *testing.T) {
	rt := newRouteTracker()
	var wg sync.WaitGroup

	// Concurrent appends from multiple goroutines.
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rt.append("v-1", float64(i), float64(i))
		}(i)
	}

	// Concurrent clears.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 10 {
			rt.clear("v-1")
		}
	}()

	// Concurrent reads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			rt.get("v-1")
		}
	}()

	wg.Wait()
	// If we get here without a race detector complaint, the test passes.
}
