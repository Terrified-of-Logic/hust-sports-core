package sports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAMapConversionAlignsDisplayTrackAcrossBatches(t *testing.T) {
	const source = `<gpx><trk><trkseg><trkpt lat="30.5" lon="114.4">
	<time>2026-01-02T03:04:05Z</time></trkpt></trkseg></trk></gpx>`
	observed, err := ReadGPX(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 39; i++ {
		observed.Segments[0] = append(observed.Segments[0], Point{Lon: 114.4 + float64(i)/1e6, Lat: 30.5})
	}
	observed.Segments = append(observed.Segments, []Point{{Lon: 114.401, Lat: 30.5}, {Lon: 114.40101, Lat: 30.5}})

	const referenceJSON = `{"type":"FeatureCollection","features":[
	 {"type":"Feature","geometry":{"type":"Point","coordinates":[114.4,30.5]}},
	 {"type":"Feature","geometry":{"type":"LineString","coordinates":[[114.404,30.498],[114.40502,30.498]]}}
	]}`
	reference, err := ReadGeoJSON(strings.NewReader(referenceJSON), GCJ02)
	if err != nil {
		t.Fatal(err)
	}
	var batchSizes []int
	converter, err := NewAMapConverter("test-key", &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "restapi.amap.com" || r.URL.Path != "/v3/assistant/coordinate/convert" {
			t.Fatalf("unexpected request: %v", r.URL)
		}
		q := r.URL.Query()
		if q.Get("coordsys") != "gps" || q.Get("output") != "JSON" || q.Get("key") != "test-key" {
			t.Fatalf("unexpected request parameters: %v", q)
		}
		pairs := strings.Split(q.Get("locations"), "|")
		batchSizes = append(batchSizes, len(pairs))
		locations := make([]string, len(pairs))
		for i, pair := range pairs {
			lon, lat, found := strings.Cut(pair, ",")
			x, xErr := strconv.ParseFloat(lon, 64)
			y, yErr := strconv.ParseFloat(lat, 64)
			if !found || xErr != nil || yErr != nil || !strings.HasPrefix(lon, "114.") || lat != "30.500000" {
				t.Fatalf("expected lon,lat with six decimals, got %q", pair)
			}
			locations[i] = fmt.Sprintf("%.6f,%.6f", x+0.004, y-0.002)
		}
		body, err := json.Marshal(map[string]string{"status": "1", "locations": strings.Join(locations, ";")})
		if err != nil {
			t.Fatal(err)
		}
		return testResponse(string(body)), nil
	})})
	if err != nil {
		t.Fatal(err)
	}

	display, err := converter.ConvertTrack(context.Background(), observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(batchSizes) != 2 || batchSizes[0] != 40 || batchSizes[1] != 1 {
		t.Fatalf("batch sizes: %v", batchSizes)
	}
	if display.CRS != GCJ02 || len(display.Segments) != 2 || len(display.Segments[1]) != 2 ||
		display.Segments[1][1].Lon != 114.40501 || !display.Segments[0][0].Time.Equal(observed.Segments[0][0].Time) ||
		observed.Segments[0][0].Lon != 114.4 || observed.CRS != WGS84 {
		t.Fatal("conversion changed point order, segments, time or source data")
	}
	result, err := AssessAlignment(display, reference, 0.05)
	if err != nil || result.WithinTolerance != 41 {
		t.Fatalf("display points did not align: %+v, %v", result, err)
	}
	copy, err := converter.ConvertTrack(context.Background(), display)
	if err != nil || len(batchSizes) != 2 {
		t.Fatalf("GCJ-02 copy made a request or failed: %v, %v", batchSizes, err)
	}
	copy.Segments[0][0].Lon = 0
	if display.Segments[0][0].Lon == 0 {
		t.Fatal("converted track shares the input segment")
	}
}

func TestAMapErrorsDoNotReturnPartialTrackOrExposeRequestURL(t *testing.T) {
	point := Point{Lon: 114.4, Lat: 30.5, Time: time.Now()}
	points := make([]Point, 41)
	for i := range points {
		points[i] = point
	}
	track := Track{CRS: WGS84, Segments: [][]Point{points}}
	var requests int
	converter, err := NewAMapConverter("secret-test-key", &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return testResponse(`{"status":1,"locations":"` + strings.Repeat("114.404,30.498;", 39) + `114.404,30.498"}`), nil
		}
		return nil, fmt.Errorf("transport failed for %s", r.URL)
	})})
	if err != nil {
		t.Fatal(err)
	}
	result, err := converter.ConvertTrack(context.Background(), track)
	if !errors.Is(err, ErrAMap) || strings.Contains(err.Error(), "secret-test-key") || strings.Contains(err.Error(), "114.4") || result.Segments != nil {
		t.Fatalf("unexpected result: %+v, %v", result, err)
	}
	if requests != 2 || track.Segments[0][0] != point {
		t.Fatal("failed conversion changed the input or did not finish its first batch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := converter.ConvertTrack(ctx, track); !errors.Is(err, context.Canceled) || requests != 2 {
		t.Fatalf("canceled conversion made a request or returned the wrong error: %v", err)
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}
