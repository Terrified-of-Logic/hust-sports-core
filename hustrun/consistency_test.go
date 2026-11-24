package hustrun

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRecordConsistencyAcceptsGeneratedRecord(t *testing.T) {
	record := generateForTest(t, 2150, 21)
	if issues := ValidateRecordConsistency(record); len(issues) > 0 {
		t.Fatalf("issues: %v", issues)
	}
	if err := AssertRecordConsistent(record); err != nil {
		t.Fatalf("AssertRecordConsistent: %v", err)
	}
}

func TestValidateRecordConsistencyDetectsAverageSpeedDrift(t *testing.T) {
	record := generateForTest(t, 1000, 22)
	record.AvgSpeed = "30"
	issues := ValidateRecordConsistency(record)
	if !hasIssue(issues, "speed.average") {
		t.Fatalf("issues = %v, want speed.average", issues)
	}
	err := AssertRecordConsistent(record)
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("error = %v, want ErrInvalidRecord", err)
	}
	var consistency *RecordConsistencyError
	if !errors.As(err, &consistency) || len(consistency.Issues) == 0 {
		t.Fatalf("error does not carry issues: %v", err)
	}
}

func TestValidateRecordConsistencyDetectsTrackProblems(t *testing.T) {
	record := generateForTest(t, 1000, 23)

	unsorted := record
	unsorted.Points = append([]TrackPoint{}, record.Points...)
	unsorted.Points[3].T = unsorted.Points[2].T - 5000
	if issues := ValidateRecordConsistency(unsorted); !hasIssue(issues, "track.time") {
		t.Errorf("issues = %v, want track.time", issues)
	}

	imprecise := record
	imprecise.Points = append([]TrackPoint{}, record.Points...)
	imprecise.Points[2].A += 1e-9
	if issues := ValidateRecordConsistency(imprecise); !hasIssue(issues, "track.coordinate_precision") {
		t.Errorf("issues = %v, want track.coordinate_precision", issues)
	}

	outOfRange := record
	outOfRange.Points = append([]TrackPoint{}, record.Points...)
	outOfRange.Points[1].A = 120
	if issues := ValidateRecordConsistency(outOfRange); !hasIssue(issues, "track.coordinate") {
		t.Errorf("issues = %v, want track.coordinate", issues)
	}

	fastPoint := record
	fastPoint.Points = append([]TrackPoint{}, record.Points...)
	fastPoint.Points[2].S = 120
	if issues := ValidateRecordConsistency(fastPoint); !hasIssue(issues, "track.segment_plausibility") {
		t.Errorf("issues = %v, want track.segment_plausibility", issues)
	}
}

func TestValidateRecordConsistencyDetectsPaceProblems(t *testing.T) {
	record := generateForTest(t, 2150, 24)
	if len(record.Pace) < 2 {
		t.Skip("need at least two pace rows")
	}
	record.Pace[1].KmDLSJ = record.Pace[0].KmDLSJ
	if issues := ValidateRecordConsistency(record); !hasIssue(issues, "pace.cumulative") {
		t.Errorf("issues = %v, want pace.cumulative", issues)
	}

	record = generateForTest(t, 2150, 24)
	record.Pace[0].T = "04'10"
	if issues := ValidateRecordConsistency(record); !hasIssue(issues, "pace.segment_format") {
		t.Errorf("issues = %v, want pace.segment_format", issues)
	}
}

func TestValidateRecordConsistencyDetectsBounds(t *testing.T) {
	record := generateForTest(t, 1000, 25)
	record.MinSpeedPerHour = "20"
	if issues := ValidateRecordConsistency(record); !hasIssue(issues, "speed.bounds") {
		t.Errorf("issues = %v, want speed.bounds", issues)
	}
}

func TestValidateRecordServerTime(t *testing.T) {
	record := generateForTest(t, 1000, 26)
	if issues := ValidateRecordServerTime(record, 0, 120000); !hasIssue(issues, "time.server") {
		t.Errorf("issues = %v, want time.server", issues)
	}
	if issues := ValidateRecordServerTime(record, record.EndDateLong-600000, 120000); len(issues) == 0 {
		t.Error("a record ending after the server clock was accepted")
	}
	if issues := ValidateRecordServerTime(record, record.EndDateLong+1000, 120000); len(issues) > 0 {
		t.Errorf("a finished record was rejected: %v", issues)
	}
	if err := AssertRecordNotInServerFuture(record, record.EndDateLong+1000, 120000); err != nil {
		t.Errorf("AssertRecordNotInServerFuture: %v", err)
	}
	if err := AssertRecordNotInServerFuture(record, record.EndDateLong-600000, 120000); err == nil {
		t.Error("assertion accepted a future record")
	}
}

func TestHaversineAndParsers(t *testing.T) {
	// One degree of latitude at the equator is about 111.2 km.
	if got := HaversineDistanceM(0, 0, 1, 0); got < 111100 || got > 111300 {
		t.Errorf("haversine one degree = %.1f m", got)
	}
	if seconds, ok := ParsePaceSeconds("04'10''"); !ok || seconds != 250 {
		t.Errorf("ParsePaceSeconds = %d, %t", seconds, ok)
	}
	if _, ok := ParsePaceSeconds("04'10"); ok {
		t.Error("unterminated pace was accepted")
	}
	if seconds, ok := ParseDurationSeconds("00:13:24"); !ok || seconds != 804 {
		t.Errorf("ParseDurationSeconds = %d, %t", seconds, ok)
	}
	if _, ok := ParseDurationSeconds("13:24"); ok {
		t.Error("short duration was accepted")
	}
	if got := FormatPace(0); got != "00'01''" {
		t.Errorf("FormatPace(0) = %q", got)
	}
}

func TestRecordHelpers(t *testing.T) {
	record := generateForTest(t, 1000, 27)
	if !strings.Contains(record.Summary(), "轨迹点") {
		t.Errorf("summary = %q", record.Summary())
	}
	if record.IsValidRecord() {
		t.Error("a 1 km record must not report itself as valid")
	}
	long := generateForTest(t, 3100, 28)
	if !long.IsValidRecord() {
		t.Error("a 3.1 km record should satisfy the coarse validity shape")
	}
}

func hasIssue(issues []ConsistencyIssue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
