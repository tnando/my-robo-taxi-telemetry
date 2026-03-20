package ws

import "sync"

// routeTracker accumulates route coordinates for vehicles that are currently
// driving. Each coordinate is a [longitude, latitude] pair following the
// Mapbox/GeoJSON convention. The tracker is safe for concurrent use.
type routeTracker struct {
	mu     sync.Mutex
	routes map[string][][2]float64
}

func newRouteTracker() *routeTracker {
	return &routeTracker{
		routes: make(map[string][][2]float64),
	}
}

// append adds a coordinate to the vehicle's route and returns the full
// accumulated route. The caller must ensure lng and lat are valid before
// calling append.
func (rt *routeTracker) append(vehicleID string, lng, lat float64) [][]float64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	rt.routes[vehicleID] = append(rt.routes[vehicleID], [2]float64{lng, lat})
	return rt.snapshot(vehicleID)
}

// clear removes all accumulated route data for a vehicle. This should be
// called when a drive ends or a vehicle disconnects.
func (rt *routeTracker) clear(vehicleID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	delete(rt.routes, vehicleID)
}

// get returns a copy of the current route for a vehicle, or nil if no
// route data exists.
func (rt *routeTracker) get(vehicleID string) [][]float64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	return rt.snapshot(vehicleID)
}

// snapshot returns a deep copy of the route for serialization. Must be
// called with rt.mu held.
func (rt *routeTracker) snapshot(vehicleID string) [][]float64 {
	coords := rt.routes[vehicleID]
	if len(coords) == 0 {
		return nil
	}

	out := make([][]float64, len(coords))
	for i, c := range coords {
		out[i] = []float64{c[0], c[1]}
	}
	return out
}
