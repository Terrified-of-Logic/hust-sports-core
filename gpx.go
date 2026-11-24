package sports

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ReadGPX reads WGS84 track segments (trk/trkseg/trkpt) from GPX 1.0 or 1.1.
// It preserves point timestamps and segment gaps, and skips empty segments.
// Route elements, waypoints, elevation, and extension data are ignored.
func ReadGPX(r io.Reader) (Track, error) {
	decoder := xml.NewDecoder(r)
	start, err := gpxRoot(decoder)
	if err != nil {
		return Track{}, fmt.Errorf("%w: GPX: %w", ErrInvalidTrack, err)
	}
	var document struct {
		Tracks []struct {
			XMLName  xml.Name
			Segments []struct {
				XMLName xml.Name
				Points  []gpxPoint `xml:"trkpt"`
			} `xml:"trkseg"`
		} `xml:"trk"`
	}
	if err := decoder.DecodeElement(&document, &start); err != nil {
		return Track{}, fmt.Errorf("%w: GPX: %w", ErrInvalidTrack, err)
	}
	if _, err := gpxRoot(decoder); err != io.EOF {
		return Track{}, fmt.Errorf("%w: extra content after GPX document", ErrInvalidTrack)
	}

	track := Track{CRS: WGS84}
	for _, sourceTrack := range document.Tracks {
		if sourceTrack.XMLName.Space != start.Name.Space {
			continue
		}
		for _, sourceSegment := range sourceTrack.Segments {
			if sourceSegment.XMLName.Space != start.Name.Space {
				continue
			}
			segment := make([]Point, 0, len(sourceSegment.Points))
			for _, sourcePoint := range sourceSegment.Points {
				if sourcePoint.XMLName.Space != start.Name.Space {
					continue
				}
				point, err := sourcePoint.point()
				if err != nil {
					return Track{}, fmt.Errorf("%w: GPX segment %d point %d: %w", ErrInvalidTrack, len(track.Segments), len(segment), err)
				}
				segment = append(segment, point)
			}
			if len(segment) > 0 {
				track.Segments = append(track.Segments, segment)
			}
		}
	}
	if err := track.Validate(); err != nil {
		return Track{}, err
	}
	return track, nil
}

func gpxRoot(decoder *xml.Decoder) (xml.StartElement, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			switch token.Name.Space {
			case "", "http://www.topografix.com/GPX/1/0", "http://www.topografix.com/GPX/1/1":
				if token.Name.Local == "gpx" {
					return token, nil
				}
			}
		case xml.ProcInst, xml.Comment:
			continue
		case xml.CharData:
			if strings.TrimSpace(string(token)) == "" {
				continue
			}
		}
		return xml.StartElement{}, fmt.Errorf("expected a GPX document")
	}
}

type gpxPoint struct {
	XMLName xml.Name
	Lat     string `xml:"lat,attr"`
	Lon     string `xml:"lon,attr"`
	Times   []struct {
		XMLName xml.Name
		Value   string `xml:",chardata"`
	} `xml:"time"`
}

func (p gpxPoint) point() (Point, error) {
	lat, latErr := strconv.ParseFloat(strings.TrimSpace(p.Lat), 64)
	lon, lonErr := strconv.ParseFloat(strings.TrimSpace(p.Lon), 64)
	point := Point{Lat: lat, Lon: lon}
	if latErr != nil || lonErr != nil || !point.valid() {
		return Point{}, fmt.Errorf("invalid or missing lat/lon")
	}
	var hasTimestamp bool
	for _, element := range p.Times {
		if element.XMLName.Space != p.XMLName.Space {
			continue
		}
		if hasTimestamp {
			return Point{}, fmt.Errorf("duplicate timestamp")
		}
		timestamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(element.Value))
		if err != nil {
			return Point{}, fmt.Errorf("timestamp must use RFC 3339 with a timezone")
		}
		point.Time = timestamp
		hasTimestamp = true
	}
	return point, nil
}
