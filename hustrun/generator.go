package hustrun

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"
)

// ServerLocation is the time zone the client formats beginTime / endTime in.
// The service runs in mainland China, so a fixed +08:00 keeps the strings and
// the epoch fields consistent no matter what the host zone is.
var ServerLocation = time.FixedZone("UTC+8", 8*3600)

// Default geometry and record shaping constants.
const (
	DefaultSmoothIterations = 3
	DefaultSportType        = "3"
	DefaultRouteID          = "7"
	DefaultRouteName        = "跑步-男生"
	metresPerDegreeLat      = 111320.0
)

// RouteGeometry is a closed or open polyline with its cumulative length.
type RouteGeometry struct {
	Points     [][2]float64
	Cumulative []float64
	LengthM    float64
}

// PointAt returns the coordinate and bearing at distanceM along the polyline.
// Distances wrap around, so a closed loop can be repeated.
func (g RouteGeometry) PointAt(distanceM float64) (lat, lon, bearing float64) {
	if g.LengthM <= 0 || len(g.Points) < 2 {
		if len(g.Points) > 0 {
			return g.Points[0][0], g.Points[0][1], 0
		}
		return 0, 0, 0
	}
	distance := math.Mod(distanceM, g.LengthM)
	if distance < 0 {
		distance += g.LengthM
	}
	low, high := 0, len(g.Cumulative)-1
	for low+1 < high {
		middle := (low + high) / 2
		if g.Cumulative[middle] <= distance {
			low = middle
		} else {
			high = middle
		}
	}
	start := g.Points[low]
	end := g.Points[high]
	segment := g.Cumulative[high] - g.Cumulative[low]
	fraction := 0.0
	if segment > 0 {
		fraction = (distance - g.Cumulative[low]) / segment
	}
	lat = start[0] + (end[0]-start[0])*fraction
	lon = start[1] + (end[1]-start[1])*fraction
	return lat, lon, bearingDegrees(start[0], start[1], end[0], end[1])
}

// LoadRoute reads a [[lat, lon], ...] route file.
func LoadRoute(path string) (RouteGeometry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RouteGeometry{}, fmt.Errorf("%w: route: %v", ErrConfig, err)
	}
	var coordinates [][]float64
	if err := json.Unmarshal(raw, &coordinates); err != nil {
		return RouteGeometry{}, fmt.Errorf("%w: route %s: %v", ErrConfig, path, err)
	}
	if len(coordinates) < 2 {
		return RouteGeometry{}, fmt.Errorf("%w: route %s has fewer than two points", ErrConfig, path)
	}
	points := make([][2]float64, 0, len(coordinates))
	for index, item := range coordinates {
		if len(item) < 2 {
			return RouteGeometry{}, fmt.Errorf("%w: route %s point %d is incomplete", ErrConfig, path, index)
		}
		points = append(points, [2]float64{item[0], item[1]})
	}
	return BuildGeometry(points, DefaultSmoothIterations)
}

// BuildGeometry smooths the polyline and records its cumulative length. When the
// first and last point are less than 20 m apart the route is treated as closed.
func BuildGeometry(points [][2]float64, smoothIterations int) (RouteGeometry, error) {
	if len(points) < 2 {
		return RouteGeometry{}, fmt.Errorf("%w: route has fewer than two points", ErrConfig)
	}
	working := append([][2]float64{}, points...)
	if HaversineDistanceM(working[0][0], working[0][1], working[len(working)-1][0], working[len(working)-1][1]) < 20.0 {
		if working[0] != working[len(working)-1] {
			working = append(working, working[0])
		}
	}
	if smoothIterations > 0 {
		working = chaikinSmooth(working, smoothIterations)
	}
	cumulative := make([]float64, 1, len(working))
	total := 0.0
	for i := 1; i < len(working); i++ {
		total += HaversineDistanceM(working[i-1][0], working[i-1][1], working[i][0], working[i][1])
		cumulative = append(cumulative, total)
	}
	if total <= 0 {
		return RouteGeometry{}, fmt.Errorf("%w: route total length is zero", ErrConfig)
	}
	return RouteGeometry{Points: working, Cumulative: cumulative, LengthM: total}, nil
}

// chaikinSmooth rounds the corners left by hand-drawn vertices so that a 1 Hz
// sample does not cut corners.
func chaikinSmooth(points [][2]float64, iterations int) [][2]float64 {
	current := append([][2]float64{}, points...)
	closed := current[0] == current[len(current)-1]
	for iteration := 0; iteration < iterations; iteration++ {
		smoothed := make([][2]float64, 0, len(current)*2)
		if !closed {
			smoothed = append(smoothed, current[0])
		}
		for i := 0; i+1 < len(current); i++ {
			start, end := current[i], current[i+1]
			smoothed = append(smoothed,
				[2]float64{0.75*start[0] + 0.25*end[0], 0.75*start[1] + 0.25*end[1]},
				[2]float64{0.25*start[0] + 0.75*end[0], 0.25*start[1] + 0.75*end[1]},
			)
		}
		if closed {
			smoothed = append(smoothed, smoothed[0])
		} else {
			smoothed = append(smoothed, current[len(current)-1])
		}
		current = smoothed
	}
	return current
}

func bearingDegrees(lat1, lon1, lat2, lon2 float64) float64 {
	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	deltaLambda := (lon2 - lon1) * math.Pi / 180
	y := math.Sin(deltaLambda) * math.Cos(phi2)
	x := math.Cos(phi1)*math.Sin(phi2) - math.Sin(phi1)*math.Cos(phi2)*math.Cos(deltaLambda)
	return math.Mod(math.Atan2(y, x)*180/math.Pi+360, 360)
}

func offsetMetres(lat, lon, northM, eastM float64) (float64, float64) {
	lat2 := lat + northM/metresPerDegreeLat
	lon2 := lon + eastM/(metresPerDegreeLat*math.Cos(lat*math.Pi/180))
	return lat2, lon2
}

func roundCoord(value float64) float64 {
	return math.Round(value*1e6) / 1e6
}

// FormatServerTime renders epoch milliseconds in the server's time zone.
func FormatServerTime(ms int64) string {
	return time.UnixMilli(ms).In(ServerLocation).Format("2006-01-02 15:04:05")
}

// FormatDuration renders seconds as HH:MM:SS.
func FormatDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, (seconds%3600)/60, seconds%60)
}

// GenerateOptions configures GenerateRouteRecord. Zero values select the
// reference defaults.
type GenerateOptions struct {
	StartMS         int64
	SportID         string
	XH              string
	Name            string
	TargetDistanceM float64
	Seed            *int64
	SportType       string
	RouteID         string
	RouteName       string
	MinSpeedMPS     float64
	MaxSpeedMPS     float64
	MinAccuracyM    float64
	MaxAccuracyM    float64
	FirstFixDelayS  float64
}

func (o *GenerateOptions) applyDefaults() error {
	if o.TargetDistanceM == 0 {
		o.TargetDistanceM = 2100
	}
	if o.SportType == "" {
		o.SportType = DefaultSportType
	}
	if o.RouteID == "" {
		o.RouteID = DefaultRouteID
	}
	if o.RouteName == "" {
		o.RouteName = DefaultRouteName
	}
	if o.MinSpeedMPS == 0 {
		o.MinSpeedMPS = 4.0
	}
	if o.MaxSpeedMPS == 0 {
		o.MaxSpeedMPS = 5.0
	}
	if o.MinAccuracyM == 0 {
		o.MinAccuracyM = 4.0
	}
	if o.MaxAccuracyM == 0 {
		o.MaxAccuracyM = 12.0
	}
	if o.FirstFixDelayS == 0 {
		o.FirstFixDelayS = 2.0
	}
	if o.TargetDistanceM < 100 {
		return fmt.Errorf("%w: target distance is too short", ErrInvalidRecord)
	}
	if !(1.0 <= o.MinSpeedMPS && o.MinSpeedMPS < o.MaxSpeedMPS && o.MaxSpeedMPS <= 12.0) {
		return fmt.Errorf("%w: speed range is invalid", ErrInvalidRecord)
	}
	if !(0.0 <= o.MinAccuracyM && o.MinAccuracyM < o.MaxAccuracyM && o.MaxAccuracyM <= 50.0) {
		return fmt.Errorf("%w: accuracy range is invalid", ErrInvalidRecord)
	}
	return nil
}

// SensorTraceOptions configures MakeSensorTrace.
type SensorTraceOptions struct {
	DurationSeconds int
	Seed            *int64
}

// GenerateRouteRecord walks the route at a plausible running pace and returns an
// internally consistent record.
//
// The server judges whether a track looks human: speed must vary slowly inside
// 4.0–5.0 m/s, accuracy must vary, and pace / minute speed must be derivable
// from the track and the duration. This produces a 1 Hz track with exactly
// those statistics, then derives every aggregate from it.
func GenerateRouteRecord(route RouteGeometry, options GenerateOptions) (SportRecord, error) {
	if err := options.applyDefaults(); err != nil {
		return SportRecord{}, err
	}
	rng := newDeterministicRNG(options.Seed)

	baseSpeed := rng.Uniform(options.MinSpeedMPS+0.15, options.MaxSpeedMPS-0.15)
	speedAmplitude := rng.Uniform(0.18, 0.35)
	speedPeriod := rng.Uniform(55.0, 95.0)
	speedPhase := rng.Uniform(0, 2*math.Pi)
	driftPeriod := rng.Uniform(30.0, 70.0)
	driftPhase := rng.Uniform(0, 2*math.Pi)

	startLat, startLon, _ := route.PointAt(0)
	// The official sample starts with a placeholder point: t=0 and every
	// measured value at zero.
	points := []TrackPoint{{
		A: roundCoord(startLat), O: roundCoord(startLon), V: 1,
	}}

	elapsed := options.FirstFixDelayS
	cumulative := 0.0
	travelled := 0.0
	var previousLat, previousLon float64
	havePrevious := false
	previousMS := options.StartMS

	for cumulative < options.TargetDistanceM {
		speed := baseSpeed +
			speedAmplitude*math.Sin(2*math.Pi*elapsed/speedPeriod+speedPhase) +
			rng.Gauss(0, 0.05)
		speed = math.Min(math.Max(speed, options.MinSpeedMPS), options.MaxSpeedMPS)
		interval := 1.0 + rng.Uniform(-0.05, 0.05)
		travelled += speed * interval

		lat, lon, bearing := route.PointAt(travelled)
		// A real runner does not hug a mathematical polyline.
		lateral := 0.7 * math.Sin(2*math.Pi*elapsed/driftPeriod+driftPhase)
		radial := bearing * math.Pi / 180
		north := -math.Sin(radial) * lateral
		east := math.Cos(radial) * lateral
		lat, lon = offsetMetres(lat, lon, north, east)
		lat, lon = roundCoord(lat), roundCoord(lon)

		pointMS := options.StartMS + int64(math.Round(elapsed*1000))
		var segment, reported float64
		if !havePrevious {
			segment = 0
			reported = speed
		} else {
			segment = HaversineDistanceM(previousLat, previousLon, lat, lon)
			// The reported speed follows the measured displacement with 1–2 %
			// location noise, which keeps avgSpeed == Σd / activeTime.
			measured := segment / math.Max(0.2, float64(pointMS-previousMS)/1000.0)
			reported = measured * rng.Uniform(0.985, 1.015)
		}
		cumulative += segment
		accuracy := math.Round(rng.Uniform(options.MinAccuracyM, options.MaxAccuracyM)*10) / 10
		points = append(points, TrackPoint{
			A: lat, O: lon, V: 1,
			S:  math.Round(reported*3.6*100) / 100,
			T:  pointMS,
			AC: accuracy,
			DA: math.Round(cumulative*1e6) / 1e6,
			D:  math.Round(segment*1e6) / 1e6,
		})
		previousLat, previousLon = lat, lon
		havePrevious = true
		previousMS = pointMS
		elapsed += interval
	}

	endMS := int64(math.Ceil(float64(previousMS)/1000.0) * 1000)
	activeSeconds := int(math.Round(float64(endMS-options.StartMS) / 1000.0))
	if activeSeconds < 1 {
		activeSeconds = 1
	}
	distanceKm := cumulative / 1000.0
	averageSpeed := distanceKm / (float64(activeSeconds) / 3600.0)
	averagePaceSeconds := float64(activeSeconds) / distanceKm
	cadence := rng.Uniform(155.0, 172.0)
	totalStep := int(math.Round(cadence * float64(activeSeconds) / 60.0))
	if totalStep < 1 {
		totalStep = 1
	}
	stepMinute := float64(totalStep) / (float64(activeSeconds) / 60.0)

	// Matches the patched client's UploadJsonSports.build(): maxSpeedPerHour is
	// overwritten with avgSpeed and minSpeedPerHour is clamped to "6.0".
	minimumPointSpeed := math.Inf(1)
	for _, point := range points {
		if point.S > 0 {
			minimumPointSpeed = math.Min(minimumPointSpeed, point.S)
		}
	}
	if math.IsInf(minimumPointSpeed, 1) {
		minimumPointSpeed = averageSpeed
	}
	minimumSpeedText := "6.0"
	if minimumPointSpeed < 6.0 {
		minimumSpeedText = FormatDecimal(minimumPointSpeed, 2)
	}

	firstMovement := activeSeconds
	for _, point := range points {
		if point.T > 0 && point.D > 0 {
			firstMovement = int((point.T - options.StartMS) / 1000)
			break
		}
	}

	record := SportRecord{
		SportID:   options.SportID,
		XH:        options.XH,
		Name:      options.Name,
		RouteID:   options.RouteID,
		RouteName: options.RouteName,
		SportType: options.SportType,

		StartDate:        FormatServerTime(options.StartMS),
		EndDate:          FormatServerTime(endMS),
		StartDateLong:    options.StartMS,
		EndDateLong:      endMS,
		DLTime:           FormatDuration(activeSeconds),
		LastOdometerTime: FormatDuration(firstMovement),

		DLLC:            FormatDecimal(distanceKm, 2),
		TotalStep:       totalStep,
		AvgSpeed:        FormatDecimal(averageSpeed, 2),
		AvgPace:         FormatPace(averagePaceSeconds),
		Cal:             math.Round(distanceKm*rng.Uniform(53.0, 60.0)*10) / 10,
		StepMinute:      FormatDecimal(stepMinute, 1),
		MaxSpeedPerHour: FormatDecimal(averageSpeed, 2),
		MinSpeedPerHour: minimumSpeedText,

		Indoor:           0,
		ActivityStatus:   1,
		Status:           0,
		Result:           1,
		GyroUploadStatus: 0,
		HasPhoto:         0,

		Points:      points,
		Pace:        buildPace(points, options.StartMS),
		MinuteSpeed: buildMinuteSpeed(points, options.StartMS),
	}
	if err := AssertRecordConsistent(record); err != nil {
		return SportRecord{}, err
	}
	return record, nil
}

func buildPace(points []TrackPoint, startMS int64) []PaceEntry {
	var entries []PaceEntry
	if len(points) == 0 {
		return entries
	}
	previousSeconds := 0
	nextKilometre := 1
	for _, point := range points {
		if point.T <= 0 || point.DA < float64(nextKilometre)*1000.0 {
			continue
		}
		elapsed := int(math.Round(float64(point.T-startMS) / 1000.0))
		segment := elapsed - previousSeconds
		if segment < 1 {
			segment = 1
		}
		entries = append(entries, PaceEntry{
			Km:            fmt.Sprint(nextKilometre),
			T:             FormatPace(float64(segment)),
			KmTotalLength: float64(nextKilometre),
			KmDLSJ:        elapsed,
		})
		previousSeconds = elapsed
		nextKilometre++
	}
	return entries
}

func buildMinuteSpeed(points []TrackPoint, startMS int64) []MinuteSpeedEntry {
	buckets := map[int][]float64{}
	for _, point := range points {
		if point.T <= 0 || point.S <= 0 {
			continue
		}
		minute := int((point.T-startMS)/60000) + 1
		buckets[minute] = append(buckets[minute], point.S)
	}
	minutes := make([]int, 0, len(buckets))
	for minute := range buckets {
		minutes = append(minutes, minute)
	}
	sortInts(minutes)
	entries := make([]MinuteSpeedEntry, 0, len(minutes))
	for _, minute := range minutes {
		values := buckets[minute]
		var sum float64
		for _, value := range values {
			sum += value
		}
		entries = append(entries, MinuteSpeedEntry{
			Min: fmt.Sprint(minute),
			V:   FormatDecimal(sum/float64(len(values)), 2),
		})
	}
	return entries
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// MakeSensorTrace generates the ~10 Hz acceleration magnitude series that
// accompanies a record of the given duration.
func MakeSensorTrace(options SensorTraceOptions) string {
	duration := options.DurationSeconds
	if duration < 1 {
		duration = 1
	}
	rng := newDeterministicRNG(options.Seed)
	total := duration * 10
	phase := rng.Uniform(0, 2*math.Pi)
	var out []byte
	out = append(out, '[')
	for index := 0; index < total; index++ {
		t := float64(index) / 10.0
		cadenceHz := rng.Uniform(2.6, 2.9)
		stride := 5.2 * math.Sin(2*math.Pi*cadenceHz*t+phase)
		sway := 0.8 * math.Sin(2*math.Pi*0.35*t)
		noise := rng.Gauss(0, 0.45)
		value := math.Round((9.8+stride+sway+noise)*1e6) / 1e6
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, FormatFloat(value)...)
	}
	out = append(out, ']')
	return string(out)
}

// RouteSummary renders a one-line description of a generated record.
func RouteSummary(record SportRecord) string {
	return fmt.Sprintf("%s → %s | %s km | %s | %s km/h | %d 点",
		record.StartDate, record.EndDate, record.DLLC, record.DLTime,
		record.AvgSpeed, len(record.Points))
}
