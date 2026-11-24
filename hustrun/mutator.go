package hustrun

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// MutateOptions configures MutateRecord.
type MutateOptions struct {
	// TargetDate is "original", "today" or "YYYY-MM-DD".
	TargetDate string
	// GPSJitterScale shifts every coordinate by up to this many degrees.
	GPSJitterScale float64
	// SpeedJitterPct / StepJitterPct scale the carried statistics.
	SpeedJitterPct float64
	StepJitterPct  float64
	Seed           *int64
	// ReferenceTimeMS is the server clock; nil falls back to the local clock and
	// is only appropriate for offline dry runs.
	ReferenceTimeMS   *int64
	DeformTrack       bool
	TrackDeformationM float64
	// TimeJitterPct reshuffles the sampling intervals by up to this fraction.
	TimeJitterPct float64
}

// DefaultMutateOptions returns the reference defaults.
func DefaultMutateOptions() MutateOptions {
	return MutateOptions{
		TargetDate:        "today",
		DeformTrack:       true,
		TrackDeformationM: 12.0,
		TimeJitterPct:     0.12,
	}
}

func (o *MutateOptions) applyDefaults() {
	if o.TargetDate == "" {
		o.TargetDate = "today"
	}
	if o.TrackDeformationM == 0 {
		o.TrackDeformationM = 12.0
	}
	if o.TimeJitterPct == 0 {
		o.TimeJitterPct = 0.12
	}
}

func (o MutateOptions) validate() error {
	if o.GPSJitterScale < 0 {
		return fmt.Errorf("%w: gps_jitter_scale 不能为负数", ErrInvalidRecord)
	}
	if o.SpeedJitterPct < 0 || o.StepJitterPct < 0 {
		return fmt.Errorf("%w: 扰动比例不能为负数", ErrInvalidRecord)
	}
	if o.TrackDeformationM < 0 || o.TimeJitterPct < 0 || o.TimeJitterPct >= 1 {
		return fmt.Errorf("%w: 轨迹形变幅度或时间扰动比例无效", ErrInvalidRecord)
	}
	if o.TargetDate != "today" && o.TargetDate != "original" {
		if _, err := time.ParseInLocation("2006-01-02", o.TargetDate, ServerLocation); err != nil {
			return fmt.Errorf("%w: 无效的日期格式: %q", ErrInvalidRecord, o.TargetDate)
		}
	}
	return nil
}

// MutateRecord derives a new internally consistent record from an imported one:
// the geometry is deformed, sampling intervals are reshuffled, per-point
// distance and speed series are reordered, and every aggregate is rebuilt from
// the new track.
func MutateRecord(record SportRecord, options MutateOptions) (SportRecord, error) {
	options.applyDefaults()
	if err := options.validate(); err != nil {
		return SportRecord{}, err
	}
	if err := validateSourceGeometry(record); err != nil {
		return SportRecord{}, err
	}
	rng := newDeterministicRNG(options.Seed)

	sourceWallDurationMS := record.EndDateLong - record.StartDateLong
	var sourceTimed []TrackPoint
	for _, point := range record.Points {
		if point.T > 0 {
			sourceTimed = append(sourceTimed, point)
		}
	}
	var sourceIntervals []int64
	for index := 1; index < len(sourceTimed); index++ {
		sourceIntervals = append(sourceIntervals, sourceTimed[index].T-sourceTimed[index-1].T)
	}
	var firstTimedOffsetMS, lastTimedGapMS int64
	if len(sourceTimed) > 0 {
		firstTimedOffsetMS = sourceTimed[0].T - record.StartDateLong
		lastTimedGapMS = record.EndDateLong - sourceTimed[len(sourceTimed)-1].T
	}
	if firstTimedOffsetMS < 0 || firstTimedOffsetMS >= sourceWallDurationMS {
		firstTimedOffsetMS = 0
	}
	if lastTimedGapMS < 0 || lastTimedGapMS >= sourceWallDurationMS {
		lastTimedGapMS = 0
	}

	reference := time.Now()
	if options.ReferenceTimeMS != nil {
		if *options.ReferenceTimeMS <= 0 {
			return SportRecord{}, fmt.Errorf("%w: reference_time_ms 必须是正的 epoch 毫秒时间戳", ErrInvalidRecord)
		}
		reference = time.UnixMilli(*options.ReferenceTimeMS)
	}
	resolvedTargetDate := options.TargetDate
	if strings.EqualFold(resolvedTargetDate, "today") {
		resolvedTargetDate = reference.In(ServerLocation).Format("2006-01-02")
	}
	shifted, err := ShiftToDate(record, resolvedTargetDate, reference)
	if err != nil {
		return SportRecord{}, err
	}
	shifted, err = adjustTimeToSafeWindow(shifted, options.TargetDate, rng, options.ReferenceTimeMS)
	if err != nil {
		return SportRecord{}, err
	}

	odometerKm := shifted.DistanceKm()
	targetTrackDistanceM := odometerKm * 1000
	if len(shifted.Points) > 0 && shifted.Points[len(shifted.Points)-1].DA > 0 {
		targetTrackDistanceM = shifted.Points[len(shifted.Points)-1].DA
	}
	targetGeometryM := CoordinateDistanceM(shifted)

	var coordinates [][2]float64
	var timestamps []int64
	if options.DeformTrack && options.TrackDeformationM > 0 {
		coordinates = deformCoordinates(shifted.Points, targetGeometryM, rng,
			options.TrackDeformationM, options.GPSJitterScale)
		firstTimedIndex := 0
		for index, point := range shifted.Points {
			if point.T > 0 {
				firstTimedIndex = index
				break
			}
		}
		timedStartMS := shifted.StartDateLong + firstTimedOffsetMS
		timedEndMS := shifted.EndDateLong - lastTimedGapMS
		if timedStartMS <= 0 || timedEndMS <= timedStartMS {
			firstTimedIndex = 0
			timedStartMS = shifted.StartDateLong
			timedEndMS = shifted.EndDateLong
		}
		rebuilt, rebuildErr := rebuildTimestamps(coordinates[firstTimedIndex:], timedStartMS, timedEndMS,
			rng, options.TimeJitterPct, sourceIntervals)
		if rebuildErr != nil {
			return SportRecord{}, rebuildErr
		}
		timestamps = make([]int64, firstTimedIndex)
		timestamps = append(timestamps, rebuilt...)
	} else {
		coordinates = rigidCoordinates(shifted.Points, rng, options.GPSJitterScale)
		timestamps = make([]int64, len(shifted.Points))
		for index, point := range shifted.Points {
			timestamps[index] = point.T
		}
	}

	points, err := rebuildPoints(shifted.Points, coordinates, timestamps, targetTrackDistanceM, rng)
	if err != nil {
		return SportRecord{}, err
	}
	activeSeconds, ok := ParseDurationSeconds(shifted.DLTime)
	if !ok || activeSeconds <= 0 {
		activeSeconds = shifted.DurationSeconds()
	}
	if activeSeconds <= 0 || odometerKm <= 0 {
		return SportRecord{}, fmt.Errorf("%w: 无法从非正数里程或时长生成一致记录", ErrInvalidRecord)
	}

	// A "today" retry must not reuse a sportId the server already saw, so the
	// server clock snapshot participates in the UUID seed.
	sportIDRNG := rng
	if options.TargetDate == "today" && options.ReferenceTimeMS != nil {
		base := int64(0)
		if options.Seed != nil {
			base = *options.Seed
		}
		derived := base*1000003 + *options.ReferenceTimeMS
		sportIDRNG = newDeterministicRNG(&derived)
	}
	sportID := generateSportID(sportIDRNG)

	totalStep := shifted.TotalStep
	if options.StepJitterPct > 0 {
		totalStep = int(math.Round(float64(shifted.TotalStep) *
			(1 + rng.Uniform(-options.StepJitterPct, options.StepJitterPct))))
		if totalStep < 0 {
			totalStep = 0
		}
	}

	// The service derives statistics from the internal cumulative distance, so
	// avgSpeed == coordinate[-1].da / activeTime.
	statisticsDistanceKm := targetTrackDistanceM / 1000.0
	if statisticsDistanceKm <= 0 {
		statisticsDistanceKm = odometerKm
	}
	averageSpeed := statisticsDistanceKm / (float64(activeSeconds) / 3600.0)
	averagePace := float64(activeSeconds) / statisticsDistanceKm
	cadence := float64(totalStep) / (float64(activeSeconds) / 60.0)

	oldAverageSpeed, speedErr := parseFloat(shifted.AvgSpeed)
	if speedErr != nil || oldAverageSpeed <= 0 {
		oldAverageSpeed = averageSpeed
	}
	speedRatio := averageSpeed / oldAverageSpeed
	maximumSpeed, maxErr := parseFloat(shifted.MaxSpeedPerHour)
	if maxErr != nil {
		maximumSpeed = averageSpeed
	}
	maximumSpeed *= speedRatio
	minimumSpeed, minErr := parseFloat(shifted.MinSpeedPerHour)
	if minErr != nil {
		minimumSpeed = averageSpeed
	}
	minimumSpeed *= speedRatio

	mutated := SportRecord{
		SportID:   sportID,
		XH:        shifted.XH,
		Name:      shifted.Name,
		RouteID:   shifted.RouteID,
		RouteName: shifted.RouteName,
		SportType: shifted.SportType,

		StartDate:     shifted.StartDate,
		EndDate:       shifted.EndDate,
		StartDateLong: shifted.StartDateLong,
		EndDateLong:   shifted.EndDateLong,
		DLTime:        FormatDuration(activeSeconds),
		// The field is not the total duration; keep the source value.
		LastOdometerTime: shifted.LastOdometerTime,

		DLLC:            firstNonEmpty(shifted.DLLC, FormatDecimal(odometerKm, 3)),
		TotalStep:       totalStep,
		AvgSpeed:        FormatDecimal(averageSpeed, 2),
		AvgPace:         FormatPace(averagePace),
		Cal:             shifted.Cal,
		StepMinute:      FormatDecimal(cadence, 1),
		MaxSpeedPerHour: FormatDecimal(math.Max(maximumSpeed, averageSpeed), 2),
		MinSpeedPerHour: FormatDecimal(math.Min(minimumSpeed, averageSpeed), 2),

		Indoor:           shifted.Indoor,
		RoutePolylineBH:  shifted.RoutePolylineBH,
		AlreadyPassPoint: shifted.AlreadyPassPoint,
		ActivityStatus:   1,
		Status:           0,
		// The App validates locally when the run ends and stores 1 for a legal
		// finished record; 0 is not a valid pending value.
		Result:           1,
		GyroUploadStatus: 0,
		HasPhoto:         0,

		Points:      points,
		Pace:        rebuildPace(points, targetTrackDistanceM, shifted.StartDateLong),
		MinuteSpeed: rebuildMinuteSpeed(points, activeSeconds, shifted.StartDateLong),
	}
	if err := AssertRecordConsistent(mutated); err != nil {
		return SportRecord{}, err
	}
	return mutated, nil
}

func validateSourceGeometry(record SportRecord) error {
	if len(record.Points) < 2 {
		return fmt.Errorf("%w: 轨迹点至少需要 2 个", ErrInvalidRecord)
	}
	expectedM := record.DistanceKm() * 1000
	geometryM := CoordinateDistanceM(record)
	if expectedM <= 0 || geometryM <= 0 {
		return fmt.Errorf("%w: 源记录缺少有效里程或坐标折线", ErrInvalidRecord)
	}
	if math.Abs(geometryM-expectedM) > math.Max(100, expectedM*0.20) {
		return &RecordConsistencyError{Issues: []ConsistencyIssue{{"track.geometry", fmt.Sprintf(
			"源坐标折线 %.3f km 与里程 %.3f km 差异过大", geometryM/1000, expectedM/1000)}}}
	}
	return nil
}

// adjustTimeToSafeWindow places the record inside a window that has already
// finished, so the uploaded times never sit in the server's future.
func adjustTimeToSafeWindow(record SportRecord, targetDate string, rng *deterministicRNG,
	referenceTimeMS *int64) (SportRecord, error) {
	const (
		windowStartHour = 7
		windowEndHour   = 22
		bufferMinutes   = 5
	)
	if strings.EqualFold(targetDate, "original") {
		return record, nil
	}
	durationMS := record.EndDateLong - record.StartDateLong
	if durationMS <= 0 {
		return SportRecord{}, fmt.Errorf("%w: 运动结束时间必须晚于开始时间", ErrInvalidRecord)
	}
	day := time.UnixMilli(record.StartDateLong).In(ServerLocation)
	windowStart := time.Date(day.Year(), day.Month(), day.Day(), windowStartHour, 0, 0, 0, ServerLocation)
	windowEnd := time.Date(day.Year(), day.Month(), day.Day(), windowEndHour, 0, 0, 0, ServerLocation)
	reference := time.Now()
	if referenceTimeMS != nil {
		reference = time.UnixMilli(*referenceTimeMS)
	}
	cutoff := reference.Add(-time.Duration(bufferMinutes) * time.Minute)
	effectiveEnd := min(windowEnd.UnixMilli(), cutoff.UnixMilli())
	latestStartMS := effectiveEnd - durationMS
	if latestStartMS < windowStart.UnixMilli() {
		return SportRecord{}, fmt.Errorf("%w: 截至%s尚无足够的已结束时间窗口；运动时长为 %.0f 分钟，最早只能从 %02d:00 开始",
			ErrInvalidRecord, reference.In(ServerLocation).Format("15:04:05"),
			float64(durationMS)/60000, windowStartHour)
	}

	var candidate SportRecord
	switch {
	case strings.EqualFold(targetDate, "today"):
		candidate = shiftCopy(record, randomStart(rng, windowStart.UnixMilli(), latestStartMS)-record.StartDateLong)
	case record.StartDateLong >= windowStart.UnixMilli() && record.EndDateLong <= windowEnd.UnixMilli():
		candidate = record
	default:
		candidate = shiftCopy(record, randomStart(rng, windowStart.UnixMilli(), latestStartMS)-record.StartDateLong)
	}
	if candidate.EndDateLong > cutoff.UnixMilli() {
		source := "本机"
		if referenceTimeMS != nil {
			source = "服务器"
		}
		return SportRecord{}, fmt.Errorf("%w: 确定性候选结束时间 %s 晚于%s参考时刻的安全截止值 %s；请稍后重试或指定已结束的目标日期",
			ErrInvalidRecord, candidate.EndDate, source, cutoff.In(ServerLocation).Format("2006-01-02 15:04:05"))
	}
	return candidate, nil
}

func randomStart(rng *deterministicRNG, earliest, latest int64) int64 {
	if latest <= earliest {
		return earliest
	}
	return earliest + int64(rng.IntN(int(latest-earliest)+1))
}

func shiftCopy(record SportRecord, deltaMS int64) SportRecord {
	if deltaMS == 0 {
		return record
	}
	shifted := record
	shifted.StartDateLong = record.StartDateLong + deltaMS
	shifted.EndDateLong = record.EndDateLong + deltaMS
	shifted.StartDate = FormatServerTime(shifted.StartDateLong)
	shifted.EndDate = FormatServerTime(shifted.EndDateLong)
	shifted.Points = make([]TrackPoint, len(record.Points))
	copy(shifted.Points, record.Points)
	for index := range shifted.Points {
		if shifted.Points[index].T > 0 {
			shifted.Points[index].T += deltaMS
		}
	}
	shifted.Pace = append([]PaceEntry{}, record.Pace...)
	shifted.MinuteSpeed = append([]MinuteSpeedEntry{}, record.MinuteSpeed...)
	return shifted
}

func toLocalXY(latitude, longitude, originLatitude, originLongitude float64) (float64, float64) {
	lat0 := originLatitude * math.Pi / 180
	x := (longitude - originLongitude) * math.Pi / 180 * earthRadiusM * math.Cos(lat0)
	y := (latitude - originLatitude) * math.Pi / 180 * earthRadiusM
	return x, y
}

func fromLocalXY(x, y, originLatitude, originLongitude float64) (float64, float64) {
	lat0 := originLatitude * math.Pi / 180
	latitude := originLatitude + y/earthRadiusM*180/math.Pi
	longitude := originLongitude + x/(earthRadiusM*math.Cos(lat0))*180/math.Pi
	return latitude, longitude
}

func coordinatesFromXY(xy [][2]float64, originLatitude, originLongitude float64) [][2]float64 {
	out := make([][2]float64, len(xy))
	for index, item := range xy {
		latitude, longitude := fromLocalXY(item[0], item[1], originLatitude, originLongitude)
		out[index] = [2]float64{roundCoord(latitude), roundCoord(longitude)}
	}
	return out
}

func coordinateLength(coordinates [][2]float64) float64 {
	var total float64
	for index := 1; index < len(coordinates); index++ {
		total += HaversineDistanceM(coordinates[index-1][0], coordinates[index-1][1],
			coordinates[index][0], coordinates[index][1])
	}
	return total
}

// deformCoordinates applies a smooth, seeded deformation in a local metric
// plane and rescales the result back to the target length.
func deformCoordinates(points []TrackPoint, targetDistanceM float64, rng *deterministicRNG,
	deformationM, gpsJitterScale float64) [][2]float64 {
	originLatitude := points[0].A
	originLongitude := points[0].O
	source := make([][2]float64, len(points))
	for index, point := range points {
		x, y := toLocalXY(point.A, point.O, originLatitude, originLongitude)
		source[index] = [2]float64{x, y}
	}
	angle := rng.Uniform(-4*math.Pi/180, 4*math.Pi/180)
	cosine, sine := math.Cos(angle), math.Sin(angle)
	phase := rng.Uniform(0, 2*math.Pi)
	amplitude := rng.Uniform(0.55, 1.0) * deformationM
	translationX := rng.Uniform(-deformationM, deformationM)
	translationY := rng.Uniform(-deformationM, deformationM)

	transformed := make([][2]float64, len(source))
	denominator := max(1, len(source)-1)
	for index, item := range source {
		progress := float64(index) / float64(denominator)
		rotatedX := item[0]*cosine - item[1]*sine
		rotatedY := item[0]*sine + item[1]*cosine
		envelope := math.Sin(math.Pi * progress)
		lateral := amplitude * envelope *
			(0.70*math.Sin(2*math.Pi*progress+phase) + 0.30*math.Sin(4*math.Pi*progress+phase/2))
		transformed[index] = [2]float64{
			rotatedX + translationX,
			rotatedY + translationY + lateral,
		}
	}

	anchorX, anchorY := transformed[0][0], transformed[0][1]
	scale := 1.0
	scaled := transformed
	for iteration := 0; iteration < 4; iteration++ {
		scaled = make([][2]float64, len(transformed))
		for index, item := range transformed {
			scaled[index] = [2]float64{
				anchorX + (item[0]-anchorX)*scale,
				anchorY + (item[1]-anchorY)*scale,
			}
		}
		coordinates := coordinatesFromXY(scaled, originLatitude, originLongitude)
		length := coordinateLength(coordinates)
		if length <= 0 {
			return nil
		}
		scale *= targetDistanceM / length
	}
	coordinates := coordinatesFromXY(scaled, originLatitude, originLongitude)
	if gpsJitterScale > 0 {
		latitudeOffset := rng.Uniform(-gpsJitterScale, gpsJitterScale)
		longitudeOffset := rng.Uniform(-gpsJitterScale, gpsJitterScale)
		for index := range coordinates {
			coordinates[index][0] = roundCoord(coordinates[index][0] + latitudeOffset)
			coordinates[index][1] = roundCoord(coordinates[index][1] + longitudeOffset)
		}
	}
	return coordinates
}

func rigidCoordinates(points []TrackPoint, rng *deterministicRNG, gpsJitterScale float64) [][2]float64 {
	var latitudeOffset, longitudeOffset float64
	if gpsJitterScale > 0 {
		latitudeOffset = rng.Uniform(-gpsJitterScale, gpsJitterScale)
		longitudeOffset = rng.Uniform(-gpsJitterScale, gpsJitterScale)
	}
	out := make([][2]float64, len(points))
	for index, point := range points {
		out[index] = [2]float64{roundCoord(point.A + latitudeOffset), roundCoord(point.O + longitudeOffset)}
	}
	return out
}

// rebuildTimestamps reshuffles the sampled intervals and jitters them, so the
// original relative timing is not replayed.
func rebuildTimestamps(coordinates [][2]float64, startMS, endMS int64, rng *deterministicRNG,
	jitterPct float64, sourceIntervals []int64) ([]int64, error) {
	if len(coordinates) < 2 {
		if len(coordinates) == 1 {
			return []int64{startMS}, nil
		}
		return nil, nil
	}
	durationMS := endMS - startMS
	if durationMS < int64(len(coordinates)-1) {
		return nil, fmt.Errorf("%w: 运动总时长不足以容纳严格递增的轨迹点", ErrInvalidRecord)
	}
	weights := make([]float64, len(coordinates)-1)
	useSource := len(sourceIntervals) == len(coordinates)-1
	if useSource {
		for _, interval := range sourceIntervals {
			if interval <= 0 {
				useSource = false
				break
			}
		}
	}
	if useSource {
		for index := range weights {
			weights[index] = float64(sourceIntervals[index])
		}
		rng.Shuffle(len(weights), func(i, j int) { weights[i], weights[j] = weights[j], weights[i] })
	} else {
		for index := range weights {
			weights[index] = 1
		}
	}
	totalWeight := 0.0
	for index := range weights {
		weights[index] *= 1 + rng.Uniform(-jitterPct, jitterPct)
		totalWeight += weights[index]
	}
	timestamps := make([]int64, 1, len(coordinates))
	timestamps[0] = startMS
	cumulativeWeight := 0.0
	for index := 1; index < len(coordinates)-1; index++ {
		cumulativeWeight += weights[index-1]
		candidate := startMS + int64(math.Round(float64(durationMS)*cumulativeWeight/totalWeight))
		minimum := timestamps[len(timestamps)-1] + 1
		maximum := endMS - int64(len(coordinates)-1-index)
		if candidate < minimum {
			candidate = minimum
		}
		if candidate > maximum {
			candidate = maximum
		}
		timestamps = append(timestamps, candidate)
	}
	timestamps = append(timestamps, endMS)
	return timestamps, nil
}

// rebuildPoints rebuilds per-point distance and speed series from the deformed
// coordinates without replaying the source order.
func rebuildPoints(sourcePoints []TrackPoint, coordinates [][2]float64, timestamps []int64,
	targetTrackDistanceM float64, rng *deterministicRNG) ([]TrackPoint, error) {
	positiveDistances := make([]float64, 0, len(sourcePoints))
	for _, point := range sourcePoints[1:] {
		if point.D > 0 {
			positiveDistances = append(positiveDistances, point.D)
		}
	}
	rng.Shuffle(len(positiveDistances), func(i, j int) {
		positiveDistances[i], positiveDistances[j] = positiveDistances[j], positiveDistances[i]
	})
	filtered := make([]float64, 0, len(sourcePoints))
	filtered = append(filtered, 0)
	cursor := 0
	for _, source := range sourcePoints[1:] {
		if source.D > 0 && cursor < len(positiveDistances) {
			filtered = append(filtered, positiveDistances[cursor])
			cursor++
		} else {
			filtered = append(filtered, 0)
		}
	}
	filteredTotal := 0.0
	for _, value := range filtered {
		filteredTotal += value
	}
	if filteredTotal <= 0 || targetTrackDistanceM <= 0 {
		return nil, fmt.Errorf("%w: 无法从轨迹生成有效的过滤里程", ErrInvalidRecord)
	}
	distanceScale := targetTrackDistanceM / filteredTotal
	segmentDistances := make([]float64, len(filtered))
	for index, value := range filtered {
		segmentDistances[index] = value * distanceScale
	}

	timedSpeeds := make([]float64, 0, len(sourcePoints))
	for _, point := range sourcePoints {
		if point.T > 0 {
			timedSpeeds = append(timedSpeeds, math.Max(0, point.S))
		}
	}
	rng.Shuffle(len(timedSpeeds), func(i, j int) { timedSpeeds[i], timedSpeeds[j] = timedSpeeds[j], timedSpeeds[i] })
	speeds := make([]float64, len(sourcePoints))
	cursor = 0
	for index, source := range sourcePoints {
		if timestamps[index] > 0 && cursor < len(timedSpeeds) {
			speeds[index] = timedSpeeds[cursor]
			cursor++
		} else {
			speeds[index] = math.Max(0, source.S)
		}
	}

	rebuilt := make([]TrackPoint, len(sourcePoints))
	cumulative := 0.0
	for index, source := range sourcePoints {
		cumulative += segmentDistances[index]
		rebuilt[index] = TrackPoint{
			A:   roundTo(coordinates[index][0], 7),
			O:   roundTo(coordinates[index][1], 7),
			V:   source.V,
			S:   roundTo(speeds[index], 2),
			T:   timestamps[index],
			AC:  source.AC,
			DA:  roundTo(cumulative, 3),
			D:   roundTo(segmentDistances[index], 3),
			ST:  source.ST,
			STA: source.STA,
		}
	}
	return rebuilt, nil
}

func timedPoints(points []TrackPoint) []TrackPoint {
	timed := make([]TrackPoint, 0, len(points))
	for _, point := range points {
		if point.T > 0 {
			timed = append(timed, point)
		}
	}
	return timed
}

func elapsedAtDistance(points []TrackPoint, distanceM float64, recordStartMS int64) float64 {
	timed := timedPoints(points)
	for index := 1; index < len(timed); index++ {
		previous, current := timed[index-1], timed[index]
		if current.DA+1e-6 < distanceM {
			continue
		}
		segmentM := current.DA - previous.DA
		fraction := 0.0
		if segmentM > 0 {
			fraction = (distanceM - previous.DA) / segmentM
		}
		crossingMS := float64(previous.T) + float64(current.T-previous.T)*fraction
		return (crossingMS - float64(recordStartMS)) / 1000
	}
	if len(timed) == 0 {
		return 0
	}
	return (float64(timed[len(timed)-1].T) - float64(recordStartMS)) / 1000
}

func rebuildPace(points []TrackPoint, totalDistanceM float64, recordStartMS int64) []PaceEntry {
	kilometres := int(totalDistanceM / 1000)
	rebuilt := make([]PaceEntry, 0, kilometres)
	previousCumulative := 0
	for index := 1; index <= kilometres; index++ {
		boundary := float64(index) * 1000
		cumulative := int(math.Round(elapsedAtDistance(points, boundary, recordStartMS)))
		if cumulative < previousCumulative+1 {
			cumulative = previousCumulative + 1
		}
		rebuilt = append(rebuilt, PaceEntry{
			Km:            fmt.Sprint(index),
			T:             FormatPace(float64(cumulative - previousCumulative)),
			KmTotalLength: roundTo(boundary/1000, 3),
			KmDLSJ:        cumulative,
		})
		previousCumulative = cumulative
	}
	return rebuilt
}

func rebuildMinuteSpeed(points []TrackPoint, durationSeconds int, recordStartMS int64) []MinuteSpeedEntry {
	timed := timedPoints(points)
	positive := make([]TrackPoint, 0, len(timed))
	for _, point := range timed {
		if point.S > 0 {
			positive = append(positive, point)
		}
	}
	candidates := positive
	if len(candidates) == 0 {
		candidates = timed
	}
	rebuilt := make([]MinuteSpeedEntry, 0, durationSeconds/60)
	for index := 1; index <= durationSeconds/60; index++ {
		targetMS := recordStartMS + int64(index)*60000
		if len(candidates) == 0 {
			break
		}
		nearest := candidates[0]
		bestDelta := absInt64(nearest.T - targetMS)
		for _, point := range candidates[1:] {
			delta := absInt64(point.T - targetMS)
			if delta < bestDelta {
				nearest = point
				bestDelta = delta
			}
		}
		rebuilt = append(rebuilt, MinuteSpeedEntry{
			Min: fmt.Sprint(index),
			V:   FormatDecimal(math.Max(0, nearest.S), 2),
		})
	}
	return rebuilt
}

// generateSportID builds a UUID v4-shaped identifier from the seeded source.
func generateSportID(rng *deterministicRNG) string {
	var raw [16]byte
	for index := range raw {
		raw[index] = byte(rng.next() >> 56)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func roundTo(value float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(value*scale) / scale
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
