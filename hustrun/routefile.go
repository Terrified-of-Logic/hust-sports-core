package hustrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"

	sports "hust-sports-core"
)

// RoutePoint is one route vertex in [latitude, longitude] order, which is the
// order the route files and the generator use.
type RoutePoint = [2]float64

// RouteFromGeoJSON converts a GeoJSON LineString into a route: coordinates are
// swapped to [lat, lon] and resampled every spacingMeters so the 1 Hz generator
// can interpolate smoothly. The first segment of a MultiLineString is used.
func RouteFromGeoJSON(reader io.Reader, spacingMeters float64) ([]RoutePoint, error) {
	if spacingMeters <= 0 {
		spacingMeters = 1.0
	}
	track, err := sports.ReadGeoJSON(reader, sports.WGS84)
	if err != nil {
		return nil, fmt.Errorf("%w: GeoJSON: %v", ErrConfig, err)
	}
	if len(track.Segments) == 0 || len(track.Segments[0]) < 2 {
		return nil, fmt.Errorf("%w: GeoJSON has no usable line", ErrConfig)
	}
	source := track.Segments[0]
	points := make([]RoutePoint, 0, len(source)*4)
	for index := 0; index+1 < len(source); index++ {
		start := RoutePoint{source[index].Lat, source[index].Lon}
		end := RoutePoint{source[index+1].Lat, source[index+1].Lon}
		distance := HaversineDistanceM(start[0], start[1], end[0], end[1])
		steps := int(distance / spacingMeters)
		if steps < 1 {
			steps = 1
		}
		for step := 0; step < steps; step++ {
			fraction := float64(step) / float64(steps)
			points = append(points, RoutePoint{
				start[0] + (end[0]-start[0])*fraction,
				start[1] + (end[1]-start[1])*fraction,
			})
		}
	}
	last := source[len(source)-1]
	points = append(points, RoutePoint{last.Lat, last.Lon})
	return points, nil
}

// MultiLapOptions configures BuildMultiLapRoute.
type MultiLapOptions struct {
	// DistanceM stops the route once this many metres have been laid down.
	DistanceM float64
	// NoiseM is the per-point GPS noise on the lateral offset.
	NoiseM float64
	// DriftM is the per-lap lateral drift amplitude.
	DriftM float64
	Seed   *int64
}

// BuildMultiLapRoute repeats one lap, drifting and jittering each round so the
// loops do not overlap exactly, until the target distance is reached.
func BuildMultiLapRoute(lap []RoutePoint, options MultiLapOptions) ([]RoutePoint, error) {
	if len(lap) < 2 {
		return nil, fmt.Errorf("%w: lap needs at least two points", ErrConfig)
	}
	if options.DistanceM <= 0 {
		options.DistanceM = 2000
	}
	if options.NoiseM <= 0 {
		options.NoiseM = 0.45
	}
	if options.DriftM <= 0 {
		options.DriftM = 1.6
	}
	loop := append([]RoutePoint{}, lap...)
	if HaversineDistanceM(loop[0][0], loop[0][1], loop[len(loop)-1][0], loop[len(loop)-1][1]) > 5 {
		loop = append(loop, loop[0])
	}

	// Local left normal at every vertex, in (east, north) metres.
	normals := make([]RoutePoint, len(loop))
	for index := range loop {
		start := loop[max(0, index-1)]
		end := loop[min(len(loop)-1, index+1)]
		east := (end[1] - start[1]) * metresPerDegreeLat * math.Cos(start[0]*math.Pi/180)
		north := (end[0] - start[0]) * metresPerDegreeLat
		length := math.Hypot(east, north)
		if length == 0 {
			length = 1
		}
		normals[index] = RoutePoint{-north / length, east / length}
	}

	rng := newDeterministicRNG(options.Seed)
	out := make([]RoutePoint, 0, len(loop)*3)
	total := 0.0
	lapNumber := 0
	for total < options.DistanceM {
		drift := options.DriftM * math.Sin(float64(lapNumber)*1.7+0.4)
		for index, point := range loop {
			normal := normals[index]
			offset := drift + rng.Gauss(0, options.NoiseM)
			latitude := point[0] + normal[1]*offset/metresPerDegreeLat
			longitude := point[1] + normal[0]*offset/(metresPerDegreeLat*math.Cos(point[0]*math.Pi/180))
			if len(out) > 0 {
				previous := out[len(out)-1]
				total += HaversineDistanceM(previous[0], previous[1], latitude, longitude)
			}
			out = append(out, RoutePoint{roundTo(latitude, 7), roundTo(longitude, 7)})
			if total >= options.DistanceM {
				break
			}
		}
		lapNumber++
	}
	return out, nil
}

// WriteRouteFile writes a route as compact [[lat, lon], ...] JSON.
func WriteRouteFile(path string, points []RoutePoint) error {
	if len(points) < 2 {
		return fmt.Errorf("%w: route needs at least two points", ErrConfig)
	}
	var buffer bytes.Buffer
	buffer.WriteByte('[')
	for index, point := range points {
		if index > 0 {
			buffer.WriteByte(',')
		}
		buffer.WriteByte('[')
		buffer.WriteString(FormatFloat(roundTo(point[0], 7)))
		buffer.WriteByte(',')
		buffer.WriteString(FormatFloat(roundTo(point[1], 7)))
		buffer.WriteByte(']')
	}
	buffer.WriteString("]\n")
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return nil
}

// RouteLengthM reports the polyline length of a route file in metres.
func RouteLengthM(points []RoutePoint) float64 {
	var total float64
	for index := 1; index < len(points); index++ {
		total += HaversineDistanceM(points[index-1][0], points[index-1][1],
			points[index][0], points[index][1])
	}
	return total
}

// LoadRoutePoints reads a route file as raw vertices, without smoothing.
func LoadRoutePoints(path string) ([]RoutePoint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: route: %v", ErrConfig, err)
	}
	var coordinates [][]float64
	if err := json.Unmarshal(raw, &coordinates); err != nil {
		return nil, fmt.Errorf("%w: route %s: %v", ErrConfig, path, err)
	}
	points := make([]RoutePoint, 0, len(coordinates))
	for index, item := range coordinates {
		if len(item) < 2 {
			return nil, fmt.Errorf("%w: route %s point %d is incomplete", ErrConfig, path, index)
		}
		points = append(points, RoutePoint{item[0], item[1]})
	}
	if len(points) < 2 {
		return nil, fmt.Errorf("%w: route %s has fewer than two points", ErrConfig, path)
	}
	return points, nil
}
