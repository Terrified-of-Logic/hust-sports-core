package hustrun

import (
	"fmt"
	"strings"
	"time"
)

// ShiftToDate moves every timestamp of a record onto targetDate, keeping the
// time of day. targetDate is "original", "today" or "YYYY-MM-DD".
func ShiftToDate(record SportRecord, targetDate string, reference time.Time) (SportRecord, error) {
	if record.StartDateLong == 0 || record.EndDateLong == 0 {
		return SportRecord{}, fmt.Errorf("%w: record has no usable timestamps", ErrInvalidRecord)
	}
	if _, err := resolveTargetDate(targetDate, reference); err != nil {
		return SportRecord{}, err
	}
	offsetMS := ComputeDateOffsetMS(record.StartDateLong, targetDate, reference)

	shifted := record
	shifted.StartDateLong = record.StartDateLong + offsetMS
	shifted.EndDateLong = record.EndDateLong + offsetMS
	shifted.StartDate = FormatServerTime(shifted.StartDateLong)
	shifted.EndDate = FormatServerTime(shifted.EndDateLong)

	shifted.Points = make([]TrackPoint, len(record.Points))
	copy(shifted.Points, record.Points)
	for index := range shifted.Points {
		if shifted.Points[index].T > 0 {
			shifted.Points[index].T += offsetMS
		}
	}
	shifted.Pace = append([]PaceEntry{}, record.Pace...)
	shifted.MinuteSpeed = append([]MinuteSpeedEntry{}, record.MinuteSpeed...)
	return shifted, nil
}

// ComputeDateOffsetMS returns the millisecond offset between the record's day
// and the target day.
func ComputeDateOffsetMS(origDateLong int64, targetDate string, reference time.Time) int64 {
	original := time.UnixMilli(origDateLong).In(ServerLocation)
	target := original
	switch strings.ToLower(strings.TrimSpace(targetDate)) {
	case "", "original":
		target = original
	case "today":
		target = reference.In(ServerLocation)
	default:
		if parsed, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(targetDate), ServerLocation); err == nil {
			target = parsed
		}
	}
	originalMidnight := time.Date(original.Year(), original.Month(), original.Day(), 0, 0, 0, 0, ServerLocation)
	targetMidnight := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, ServerLocation)
	return targetMidnight.Sub(originalMidnight).Milliseconds()
}

func resolveTargetDate(targetDate string, reference time.Time) (time.Time, error) {
	switch strings.ToLower(strings.TrimSpace(targetDate)) {
	case "", "original", "today":
		return reference.In(ServerLocation), nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(targetDate), ServerLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: 无效的日期格式: %q，请使用 'today' 或 'YYYY-MM-DD'", ErrInvalidRecord, targetDate)
	}
	return parsed, nil
}

// ValidateTimeWindow reports whether the record lies inside the official
// 06:00–23:00 window.
func ValidateTimeWindow(record SportRecord) bool {
	if record.StartDateLong == 0 || record.EndDateLong == 0 {
		return false
	}
	start := time.UnixMilli(record.StartDateLong).In(ServerLocation)
	end := time.UnixMilli(record.EndDateLong).In(ServerLocation)
	startHour := float64(start.Hour()) + float64(start.Minute())/60
	endHour := float64(end.Hour()) + float64(end.Minute())/60
	return startHour >= 6.0 && startHour <= 23.0 && endHour >= 6.0 && endHour <= 23.0
}
