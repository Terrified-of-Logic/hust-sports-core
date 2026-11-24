package hustrun

import (
	"math"
	"testing"
	"time"
)

func mutateForTest(t *testing.T, record SportRecord, reference int64, seed int64) SportRecord {
	t.Helper()
	options := DefaultMutateOptions()
	options.Seed = &seed
	options.ReferenceTimeMS = &reference
	mutated, err := MutateRecord(record, options)
	if err != nil {
		t.Fatalf("MutateRecord: %v", err)
	}
	return mutated
}

func TestMutateRecordProducesConsistentCopy(t *testing.T) {
	source := generateForTest(t, 3000, 31)
	reference := time.Date(2026, 10, 25, 20, 0, 0, 0, ServerLocation).UnixMilli()

	mutated := mutateForTest(t, source, reference, 5)
	if issues := ValidateRecordConsistency(mutated); len(issues) > 0 {
		t.Fatalf("mutated record is inconsistent: %v", issues)
	}
	if mutated.SportID == source.SportID {
		t.Error("mutation kept the source sport id")
	}
	if len(mutated.SportID) != 36 || mutated.SportID[14] != '4' {
		t.Errorf("sport id %q is not UUID v4 shaped", mutated.SportID)
	}
	if len(mutated.Points) != len(source.Points) {
		t.Errorf("point count changed: %d -> %d", len(source.Points), len(mutated.Points))
	}
	if mutated.ActivityStatus != 1 || mutated.Result != 1 {
		t.Errorf("flags = %d / %d", mutated.ActivityStatus, mutated.Result)
	}

	// The geometry must actually move.
	moved := 0
	for index := range mutated.Points {
		delta := HaversineDistanceM(source.Points[index].A, source.Points[index].O,
			mutated.Points[index].A, mutated.Points[index].O)
		if delta > 0.5 {
			moved++
		}
	}
	if moved < len(source.Points)/2 {
		t.Errorf("only %d/%d points moved", moved, len(source.Points))
	}

	// Timestamps stay strictly increasing and inside the safe window.
	for index := 1; index < len(mutated.Points); index++ {
		if mutated.Points[index].T <= mutated.Points[index-1].T {
			t.Fatalf("timestamps not increasing at %d", index)
		}
	}
	start := time.UnixMilli(mutated.StartDateLong).In(ServerLocation)
	if start.Hour() < 7 || start.Hour() >= 22 {
		t.Errorf("start %s is outside the safe window", mutated.StartDate)
	}
	if mutated.EndDateLong > reference {
		t.Errorf("record ends in the server's future: %d > %d", mutated.EndDateLong, reference)
	}
	if mutated.StartDate != FormatServerTime(mutated.StartDateLong) {
		t.Errorf("start string %q does not match the timestamp", mutated.StartDate)
	}
}

func TestMutateRecordIsDeterministic(t *testing.T) {
	source := generateForTest(t, 1500, 32)
	reference := time.Date(2026, 10, 25, 12, 0, 0, 0, ServerLocation).UnixMilli()

	first := mutateForTest(t, source, reference, 77)
	second := mutateForTest(t, source, reference, 77)
	other := mutateForTest(t, source, reference, 78)

	firstPayload, _ := BuildPayloadJSON(&first, AuthConfig{})
	secondPayload, _ := BuildPayloadJSON(&second, AuthConfig{})
	otherPayload, _ := BuildPayloadJSON(&other, AuthConfig{})
	if firstPayload != secondPayload {
		t.Error("identical seeds produced different records")
	}
	if firstPayload == otherPayload {
		t.Error("different seeds produced the same record")
	}
}

func TestMutateRecordKeepsOdometerAndRebuildsDerivedRows(t *testing.T) {
	source := generateForTest(t, 3000, 33)
	reference := time.Date(2026, 10, 25, 18, 0, 0, 0, ServerLocation).UnixMilli()
	mutated := mutateForTest(t, source, reference, 9)

	if mutated.DLLC != source.DLLC {
		t.Errorf("odometer changed: %q -> %q", source.DLLC, mutated.DLLC)
	}
	if len(mutated.Pace) != len(source.Pace) {
		t.Errorf("pace rows changed: %d -> %d", len(source.Pace), len(mutated.Pace))
	}
	if len(mutated.MinuteSpeed) == 0 {
		t.Error("minute speed rows were dropped")
	}
	if mutated.Pace[0].KmDLSJ <= 0 {
		t.Errorf("first pace row = %+v", mutated.Pace[0])
	}

	// avgSpeed must follow the rebuilt cumulative distance.
	distanceKm := mutated.Points[len(mutated.Points)-1].DA / 1000
	activeSeconds, ok := ParseDurationSeconds(mutated.DLTime)
	if !ok {
		t.Fatalf("duration %q is invalid", mutated.DLTime)
	}
	average, err := parseFloat(mutated.AvgSpeed)
	if err != nil {
		t.Fatalf("avg speed: %v", err)
	}
	expected := distanceKm / (float64(activeSeconds) / 3600)
	if math.Abs(average-expected) > 0.25 {
		t.Errorf("avgSpeed %.2f vs %.2f", average, expected)
	}
}

func TestMutateRecordWithoutSeedStillWorks(t *testing.T) {
	source := generateForTest(t, 1200, 37)
	reference := time.Date(2026, 10, 25, 16, 0, 0, 0, ServerLocation).UnixMilli()
	options := DefaultMutateOptions()
	options.ReferenceTimeMS = &reference
	mutated, err := MutateRecord(source, options)
	if err != nil {
		t.Fatalf("MutateRecord without a seed: %v", err)
	}
	if issues := ValidateRecordConsistency(mutated); len(issues) > 0 {
		t.Fatalf("issues: %v", issues)
	}
	if mutated.SportID == source.SportID {
		t.Error("sport id was not regenerated")
	}
}

func TestMutateRecordRejectsBadInput(t *testing.T) {
	source := generateForTest(t, 500, 34)
	reference := time.Date(2026, 10, 25, 20, 0, 0, 0, ServerLocation).UnixMilli()
	seed := int64(1)

	options := DefaultMutateOptions()
	options.Seed = &seed
	options.ReferenceTimeMS = &reference
	options.TimeJitterPct = 1.5
	if _, err := MutateRecord(source, options); err == nil {
		t.Error("out-of-range jitter accepted")
	}

	options = DefaultMutateOptions()
	options.Seed = &seed
	badReference := int64(-5)
	options.ReferenceTimeMS = &badReference
	if _, err := MutateRecord(source, options); err == nil {
		t.Error("negative reference time accepted")
	}

	if _, err := MutateRecord(SportRecord{}, DefaultMutateOptions()); err == nil {
		t.Error("empty record accepted")
	}
}

func TestShiftToDatePreservesTimeOfDay(t *testing.T) {
	source := generateForTest(t, 500, 35)
	reference := time.Date(2026, 11, 3, 9, 0, 0, 0, ServerLocation)
	shifted, err := ShiftToDate(source, "2026-11-03", reference)
	if err != nil {
		t.Fatalf("ShiftToDate: %v", err)
	}
	start := time.UnixMilli(shifted.StartDateLong).In(ServerLocation)
	if start.Year() != 2026 || start.Month() != time.November || start.Day() != 3 {
		t.Errorf("shifted start = %s", shifted.StartDate)
	}
	original := time.UnixMilli(source.StartDateLong).In(ServerLocation)
	if start.Hour() != original.Hour() || start.Minute() != original.Minute() {
		t.Errorf("time of day changed: %s -> %s", original.Format("15:04"), start.Format("15:04"))
	}
	if got := ComputeDateOffsetMS(source.StartDateLong, "2026-11-03", reference); got%int64(24*time.Hour/time.Millisecond) != 0 {
		t.Errorf("offset %d ms is not a whole number of days", got)
	}
	if _, err := ShiftToDate(source, "not-a-date", reference); err == nil {
		t.Error("invalid date accepted")
	}
	if _, err := ShiftToDate(SportRecord{}, "today", reference); err == nil {
		t.Error("record without timestamps accepted")
	}
}

func TestValidateTimeWindow(t *testing.T) {
	record := generateForTest(t, 500, 36)
	if !ValidateTimeWindow(record) {
		t.Errorf("record %s -> %s should be inside the window", record.StartDate, record.EndDate)
	}
	record.StartDateLong = time.Date(2026, 10, 25, 3, 0, 0, 0, ServerLocation).UnixMilli()
	if ValidateTimeWindow(record) {
		t.Error("a 03:00 record was accepted")
	}
}
