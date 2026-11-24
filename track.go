// Package sports provides APIs for importing, measuring, and displaying GPS
// tracks and comparing them with a surveyed reference route.
package sports

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// CRS identifies the coordinate reference system of a Track.
type CRS string

const (
	// WGS84 is used by GPS, GPX, and RFC 7946 GeoJSON.
	WGS84 CRS = "WGS84"
	// GCJ02 is used by AMap basemaps.
	GCJ02 CRS = "GCJ-02"
)

var (
	// ErrInvalidTrack indicates missing or malformed track geometry or coordinates.
	ErrInvalidTrack = errors.New("invalid track")
	// ErrCRS indicates an unsupported or mismatched coordinate reference system.
	ErrCRS = errors.New("incompatible coordinate reference system")
)

// Point is a longitude and latitude in decimal degrees with an optional time.
// A zero Time indicates that the source did not supply a timestamp.
type Point struct {
	Lon  float64
	Lat  float64
	Time time.Time
}

// Track holds ordered segments in one explicitly declared CRS. A gap between
// segments is preserved by import, export, and measurement.
type Track struct {
	CRS      CRS
	Segments [][]Point
}

// Validate checks the CRS, segment structure, and coordinate ranges. Single
// points are valid track segments but do not contribute to distance.
func (t Track) Validate() error {
	if err := validateCRS(t.CRS); err != nil {
		return err
	}
	if len(t.Segments) == 0 {
		return fmt.Errorf("%w: no segments", ErrInvalidTrack)
	}
	for i, segment := range t.Segments {
		if len(segment) == 0 {
			return fmt.Errorf("%w: segment %d is empty", ErrInvalidTrack, i)
		}
		for j, point := range segment {
			if !point.valid() {
				return fmt.Errorf("%w: segment %d point %d has invalid coordinates", ErrInvalidTrack, i, j)
			}
		}
	}
	return nil
}

func validateCRS(crs CRS) error {
	if crs != WGS84 && crs != GCJ02 {
		return fmt.Errorf("%w: unsupported CRS %q", ErrCRS, crs)
	}
	return nil
}

func (p Point) valid() bool {
	return p.Lat >= -90 && p.Lat <= 90 && p.Lon >= -180 && p.Lon <= 180
}

// LengthMeters sums the spherical distances between successive points within
// each segment. Use WGS84 data for physical distance measurements.
func (t Track) LengthMeters() (float64, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	var meters float64
	for _, segment := range t.Segments {
		previous := unitPosition(segment[0])
		for _, point := range segment[1:] {
			current := unitPosition(point)
			meters += distanceMeters(previous, current)
			previous = current
		}
	}
	return meters, nil
}

const earthRadiusMeters = 6371008.8

type vector struct{ x, y, z float64 }

func unitPosition(p Point) vector {
	lat := p.Lat * math.Pi / 180
	lon := p.Lon * math.Pi / 180
	return vector{math.Cos(lat) * math.Cos(lon), math.Cos(lat) * math.Sin(lon), math.Sin(lat)}
}

func (a vector) dot(b vector) float64 {
	return a.x*b.x + a.y*b.y + a.z*b.z
}

func (a vector) cross(b vector) vector {
	return vector{a.y*b.z - a.z*b.y, a.z*b.x - a.x*b.z, a.x*b.y - a.y*b.x}
}

func (a vector) norm() float64 {
	return math.Sqrt(a.dot(a))
}

func (a vector) scale(k float64) vector {
	return vector{a.x * k, a.y * k, a.z * k}
}

func distanceMeters(a, b vector) float64 {
	return earthRadiusMeters * math.Atan2(a.cross(b).norm(), a.dot(b))
}
