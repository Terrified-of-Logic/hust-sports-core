package hustrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleGeoJSON = `{"type":"Feature","properties":{},"geometry":{"type":"LineString","coordinates":[[114.413853,30.5126406],[114.41377,30.5126395],[114.41370,30.5126301]]}}`

func TestRouteFromGeoJSONResamples(t *testing.T) {
	points, err := RouteFromGeoJSON(strings.NewReader(sampleGeoJSON), 1.0)
	if err != nil {
		t.Fatalf("RouteFromGeoJSON: %v", err)
	}
	if len(points) < 3 {
		t.Fatalf("points = %d", len(points))
	}
	// The first vertex must be [lat, lon], not [lon, lat].
	if points[0][0] < 30 || points[0][0] > 31 {
		t.Fatalf("first point = %v, want latitude first", points[0])
	}
	if last := points[len(points)-1]; last[1] < 114 || last[1] > 115 {
		t.Fatalf("last point = %v", last)
	}
	length := RouteLengthM(points)
	if length < 10 || length > 60 {
		t.Errorf("length = %.1f m", length)
	}
	// Coarser spacing yields fewer vertices.
	coarse, err := RouteFromGeoJSON(strings.NewReader(sampleGeoJSON), 10)
	if err != nil {
		t.Fatalf("RouteFromGeoJSON: %v", err)
	}
	if len(coarse) >= len(points) {
		t.Errorf("coarse spacing produced %d points, fine produced %d", len(coarse), len(points))
	}
}

func TestRouteFromGeoJSONRejectsNonLine(t *testing.T) {
	if _, err := RouteFromGeoJSON(strings.NewReader(`{"type":"Point","coordinates":[1,2]}`), 1); err == nil {
		t.Error("point geometry accepted as a route")
	}
	if _, err := RouteFromGeoJSON(strings.NewReader("not json"), 1); err == nil {
		t.Error("garbage accepted as a route")
	}
}

func TestBuildMultiLapRoute(t *testing.T) {
	lap, err := LoadRoutePoints("testdata/route_track.json")
	if err != nil {
		t.Fatalf("LoadRoutePoints: %v", err)
	}
	seed := int64(1)
	options := MultiLapOptions{DistanceM: 5000, NoiseM: 0.45, DriftM: 1.6, Seed: &seed}
	points, err := BuildMultiLapRoute(lap, options)
	if err != nil {
		t.Fatalf("BuildMultiLapRoute: %v", err)
	}
	length := RouteLengthM(points)
	if length < 5000 || length > 5050 {
		t.Errorf("length = %.1f m, want just over 5000", length)
	}
	if len(points) < len(lap)*2 {
		t.Errorf("points = %d, want more than one lap of %d", len(points), len(lap))
	}
	// The second lap must not sit exactly on the first one.
	drifted := 0
	for index := 0; index < len(lap); index++ {
		first := points[index]
		second := points[len(lap)+index]
		if HaversineDistanceM(first[0], first[1], second[0], second[1]) > 0.2 {
			drifted++
		}
	}
	if drifted < len(lap)/2 {
		t.Errorf("only %d/%d vertices drifted between laps", drifted, len(lap))
	}

	again, err := BuildMultiLapRoute(lap, options)
	if err != nil {
		t.Fatalf("BuildMultiLapRoute: %v", err)
	}
	if len(again) != len(points) || again[0] != points[0] {
		t.Error("multiroute is not deterministic for a seed")
	}
}

func TestWriteRouteFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "route.json")
	original, err := LoadRoutePoints("testdata/route_track.json")
	if err != nil {
		t.Fatalf("LoadRoutePoints: %v", err)
	}
	if err := WriteRouteFile(path, original); err != nil {
		t.Fatalf("WriteRouteFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(raw), "[[") {
		t.Errorf("route file = %s", truncate(string(raw), 60))
	}
	loaded, err := LoadRoutePoints(path)
	if err != nil {
		t.Fatalf("LoadRoutePoints: %v", err)
	}
	if len(loaded) != len(original) {
		t.Fatalf("points = %d, want %d", len(loaded), len(original))
	}
	if loaded[0] != original[0] {
		t.Errorf("first point = %v, want %v", loaded[0], original[0])
	}
	if err := WriteRouteFile(path, nil); err == nil {
		t.Error("empty route accepted")
	}
}
