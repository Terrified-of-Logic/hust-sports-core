package sports

import (
	"encoding/json"
	"fmt"
	"io"
)

// ReadGeoJSON reads LineString and MultiLineString geometries, standalone or
// inside a Feature or FeatureCollection. Use WGS84 for RFC 7946 data. Use
// GCJ02 only when the producer explicitly declares that its export uses it.
// Feature properties and altitude are ignored.
func ReadGeoJSON(r io.Reader, crs CRS) (Track, error) {
	if err := validateCRS(crs); err != nil {
		return Track{}, err
	}
	var object geoJSONObject
	if err := decodeJSON(r, &object); err != nil {
		return Track{}, fmt.Errorf("%w: GeoJSON: %w", ErrInvalidTrack, err)
	}

	track := Track{CRS: crs}
	switch object.Type {
	case "FeatureCollection":
		for i, feature := range object.Features {
			if feature.Type != "Feature" {
				return Track{}, fmt.Errorf("%w: GeoJSON element %d must be a Feature", ErrInvalidTrack, i)
			}
			if err := track.addGeometry(feature.Geometry); err != nil {
				return Track{}, fmt.Errorf("GeoJSON feature %d: %w", i, err)
			}
		}
	case "Feature":
		if err := track.addGeometry(object.Geometry); err != nil {
			return Track{}, err
		}
	case "LineString", "MultiLineString":
		if err := track.addGeometry(&object); err != nil {
			return Track{}, err
		}
	default:
		return Track{}, fmt.Errorf("%w: unsupported GeoJSON type %q", ErrInvalidTrack, object.Type)
	}
	if err := track.Validate(); err != nil {
		return Track{}, err
	}
	return track, nil
}

type geoJSONObject struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
	Geometry    *geoJSONObject  `json:"geometry"`
	Features    []geoJSONObject `json:"features"`
}

type jsonPosition []*float64

func (t *Track) addGeometry(object *geoJSONObject) error {
	if object == nil {
		return nil
	}
	var lines [][]jsonPosition
	switch object.Type {
	case "LineString":
		var line []jsonPosition
		if err := json.Unmarshal(object.Coordinates, &line); err != nil {
			return fmt.Errorf("%w: GeoJSON LineString coordinates: %w", ErrInvalidTrack, err)
		}
		lines = [][]jsonPosition{line}
	case "MultiLineString":
		if err := json.Unmarshal(object.Coordinates, &lines); err != nil {
			return fmt.Errorf("%w: GeoJSON MultiLineString coordinates: %w", ErrInvalidTrack, err)
		}
		if len(lines) == 0 {
			return fmt.Errorf("%w: GeoJSON MultiLineString is empty", ErrInvalidTrack)
		}
	case "Point", "MultiPoint", "Polygon", "MultiPolygon", "GeometryCollection":
		return nil
	default:
		return fmt.Errorf("%w: invalid GeoJSON geometry type %q", ErrInvalidTrack, object.Type)
	}
	for _, line := range lines {
		segmentIndex := len(t.Segments)
		if len(line) < 2 {
			return fmt.Errorf("%w: GeoJSON segment %d has fewer than two positions", ErrInvalidTrack, segmentIndex)
		}
		segment := make([]Point, len(line))
		for i, position := range line {
			if len(position) < 2 || len(position) > 3 || position[0] == nil || position[1] == nil || (len(position) == 3 && position[2] == nil) {
				return fmt.Errorf("%w: GeoJSON segment %d position %d must contain longitude, latitude, and optional altitude", ErrInvalidTrack, segmentIndex, i)
			}
			segment[i] = Point{Lon: *position[0], Lat: *position[1]}
		}
		t.Segments = append(t.Segments, segment)
	}
	return nil
}

// WriteGeoJSON writes a WGS84 LineString or MultiLineString in [lon, lat] order.
// Every segment must contain at least two points. Point timestamps are omitted.
func WriteGeoJSON(w io.Writer, track Track) error {
	if err := track.Validate(); err != nil {
		return err
	}
	if track.CRS != WGS84 {
		return fmt.Errorf("%w: RFC 7946 GeoJSON requires WGS84", ErrCRS)
	}
	lines := make([][][2]float64, len(track.Segments))
	for i, segment := range track.Segments {
		if len(segment) < 2 {
			return fmt.Errorf("%w: GeoJSON segment %d needs at least two points", ErrInvalidTrack, i)
		}
		lines[i] = make([][2]float64, len(segment))
		for j, point := range segment {
			lines[i][j] = [2]float64{point.Lon, point.Lat}
		}
	}
	object := struct {
		Type        string `json:"type"`
		Coordinates any    `json:"coordinates"`
	}{Type: "MultiLineString", Coordinates: lines}
	if len(lines) == 1 {
		object.Type = "LineString"
		object.Coordinates = lines[0]
	}
	return json.NewEncoder(w).Encode(object)
}

func decodeJSON(r io.Reader, target any) error {
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	_, err := decoder.Token()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("after JSON document: %w", err)
	}
	return fmt.Errorf("extra content after JSON document")
}
