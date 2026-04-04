// Package ws re-exports polyline functions from the shared internal/polyline
// package for backward compatibility within the ws package.
package ws

import "github.com/tnando/my-robo-taxi-telemetry/internal/polyline"

// DecodeRouteLine delegates to polyline.DecodeRouteLine.
func DecodeRouteLine(encoded string) ([][]float64, error) {
	return polyline.DecodeRouteLine(encoded)
}

// DecodePolyline delegates to polyline.DecodePolyline.
func DecodePolyline(encoded string) ([][]float64, error) {
	return polyline.DecodePolyline(encoded)
}
