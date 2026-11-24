package sports

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGPXAndGeoJSONPreserveSegmentsAndAxisOrder(t *testing.T) {
	const gpx = `<?xml version="1.0"?>
<gpx xmlns="http://www.topografix.com/GPX/1/1" version="1.1" creator="test">
  <trk><trkseg>
    <trkpt lat="30.5" lon="114.4"><time>2026-01-02T03:04:05+08:00</time></trkpt>
    <trkpt lat="30.501" lon="114.4"/>
  </trkseg><trkseg/></trk>
  <trk><trkseg><trkpt lat="31.0" lon="115.0"/><trkpt lat="31.001" lon="115.0"/></trkseg></trk>
</gpx>`
	track, err := ReadGPX(strings.NewReader(gpx))
	if err != nil {
		t.Fatal(err)
	}
	if track.CRS != WGS84 || len(track.Segments) != 2 {
		t.Fatalf("unexpected track: %+v", track)
	}
	first := track.Segments[0][0]
	if first.Lon != 114.4 || first.Lat != 30.5 || !first.Time.Equal(time.Date(2026, 1, 1, 19, 4, 5, 0, time.UTC)) {
		t.Fatalf("unexpected first point: %+v", first)
	}
	meters, err := track.LengthMeters()
	if err != nil || math.Abs(meters-222.39) > 0.02 {
		t.Fatalf("length = %f, err = %v", meters, err)
	}

	var output bytes.Buffer
	if err := WriteGeoJSON(&output, track); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `[114.4,30.5]`) {
		t.Fatalf("expected longitude first: %s", output.String())
	}
	roundTrip, err := ReadGeoJSON(&output, WGS84)
	if err != nil {
		t.Fatal(err)
	}
	track.Segments[0][0].Time = time.Time{}
	if !reflect.DeepEqual(track, roundTrip) {
		t.Fatalf("round trip changed geometry: %+v", roundTrip)
	}
}

func TestImportsRejectInvalidCoordinatesAndExtraDocuments(t *testing.T) {
	for _, gpx := range []string{
		`<gpx><trk><trkseg><trkpt lat="30.5"/></trkseg></trk></gpx>`,
		`<gpx><trk><trkseg><trkpt lat="NaN" lon="114.4"/></trkseg></trk></gpx>`,
		`<gpx><trk><trkseg><trkpt lat="30.5" lon="114.4"/></trkseg></trk></gpx><gpx/>`,
	} {
		if _, err := ReadGPX(strings.NewReader(gpx)); !errors.Is(err, ErrInvalidTrack) {
			t.Errorf("ReadGPX(%q): %v", gpx, err)
		}
	}
	for _, data := range []string{
		`{"type":"LineString","coordinates":[[null,30.5],[114.4,30.501]]}`,
		`{"type":"LineString","coordinates":[[30.5,114.4],[30.501,114.4]]}`,
		`{"type":"LineString","coordinates":[[114.4,30.5],[114.4,30.501]]} {}`,
	} {
		if _, err := ReadGeoJSON(strings.NewReader(data), WGS84); !errors.Is(err, ErrInvalidTrack) {
			t.Errorf("ReadGeoJSON(%q): %v", data, err)
		}
	}
}

func TestAlignmentUsesNearestEdgeAndDoesNotBridgeGaps(t *testing.T) {
	reference := Track{CRS: WGS84, Segments: [][]Point{
		{{Lon: 0}, {Lon: 0.001}},
		{{Lon: 0.003}, {Lon: 0.004}},
	}}
	observed := Track{CRS: WGS84, Segments: [][]Point{
		{{Lon: 0.0005, Lat: 0.00001}, {Lon: 0.002}, {Lon: 0.004}},
	}}
	result, err := AssessAlignment(observed, reference, 2)
	if err != nil || result.PointCount != 3 || result.WithinTolerance != 2 ||
		math.Abs(result.MaxOffsetMeters-111.195) > 0.02 || math.Abs(result.MeanOffsetMeters-37.436) > 0.02 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	reference.CRS = GCJ02
	if _, err := AssessAlignment(observed, reference, 2); !errors.Is(err, ErrCRS) {
		t.Fatalf("expected mixed CRS error, got %v", err)
	}
	if err := WriteGeoJSON(&bytes.Buffer{}, reference); !errors.Is(err, ErrCRS) {
		t.Fatalf("expected GCJ-02 GeoJSON export to fail, got %v", err)
	}
}
