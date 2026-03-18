package simulator

import (
	"testing"
)

func TestNewScenario(t *testing.T) {
	tests := []struct {
		name     string
		wantName string
		wantNil  bool
	}{
		{name: "highway-drive", wantName: "highway-drive"},
		{name: "city-drive", wantName: "city-drive"},
		{name: "parking-lot", wantName: "parking-lot"},
		{name: "unknown", wantNil: true},
		{name: "", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScenario(tt.name)
			if tt.wantNil {
				if s != nil {
					t.Fatalf("NewScenario(%q) = %v, want nil", tt.name, s)
				}
				return
			}
			if s == nil {
				t.Fatalf("NewScenario(%q) = nil, want non-nil", tt.name)
			}
			if got := s.Name(); got != tt.wantName {
				t.Errorf("Name() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestHighwayDrive_StartsParked(t *testing.T) {
	s := newHighwayDrive()
	state := s.Next()

	if state.GearPosition != "P" {
		t.Errorf("initial gear = %q, want P", state.GearPosition)
	}
	if state.Speed != 0 {
		t.Errorf("initial speed = %f, want 0", state.Speed)
	}
}

func TestHighwayDrive_EndsParked(t *testing.T) {
	s := newHighwayDrive()

	var lastState ScenarioState
	for !s.Done() {
		lastState = s.Next()
	}

	if lastState.GearPosition != "P" {
		t.Errorf("final gear = %q, want P", lastState.GearPosition)
	}
	if lastState.Speed != 0 {
		t.Errorf("final speed = %f, want 0", lastState.Speed)
	}
}

func TestHighwayDrive_ReachesHighwaySpeed(t *testing.T) {
	s := newHighwayDrive()

	var maxSpeed float64
	for !s.Done() {
		state := s.Next()
		if state.Speed > maxSpeed {
			maxSpeed = state.Speed
		}
	}

	if maxSpeed < 60 {
		t.Errorf("max speed = %f, want >= 60 mph", maxSpeed)
	}
}

func TestHighwayDrive_DrainsCharge(t *testing.T) {
	s := newHighwayDrive()

	first := s.Next()
	var last ScenarioState
	for !s.Done() {
		last = s.Next()
	}

	if last.ChargeLevel >= first.ChargeLevel {
		t.Errorf("charge did not drain: start=%d, end=%d", first.ChargeLevel, last.ChargeLevel)
	}
}

func TestHighwayDrive_AdvancesPosition(t *testing.T) {
	s := newHighwayDrive()

	first := s.Next()
	var last ScenarioState
	for !s.Done() {
		last = s.Next()
	}

	if last.Latitude == first.Latitude && last.Longitude == first.Longitude {
		t.Error("position did not change during drive")
	}
	if last.OdometerMiles <= first.OdometerMiles {
		t.Errorf("odometer did not advance: start=%f, end=%f",
			first.OdometerMiles, last.OdometerMiles)
	}
}

func TestCityDrive_HasStopAndGo(t *testing.T) {
	s := newCityDrive()

	var transitions int
	var wasMoving bool
	for !s.Done() {
		state := s.Next()
		isMoving := state.Speed > 0
		if isMoving != wasMoving {
			transitions++
		}
		wasMoving = isMoving
	}

	// City drive should have multiple stop/go transitions.
	if transitions < 4 {
		t.Errorf("transitions = %d, want >= 4 for stop-and-go pattern", transitions)
	}
}

func TestParkingLot_HasRapidGearChanges(t *testing.T) {
	s := newParkingLot()

	gearChanges := 0
	prevGear := ""
	seenGears := make(map[string]bool)

	for !s.Done() {
		state := s.Next()
		seenGears[state.GearPosition] = true
		if prevGear != "" && state.GearPosition != prevGear {
			gearChanges++
		}
		prevGear = state.GearPosition
	}

	if gearChanges < 5 {
		t.Errorf("gear changes = %d, want >= 5 for parking-lot scenario", gearChanges)
	}
	if !seenGears["P"] || !seenGears["D"] || !seenGears["R"] {
		t.Errorf("expected P, D, R gears; got %v", seenGears)
	}
}

func TestScenarioNames(t *testing.T) {
	names := ScenarioNames()
	if len(names) != 3 {
		t.Fatalf("ScenarioNames() returned %d names, want 3", len(names))
	}

	// Every name should produce a valid scenario.
	for _, name := range names {
		if s := NewScenario(name); s == nil {
			t.Errorf("NewScenario(%q) = nil, want valid scenario", name)
		}
	}
}
