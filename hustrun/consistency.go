package hustrun

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ConsistencyIssue is one locatable internal inconsistency of a record.
type ConsistencyIssue struct {
	Code    string
	Message string
}

// RecordConsistencyError reports every issue found by the local checks.
type RecordConsistencyError struct {
	Issues []ConsistencyIssue
}

func (e *RecordConsistencyError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.Code + ": " + issue.Message
	}
	return "运动记录内部不一致: " + strings.Join(parts, "; ")
}

// Is lets errors.Is(err, ErrInvalidRecord) match consistency failures.
func (e *RecordConsistencyError) Is(target error) bool { return target == ErrInvalidRecord }

// Unwrap keeps errors.Is working through wrapping.
func (e *RecordConsistencyError) Unwrap() error { return ErrInvalidRecord }

var (
	pacePattern     = regexp.MustCompile(`^(\d+)'([0-5]\d)''$`)
	durationPattern = regexp.MustCompile(`^(\d+):([0-5]\d):([0-5]\d)$`)
)

// ParsePaceSeconds parses 04'08” into seconds per kilometre.
func ParsePaceSeconds(value string) (int, bool) {
	match := pacePattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, false
	}
	minutes, _ := strconv.Atoi(match[1])
	seconds, _ := strconv.Atoi(match[2])
	return minutes*60 + seconds, true
}

// FormatPace renders seconds per kilometre as 04'08”.
func FormatPace(seconds float64) string {
	rounded := int(math.Round(seconds))
	if rounded < 1 {
		rounded = 1
	}
	return fmt.Sprintf("%02d'%02d''", rounded/60, rounded%60)
}

// ParseDurationSeconds parses HH:MM:SS into seconds.
func ParseDurationSeconds(value string) (int, bool) {
	match := durationPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, false
	}
	hours, _ := strconv.Atoi(match[1])
	minutes, _ := strconv.Atoi(match[2])
	seconds, _ := strconv.Atoi(match[3])
	return hours*3600 + minutes*60 + seconds, true
}

const earthRadiusM = 6371000.0

// HaversineDistanceM returns the spherical distance between two WGS84 points.
func HaversineDistanceM(latA, lonA, latB, lonB float64) float64 {
	phiA := latA * math.Pi / 180
	phiB := latB * math.Pi / 180
	deltaPhi := phiB - phiA
	deltaLambda := (lonB - lonA) * math.Pi / 180
	haversine := math.Sin(deltaPhi/2)*math.Sin(deltaPhi/2) +
		math.Cos(phiA)*math.Cos(phiB)*math.Sin(deltaLambda/2)*math.Sin(deltaLambda/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(haversine)))
}

// CoordinateDistanceM sums the haversine length of a record's coordinate
// polyline.
func CoordinateDistanceM(record SportRecord) float64 {
	var total float64
	for i := 1; i < len(record.Points); i++ {
		previous := record.Points[i-1]
		current := record.Points[i]
		total += HaversineDistanceM(previous.A, previous.O, current.A, current.O)
	}
	return total
}

// ValidateRecordConsistency returns every internal inconsistency that the
// payload alone can prove. An empty slice means the record is self-consistent;
// it is not a prediction of the server's verdict.
func ValidateRecordConsistency(record SportRecord) []ConsistencyIssue {
	var issues []ConsistencyIssue
	wallDuration := record.DurationSeconds()
	activeDuration, hasActive := ParseDurationSeconds(record.DLTime)
	duration := wallDuration
	if hasActive && activeDuration > 0 {
		duration = activeDuration
	}
	distanceKm := record.DistanceKm()
	statisticsDistanceKm := distanceKm
	if len(record.Points) > 0 && record.Points[len(record.Points)-1].DA > 0 {
		statisticsDistanceKm = record.Points[len(record.Points)-1].DA / 1000.0
	}

	if duration <= 0 {
		issues = append(issues, ConsistencyIssue{"time.duration", "结束时间必须晚于开始时间"})
	}
	if distanceKm <= 0 {
		issues = append(issues, ConsistencyIssue{"distance.total", "总里程必须大于 0"})
	}
	if len(record.Points) < 2 {
		issues = append(issues, ConsistencyIssue{"track.count", "轨迹点至少需要 2 个"})
	}

	if duration > 0 && statisticsDistanceKm > 0 {
		expectedSpeed := statisticsDistanceKm / (float64(duration) / 3600.0)
		actualSpeed, speedErr := parseFloat(record.AvgSpeed)
		tolerance := math.Max(0.2, expectedSpeed*0.02)
		if speedErr != nil || math.IsNaN(actualSpeed) || math.IsInf(actualSpeed, 0) {
			issues = append(issues, ConsistencyIssue{"speed.average", "平均速度不是有效数值"})
		} else if math.Abs(actualSpeed-expectedSpeed) > tolerance {
			issues = append(issues, ConsistencyIssue{"speed.average", fmt.Sprintf(
				"平均速度 %.2f 与累计距离/时长推导值 %.2f km/h 不符", actualSpeed, expectedSpeed)})
		} else if actualSpeed > 50.0 {
			issues = append(issues, ConsistencyIssue{"speed.plausibility", fmt.Sprintf(
				"平均速度 %.2f km/h 超出步行/跑步合理范围", actualSpeed)})
		}

		actualPace, paceOK := ParsePaceSeconds(record.AvgPace)
		expectedPace := float64(duration) / statisticsDistanceKm
		if !paceOK {
			issues = append(issues, ConsistencyIssue{"pace.average", "平均配速格式无效"})
		} else if math.Abs(float64(actualPace)-expectedPace) > 3.0 {
			issues = append(issues, ConsistencyIssue{"pace.average", fmt.Sprintf(
				"平均配速 %d 秒/公里与累计距离/时长推导值 %.1f 秒/公里不符", actualPace, expectedPace)})
		}

		expectedCadence := float64(record.TotalStep) / (float64(duration) / 60.0)
		actualCadence, cadenceErr := parseFloat(record.StepMinute)
		if cadenceErr != nil || math.IsNaN(actualCadence) || math.IsInf(actualCadence, 0) {
			issues = append(issues, ConsistencyIssue{"step.cadence", "步频不是有效数值"})
		} else if math.Abs(actualCadence-expectedCadence) > 0.2 {
			issues = append(issues, ConsistencyIssue{"step.cadence", fmt.Sprintf(
				"步频 %.1f 与步数/时长推导值 %.1f 不符", actualCadence, expectedCadence)})
		}
	}

	if len(record.Points) > 0 {
		var previousPoint *TrackPoint
		segmentSum := 0.0
		validCoordinates := true
		for index := range record.Points {
			point := record.Points[index]
			if math.IsNaN(point.A) || math.IsNaN(point.O) || math.IsInf(point.A, 0) || math.IsInf(point.O, 0) ||
				point.A < -90 || point.A > 90 || point.O < -180 || point.O > 180 {
				validCoordinates = false
				issues = append(issues, ConsistencyIssue{"track.coordinate", fmt.Sprintf(
					"第 %d 个轨迹点经纬度无效", index+1)})
				break
			}
			if math.Abs(point.A-math.Round(point.A*1e6)/1e6) > 1e-9 ||
				math.Abs(point.O-math.Round(point.O*1e6)/1e6) > 1e-9 {
				issues = append(issues, ConsistencyIssue{"track.coordinate_precision", fmt.Sprintf(
					"第 %d 个轨迹点超过 App 使用的 6 位小数精度", index+1)})
				break
			}
			if point.D < 0 || point.DA < 0 {
				issues = append(issues, ConsistencyIssue{"track.distance", fmt.Sprintf(
					"第 %d 个轨迹点距离为负数", index+1)})
				break
			}
			if math.IsNaN(point.S) || math.IsInf(point.S, 0) || point.S < 0 {
				issues = append(issues, ConsistencyIssue{"track.speed", fmt.Sprintf(
					"第 %d 个轨迹点速度不是有效的非负数", index+1)})
				break
			}
			if point.S > 50.0 {
				issues = append(issues, ConsistencyIssue{"track.segment_plausibility", fmt.Sprintf(
					"第 %d 个轨迹点瞬时速度 %.2f km/h 超出步行/跑步合理范围", index+1, point.S)})
				break
			}
			if previousPoint != nil {
				if point.T < previousPoint.T {
					issues = append(issues, ConsistencyIssue{"track.time", "轨迹点时间戳不是单调递增"})
					break
				}
				if point.DA < previousPoint.DA {
					issues = append(issues, ConsistencyIssue{"track.cumulative", "累计距离 da 出现倒退"})
					break
				}
				deltaMS := point.T - previousPoint.T
				distanceDelta := point.DA - previousPoint.DA
				if math.Abs(point.D-distanceDelta) > math.Max(3.0, point.D*0.05) {
					issues = append(issues, ConsistencyIssue{"track.segment_cumulative", fmt.Sprintf(
						"第 %d 个轨迹点 d=%.1f m 与 da 增量 %.1f m 不符", index+1, point.D, distanceDelta)})
					break
				}
				if deltaMS <= 0 && point.D > 0 {
					issues = append(issues, ConsistencyIssue{"track.segment_time", fmt.Sprintf(
						"第 %d 个轨迹段有距离但没有正时间间隔", index+1)})
					break
				}
			}
			previousPoint = &record.Points[index]
			segmentSum += point.D
		}

		if duration > 0 {
			minimumTime := int64(math.MaxInt64)
			maximumTime := int64(math.MinInt64)
			var timed bool
			for _, point := range record.Points {
				if point.T <= 0 {
					continue
				}
				timed = true
				minimumTime = min(minimumTime, point.T)
				maximumTime = max(maximumTime, point.T)
			}
			const toleranceMS = int64(5000)
			if timed && (minimumTime < record.StartDateLong-toleranceMS ||
				maximumTime > record.EndDateLong+toleranceMS) {
				issues = append(issues, ConsistencyIssue{"track.time_window", "轨迹点时间戳超出运动起止时间"})
			}
		}

		lastDA := record.Points[len(record.Points)-1].DA
		if lastDA > 0 {
			distanceTolerance := math.Max(50.0, lastDA*0.05)
			if math.Abs(segmentSum-lastDA) > distanceTolerance {
				issues = append(issues, ConsistencyIssue{"track.d_da", fmt.Sprintf(
					"分段距离之和 %.1f m 与最终 da %.1f m 不符", segmentSum, lastDA)})
			}
			if math.Abs(lastDA/1000.0-distanceKm) > math.Max(0.1, distanceKm*0.05) {
				issues = append(issues, ConsistencyIssue{"track.odometer", fmt.Sprintf(
					"最终 da %.3f km 与 odometer %.3f km 不符", lastDA/1000.0, distanceKm)})
			}
			if validCoordinates && len(record.Points) >= 2 {
				geometry := CoordinateDistanceM(record)
				if math.Abs(geometry-lastDA) > math.Max(100.0, lastDA*0.20) {
					issues = append(issues, ConsistencyIssue{"track.geometry", fmt.Sprintf(
						"坐标折线 %.3f km 与最终 da %.3f km 差异过大", geometry/1000.0, lastDA/1000.0)})
				}
			}
		}
	}

	previousCumulative := 0
	for index, pace := range record.Pace {
		if pace.KmDLSJ <= 0 {
			continue
		}
		if pace.KmDLSJ <= previousCumulative {
			issues = append(issues, ConsistencyIssue{"pace.cumulative", "分公里累计时间不是严格递增"})
			break
		}
		segmentSeconds := pace.KmDLSJ - previousCumulative
		paceSeconds, ok := ParsePaceSeconds(pace.T)
		if !ok {
			issues = append(issues, ConsistencyIssue{"pace.segment_format", fmt.Sprintf(
				"第 %d 段配速格式无效", index+1)})
			break
		}
		if absInt(paceSeconds-segmentSeconds) > 2 {
			issues = append(issues, ConsistencyIssue{"pace.segment", fmt.Sprintf(
				"第 %d 段配速 %d 秒与累计时间差 %d 秒不符", index+1, paceSeconds, segmentSeconds)})
			break
		}
		previousCumulative = pace.KmDLSJ
	}

	minimum, minErr := parseFloat(record.MinSpeedPerHour)
	average, avgErr := parseFloat(record.AvgSpeed)
	maximum, maxErr := parseFloat(record.MaxSpeedPerHour)
	if minErr == nil && avgErr == nil && maxErr == nil && !(minimum <= average && average <= maximum) {
		issues = append(issues, ConsistencyIssue{"speed.bounds", "最小速度、平均速度、最大速度的大小关系无效"})
	}

	return issues
}

// AssertRecordConsistent fails with a RecordConsistencyError listing every
// issue.
func AssertRecordConsistent(record SportRecord) error {
	if issues := ValidateRecordConsistency(record); len(issues) > 0 {
		return &RecordConsistencyError{Issues: issues}
	}
	return nil
}

// ValidateRecordServerTime checks that a finished record is not in the
// server's future. A small allowance covers clock skew without rewriting the
// recorded times.
func ValidateRecordServerTime(record SportRecord, serverTimeMS int64, allowedFutureMS int64) []ConsistencyIssue {
	if serverTimeMS <= 0 {
		return []ConsistencyIssue{{"time.server", "服务器没有返回有效的毫秒时间戳"}}
	}
	if record.EndDateLong <= 0 {
		return []ConsistencyIssue{{"time.end", "记录缺少有效的结束时间戳"}}
	}
	futureMS := record.EndDateLong - serverTimeMS
	if futureMS > allowedFutureMS {
		return []ConsistencyIssue{{"time.future", fmt.Sprintf(
			"运动结束时间位于服务器未来 %.1f 分钟", float64(futureMS)/60000.0)}}
	}
	return nil
}

// AssertRecordNotInServerFuture fails when the record ends measurably after the
// server clock.
func AssertRecordNotInServerFuture(record SportRecord, serverTimeMS int64, allowedFutureMS int64) error {
	if issues := ValidateRecordServerTime(record, serverTimeMS, allowedFutureMS); len(issues) > 0 {
		return &RecordConsistencyError{Issues: issues}
	}
	return nil
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
