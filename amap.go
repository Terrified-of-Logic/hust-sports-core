package sports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const amapEndpoint = "https://restapi.amap.com/v3/assistant/coordinate/convert"
const amapBatchSize = 40

// ErrAMap indicates that an AMap request failed or returned an invalid response.
var ErrAMap = errors.New("AMap coordinate conversion failed")

// AMapConverter transforms WGS84 tracks to AMap's GCJ-02 coordinates using
// the public AMap Web Service API. Construct it with NewAMapConverter.
type AMapConverter struct {
	key    string
	client *http.Client
}

// NewAMapConverter configures an AMap Web Service key and an optional HTTP
// client. A nil client uses a ten-second timeout per batch. Redirects are
// disabled to keep coordinates and the key on the AMap endpoint.
func NewAMapConverter(key string, client *http.Client) (*AMapConverter, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("%w: a Web Service API key is required", ErrAMap)
	}
	configured := http.Client{Timeout: 10 * time.Second}
	if client != nil {
		configured = *client
	}
	configured.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &AMapConverter{key: key, client: &configured}, nil
}

// ConvertTrack returns an independent GCJ-02 copy for map display. WGS84
// points are sent to AMap in batches; GCJ-02 input is copied locally. Point
// order, segment boundaries, and timestamps are preserved. On error, no
// partial track is returned and the input is untouched.
func (c *AMapConverter) ConvertTrack(ctx context.Context, track Track) (Track, error) {
	if c == nil || c.client == nil {
		return Track{}, fmt.Errorf("%w: use NewAMapConverter to initialize the converter", ErrAMap)
	}
	if err := ctx.Err(); err != nil {
		return Track{}, err
	}
	if err := track.Validate(); err != nil {
		return Track{}, err
	}
	result := Track{CRS: GCJ02, Segments: make([][]Point, len(track.Segments))}
	batch := make([]*Point, 0, amapBatchSize)
	for i, source := range track.Segments {
		segment := slices.Clone(source)
		result.Segments[i] = segment
		if track.CRS == GCJ02 {
			continue
		}
		for j := range segment {
			batch = append(batch, &segment[j])
			if len(batch) == amapBatchSize {
				if err := c.convertBatch(ctx, batch); err != nil {
					return Track{}, err
				}
				batch = batch[:0]
			}
		}
	}
	if len(batch) > 0 {
		if err := c.convertBatch(ctx, batch); err != nil {
			return Track{}, err
		}
	}
	return result, nil
}

func (c *AMapConverter) convertBatch(ctx context.Context, points []*Point) error {
	locations := make([]string, len(points))
	for i, point := range points {
		locations[i] = strconv.FormatFloat(point.Lon, 'f', 6, 64) + "," + strconv.FormatFloat(point.Lat, 'f', 6, 64)
	}
	query := url.Values{
		"key":       {c.key},
		"coordsys":  {"gps"},
		"output":    {"JSON"},
		"locations": {strings.Join(locations, "|")},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, amapEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return fmt.Errorf("%w: cannot construct request", ErrAMap)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return amapRequestError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP status %d", ErrAMap, response.StatusCode)
	}

	var payload struct {
		Status    json.Number `json:"status"`
		InfoCode  json.Number `json:"infocode"`
		Locations string      `json:"locations"`
	}
	body := &io.LimitedReader{R: response.Body, N: 16<<10 + 1}
	if err := decodeJSON(body, &payload); err != nil || body.N == 0 {
		return amapRequestError(ctx, err)
	}
	if payload.Status != "1" {
		if code, err := payload.InfoCode.Int64(); err == nil {
			return fmt.Errorf("%w: service code %d", ErrAMap, code)
		}
		return fmt.Errorf("%w: service rejected the request", ErrAMap)
	}
	converted := strings.Split(payload.Locations, ";")
	if len(converted) != len(points) {
		return fmt.Errorf("%w: expected %d points, received %d", ErrAMap, len(points), len(converted))
	}
	for i, pair := range converted {
		lon, lat, _ := strings.Cut(pair, ",")
		x, xErr := strconv.ParseFloat(strings.TrimSpace(lon), 64)
		y, yErr := strconv.ParseFloat(strings.TrimSpace(lat), 64)
		if xErr != nil || yErr != nil || !(Point{Lon: x, Lat: y}).valid() {
			return fmt.Errorf("%w: invalid coordinates at response position %d", ErrAMap, i)
		}
		points[i].Lon, points[i].Lat = x, y
	}
	return nil
}

func amapRequestError(ctx context.Context, err error) error {
	// Transport errors can include the full URL and key. Keep only safe causes.
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrAMap, context.DeadlineExceeded)
	}
	return fmt.Errorf("%w: request or response could not be processed", ErrAMap)
}
