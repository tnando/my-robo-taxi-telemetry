package simulator

import (
	"math"
	"testing"
)

func TestAdvancePosition_Stationary(t *testing.T) {
	lat, lng := advancePosition(32.7767, -96.7970, 0, 0, 1.0)
	if lat != 32.7767 || lng != -96.7970 {
		t.Errorf("stationary position changed: (%f, %f)", lat, lng)
	}
}

func TestAdvancePosition_MovesNorth(t *testing.T) {
	lat, lng := advancePosition(32.7767, -96.7970, 0, 60, 60.0)
	// Heading 0 = north, so latitude should increase.
	if lat <= 32.7767 {
		t.Errorf("latitude should increase heading north, got %f", lat)
	}
	// Longitude should stay approximately the same heading due north.
	if math.Abs(lng-(-96.7970)) > 0.001 {
		t.Errorf("longitude should be stable heading north, got %f", lng)
	}
}

func TestAdvancePosition_MovesEast(t *testing.T) {
	_, lng := advancePosition(32.7767, -96.7970, 90, 60, 60.0)
	// Heading 90 = east, longitude should increase (less negative).
	if lng <= -96.7970 {
		t.Errorf("longitude should increase heading east, got %f", lng)
	}
}

func TestAdvancePosition_Distance(t *testing.T) {
	// 60 mph for 1 hour = 60 miles.
	startLat, startLng := 32.7767, -96.7970
	endLat, endLng := advancePosition(startLat, startLng, 0, 60, 3600)

	// Approximate distance using lat difference (1 degree lat ~ 69 miles).
	distMiles := (endLat - startLat) * 69.0
	if math.Abs(distMiles-60) > 2.0 {
		t.Errorf("expected ~60 miles, got ~%.1f miles", distMiles)
	}
	_ = endLng
}

func TestNormalizeHeading(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"zero", 0, 0},
		{"positive", 90, 90},
		{"full circle", 360, 0},
		{"over 360", 450, 90},
		{"negative", -90, 270},
		{"large negative", -450, 270},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeHeading(tt.in)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("normalizeHeading(%f) = %f, want %f", tt.in, got, tt.want)
			}
		})
	}
}
