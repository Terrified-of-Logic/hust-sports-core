package hustrun

import (
	"testing"
	"time"
)

func generateForTest(t *testing.T, distance float64, seed int64) SportRecord {
	t.Helper()
	route, err := LoadRoute("testdata/route_track.json")
	if err != nil {
		t.Fatalf("LoadRoute: %v", err)
	}
	record, err := GenerateRouteRecord(route, GenerateOptions{
		StartMS:         1784961322000,
		SportID:         "00000000-0000-4000-8000-000000000001",
		XH:              "U000000000",
		TargetDistanceM: distance,
		Seed:            &seed,
	})
	if err != nil {
		t.Fatalf("GenerateRouteRecord: %v", err)
	}
	return record
}

func TestGenerateAllRoutesAreConsistent(t *testing.T) {
	for _, path := range []string{"testdata/route_track.json", "routes/route_stadium.json"} {
		route, err := LoadRoute(path)
		if err != nil {
			t.Fatalf("LoadRoute(%s): %v", path, err)
		}
		seed := int64(11)
		record, err := GenerateRouteRecord(route, GenerateOptions{
			StartMS:         1784961322000,
			SportID:         "00000000-0000-4000-8000-000000000001",
			XH:              "U000000000",
			TargetDistanceM: 2150,
			Seed:            &seed,
		})
		if err != nil {
			t.Fatalf("GenerateRouteRecord(%s): %v", path, err)
		}
		if issues := ValidateRecordConsistency(record); len(issues) > 0 {
			t.Fatalf("%s produced an inconsistent record: %v", path, issues)
		}
		if last := record.Points[len(record.Points)-1].DA; last < 2000 {
			t.Errorf("%s: final da = %.1f m, want >= 2000", path, last)
		}
		if record.ActivityStatus != 1 || record.Result != 1 {
			t.Errorf("%s: flags = activity %d result %d", path, record.ActivityStatus, record.Result)
		}
	}
}

func TestGenerateSpeedStatisticsLookHuman(t *testing.T) {
	record := generateForTest(t, 2150, 3)

	distinct := map[float64]struct{}{}
	minimum, maximum := 1e9, 0.0
	for _, point := range record.Points {
		if point.S <= 0 {
			continue
		}
		distinct[point.S] = struct{}{}
		minimum = min(minimum, point.S)
		maximum = max(maximum, point.S)
	}
	if len(distinct) < 50 {
		t.Errorf("只有 %d 个不同的瞬时速度，轨迹过于规整", len(distinct))
	}
	if minimum < 13.0 || maximum > 19.0 {
		t.Errorf("瞬时速度范围 %.2f–%.2f km/h 超出 4.0–5.0 m/s", minimum, maximum)
	}

	var weightedSum, weightedTime float64
	for index := 2; index < len(record.Points); index++ {
		previous := record.Points[index-1]
		point := record.Points[index]
		weight := float64(point.T-previous.T) / 1000
		weightedSum += point.S * weight
		weightedTime += weight
	}
	average, err := parseFloat(record.AvgSpeed)
	if err != nil {
		t.Fatalf("avg speed: %v", err)
	}
	if delta := weightedSum/weightedTime - average; delta > 0.6 || delta < -0.6 {
		t.Errorf("速度加权平均 %.2f 与 avgSpeed %.2f 相差 %.2f", weightedSum/weightedTime, average, delta)
	}
}

func TestGenerateAccuracyVariesAndSamplingIsOneHertz(t *testing.T) {
	record := generateForTest(t, 2150, 5)
	accuracies := map[float64]struct{}{}
	for _, point := range record.Points {
		if point.T > 0 {
			accuracies[point.AC] = struct{}{}
			if point.AC < 4.0 || point.AC > 12.0 {
				t.Fatalf("accuracy %.1f outside the requested range", point.AC)
			}
		}
	}
	if len(accuracies) < 20 {
		t.Errorf("only %d distinct accuracies", len(accuracies))
	}
	for index := 2; index < len(record.Points); index++ {
		delta := float64(record.Points[index].T-record.Points[index-1].T) / 1000
		if delta < 0.9 || delta > 1.1 {
			t.Fatalf("sampling interval %.2fs at point %d", delta, index)
		}
	}
	if record.Points[0].T != 0 || record.Points[0].S != 0 || record.Points[0].DA != 0 {
		t.Error("the placeholder first point must be all zero")
	}
}

func TestGenerateSpeedBoundFieldsMatchPatchedClient(t *testing.T) {
	record := generateForTest(t, 2150, 13)
	if record.MaxSpeedPerHour != record.AvgSpeed {
		t.Errorf("maxSpeedPerHour = %q, want avgSpeed %q", record.MaxSpeedPerHour, record.AvgSpeed)
	}
	if record.MinSpeedPerHour != "6.0" {
		t.Errorf("minSpeedPerHour = %q, want the clamped 6.0", record.MinSpeedPerHour)
	}
}

func TestGenerateMinuteSpeedChanges(t *testing.T) {
	record := generateForTest(t, 2150, 101)
	if len(record.MinuteSpeed) < 5 {
		t.Fatalf("only %d minute rows", len(record.MinuteSpeed))
	}
	distinct := map[string]struct{}{}
	for _, entry := range record.MinuteSpeed {
		distinct[entry.V] = struct{}{}
	}
	if len(distinct)*2 <= len(record.MinuteSpeed) {
		t.Errorf("minute speeds barely change: %v", record.MinuteSpeed)
	}
}

func TestGenerateHasDistanceMarginAndPaceRows(t *testing.T) {
	record := generateForTest(t, 2150, 7)
	payload, err := BuildPayloadJSON(&record, AuthConfig{Name: "测试", PhoneVersion: "PJE110,35,15|1.0.9"})
	if err != nil {
		t.Fatalf("BuildPayloadJSON: %v", err)
	}
	if len(payload) < 1000 {
		t.Fatalf("payload suspiciously small: %d bytes", len(payload))
	}
	odometer, err := parseFloat(record.DLLC)
	if err != nil || odometer < 2.0 {
		t.Errorf("odometer = %q, want >= 2.0 km", record.DLLC)
	}
	if len(record.Pace) < 2 {
		t.Errorf("pace rows = %d, want at least 2", len(record.Pace))
	}
	if record.DurationSeconds() < 400 {
		t.Errorf("duration = %d s, want a realistic run", record.DurationSeconds())
	}
}

func TestGenerateIsDeterministicForASeed(t *testing.T) {
	first := generateForTest(t, 2150, 42)
	second := generateForTest(t, 2150, 42)
	other := generateForTest(t, 2150, 43)

	firstPayload, _ := BuildPayloadJSON(&first, AuthConfig{})
	secondPayload, _ := BuildPayloadJSON(&second, AuthConfig{})
	otherPayload, _ := BuildPayloadJSON(&other, AuthConfig{})
	if firstPayload != secondPayload {
		t.Fatal("the same seed produced different payloads")
	}
	if firstPayload == otherPayload {
		t.Fatal("different seeds produced the same payload")
	}
}

func TestGenerateWithoutSeedUsesFreshEntropy(t *testing.T) {
	route, err := LoadRoute("testdata/route_track.json")
	if err != nil {
		t.Fatalf("LoadRoute: %v", err)
	}
	build := func() string {
		record, err := GenerateRouteRecord(route, GenerateOptions{
			StartMS:         1784961322000,
			SportID:         "00000000-0000-4000-8000-00000000000b",
			XH:              "U000000000",
			TargetDistanceM: 400,
		})
		if err != nil {
			t.Fatalf("GenerateRouteRecord: %v", err)
		}
		payload, err := BuildPayloadJSON(&record, AuthConfig{})
		if err != nil {
			t.Fatalf("BuildPayloadJSON: %v", err)
		}
		return payload
	}
	if build() == build() {
		t.Fatal("two unseeded runs produced identical payloads")
	}
}

func TestMakeSensorTrace(t *testing.T) {
	trace := MakeSensorTrace(SensorTraceOptions{DurationSeconds: 500, Seed: int64Ptr(1)})
	stats, err := AnalyzeSensorTrace(trace)
	if err != nil {
		t.Fatalf("AnalyzeSensorTrace: %v", err)
	}
	if stats["count"] != 5000 {
		t.Errorf("sample count = %.0f, want 5000", stats["count"])
	}
	if stats["min"] <= 0 || stats["max"] >= 40 {
		t.Errorf("sensor range %.2f–%.2f is implausible", stats["min"], stats["max"])
	}
	again := MakeSensorTrace(SensorTraceOptions{DurationSeconds: 500, Seed: int64Ptr(1)})
	if again != trace {
		t.Error("sensor trace is not deterministic for a seed")
	}
}

func TestLoadRouteRejectsBadFiles(t *testing.T) {
	if _, err := LoadRoute("testdata/does-not-exist.json"); err == nil {
		t.Error("missing route accepted")
	}
}

func TestRouteGeometryPointAtClosesTheLoop(t *testing.T) {
	route, err := LoadRoute("testdata/route_track.json")
	if err != nil {
		t.Fatalf("LoadRoute: %v", err)
	}
	startLat, startLon, _ := route.PointAt(0)
	endLat, endLon, _ := route.PointAt(route.LengthM)
	if HaversineDistanceM(startLat, startLon, endLat, endLon) > 1.0 {
		t.Errorf("wrapping the loop left a %.2f m gap", HaversineDistanceM(startLat, startLon, endLat, endLon))
	}
}

func TestFormatServerTimeUsesChinaZone(t *testing.T) {
	// 2026-10-25 02:00:00 UTC is 10:00 in the server zone.
	ms := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC).UnixMilli()
	if got := FormatServerTime(ms); got != "2026-10-25 10:00:00" {
		t.Errorf("FormatServerTime = %q", got)
	}
	if ServerLocation == time.Local {
		t.Fatal("the server zone must not depend on the host zone")
	}
}

func int64Ptr(value int64) *int64 { return &value }
