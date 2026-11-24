// Package hustrun implements the protocol client behind the campus running
// upload chain: credential loading, request signing, track generation,
// payload construction, the HTTP endpoints, and the prepare/submit pipeline.
//
// The package performs no network I/O unless a caller uses Client, Login, or
// the pipeline functions. Everything else (generation, payload building,
// consistency checks, mutation, sensor packing) is pure and offline, which is
// what the tests exercise.
package hustrun

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors. Callers can classify failures with errors.Is.
var (
	// ErrConfig reports missing or malformed configuration, credentials or
	// device fingerprint values.
	ErrConfig = errors.New("hustrun: invalid configuration")
	// ErrSigner reports an unusable signing key or signing target.
	ErrSigner = errors.New("hustrun: request signer unavailable")
	// ErrInvalidRecord reports a record that fails local consistency checks.
	ErrInvalidRecord = errors.New("hustrun: inconsistent sport record")
	// ErrProtocol reports an unexpected server response shape.
	ErrProtocol = errors.New("hustrun: unexpected protocol response")
)

// TrackPoint is one coordinate sample of a running record. The single-letter
// names match the wire format of the official client: a=latitude, o=longitude,
// v=speed flag, s=instantaneous speed in km/h, t=epoch milliseconds,
// ac=location accuracy in metres, da=cumulative metres, d=segment metres.
type TrackPoint struct {
	A  float64 `json:"a"`
	O  float64 `json:"o"`
	V  int     `json:"v"`
	S  float64 `json:"s"`
	T  int64   `json:"t"`
	AC float64 `json:"ac"`
	DA float64 `json:"da"`
	D  float64 `json:"d"`
	ST int     `json:"st"`
	// STA mirrors the second status column of the local database row.
	STA int `json:"sta"`
	// SportbeanID is the LitePal foreign key. It is carried so imported rows
	// round-trip, and stripped when the payload is built.
	SportbeanID string `json:"sportbean_id,omitempty"`
}

// CoordinateValue is the wire form of one TrackPoint inside the payload's
// coordinate array. Field order in the serialized object is a, ac, d, da, o,
// s, st, sta, t, v.
type CoordinateValue struct {
	A  float64
	AC float64
	D  float64
	DA float64
	O  float64
	S  float64
	ST int
	// STA mirrors the second status column.
	STA int
	T   int64
	V   int
}

// Coordinate converts a point to its payload representation.
func (p TrackPoint) Coordinate() CoordinateValue {
	return CoordinateValue{
		A: p.A, AC: p.AC, D: p.D, DA: p.DA, O: p.O, S: p.S,
		ST: p.ST, STA: p.STA, T: p.T, V: p.V,
	}
}

// PaceEntry is one per-kilometre pace row (the cpacestr table).
type PaceEntry struct {
	Km string `json:"km"`
	// T is formatted like 05'30''.
	T string `json:"t"`
	// KmTotalLength is the cumulative kilometre count at the segment boundary.
	KmTotalLength float64 `json:"km_total_length"`
	// KmDLSJ is the cumulative exercise seconds at that boundary.
	KmDLSJ int `json:"km_dlsj"`
}

// MinuteSpeedEntry is one per-minute speed row (the cminuteSpeedstr table).
type MinuteSpeedEntry struct {
	Min string `json:"min"`
	V   string `json:"v"`
}

// SportRecord is a complete running record plus its child rows.
type SportRecord struct {
	SportID string `json:"sport_id"`
	XH      string `json:"xh"`
	Name    string `json:"name"`

	RouteID   string `json:"route_id"`
	RouteName string `json:"route_name"`
	SportType string `json:"sport_type"`

	StartDate     string `json:"start_date"`
	EndDate       string `json:"end_date"`
	StartDateLong int64  `json:"start_date_long"`
	EndDateLong   int64  `json:"end_date_long"`
	DLTime        string `json:"dl_time"`
	// LastOdometerTime is not the total duration; keep whatever the source
	// said instead of deriving it.
	LastOdometerTime string `json:"last_odometer_time"`

	DLLC            string  `json:"dllc"`
	TotalStep       int     `json:"total_step"`
	AvgSpeed        string  `json:"avg_speed"`
	AvgPace         string  `json:"avg_pace"`
	Cal             float64 `json:"cal"`
	StepMinute      string  `json:"step_minute"`
	MaxSpeedPerHour string  `json:"max_speed_per_hour"`
	MinSpeedPerHour string  `json:"min_speed_per_hour"`

	Indoor           int    `json:"indoor"`
	RoutePolylineBH  string `json:"route_polyline_bh,omitempty"`
	AlreadyPassPoint string `json:"already_pass_point,omitempty"`
	ActivityStatus   int    `json:"activity_status"`
	Status           int    `json:"status"`
	Result           int    `json:"result"`
	GyroUploadStatus int    `json:"gyro_upload_status"`
	HasPhoto         int    `json:"has_photo"`

	Points      []TrackPoint       `json:"points"`
	Pace        []PaceEntry        `json:"pace"`
	MinuteSpeed []MinuteSpeedEntry `json:"minute_speed"`
}

// DistanceKm returns the recorded distance in kilometres, falling back to the
// last cumulative sample when dllc is not a number.
func (r SportRecord) DistanceKm() float64 {
	if value, err := parseFloat(r.DLLC); err == nil {
		return value
	}
	if len(r.Points) > 0 {
		return r.Points[len(r.Points)-1].DA / 1000.0
	}
	return 0
}

// DurationSeconds returns the wall-clock seconds between the record's start and
// end timestamps.
func (r SportRecord) DurationSeconds() int {
	if r.StartDateLong > 0 && r.EndDateLong > 0 {
		return int((r.EndDateLong - r.StartDateLong) / 1000)
	}
	return 0
}

// IsValidRecord reports whether the record satisfies the coarse shape checks
// the reference implementation uses (distance, sample count, identity).
func (r SportRecord) IsValidRecord() bool {
	return r.DistanceKm() >= 3.0 && len(r.Points) > 10 && r.SportID != "" && r.XH != ""
}

// Summary renders a one-line description of the record.
func (r SportRecord) Summary() string {
	id := r.SportID
	if len(id) > 8 {
		id = id[:8]
	}
	return fmt.Sprintf("[%s...] %s → %s | %skm | %d步 | %skm/h | %s | %d轨迹点",
		id, r.StartDate, r.EndDate, r.DLLC, r.TotalStep, r.AvgSpeed, r.AvgPace, len(r.Points))
}

// UploadResult is the outcome of a savesports call.
type UploadResult struct {
	Success       bool
	SportID       string
	IsValid       string
	InValidReason string
	CheckStatus   string
	Tip           string
	RawResponse   string
	SensorZipOK   bool
	SensorFileOK  bool
	HTTPStatus    int
	APICode       string
	APIMessage    string
}

// ServerProbe is the result of asking the server for its clock.
type ServerProbe struct {
	ServerTime int64
	HTTPStatus int
	APICode    string
	Message    string
}

// OK reports whether a usable millisecond timestamp was parsed.
func (p ServerProbe) OK() bool { return p.ServerTime > 0 }

// InSportTimeResult is the server's judgement on the current exercise window.
type InSportTimeResult struct {
	Allowed    *bool
	HTTPStatus int
	APICode    string
	Message    string
}

// OK reports whether the server answered the window question at all.
func (r InSportTimeResult) OK() bool { return r.Allowed != nil }

// SportsIDResult is the server-issued sport identifier that precedes a run.
type SportsIDResult struct {
	SportID    string
	HTTPStatus int
	APICode    string
	Message    string
}

// OK reports whether an identifier was returned.
func (r SportsIDResult) OK() bool { return r.SportID != "" }

// PrejudgmentCheck is one item of the end-of-run prejudgment list.
type PrejudgmentCheck struct {
	IsValid string
	Content string
}

// PrejudgmentResult is the server's end-of-run pre-check.
type PrejudgmentResult struct {
	Success    bool
	Checks     []PrejudgmentCheck
	HTTPStatus int
	APICode    string
	Message    string
}

// OK reports whether every prejudgment item passed.
func (r PrejudgmentResult) OK() bool {
	if !r.Success || len(r.Checks) == 0 {
		return false
	}
	for _, check := range r.Checks {
		if check.IsValid != "1" {
			return false
		}
	}
	return true
}

// Failures returns the prejudgment items that did not pass.
func (r PrejudgmentResult) Failures() []PrejudgmentCheck {
	var failed []PrejudgmentCheck
	for _, check := range r.Checks {
		if check.IsValid != "1" {
			failed = append(failed, check)
		}
	}
	return failed
}

// AuthConfig is the credential, endpoint and device fingerprint set used for
// every business request.
type AuthConfig struct {
	Token       string
	Secret      string
	Sign        string
	Timestamp   string
	WebViewTime string

	XH   string
	Name string

	ServerURL    string
	LoginOutTime string

	AESEncoding  string
	TokenHeader  string
	IMEI         string
	OAID         string
	UserAgent    string
	AppKey       string
	V1Seed       string
	IsRoot       bool
	PhoneVersion string
	SkipSensor   bool
	SensorPath   string
	SensorSHA256 string
}

// AESKey returns the first 16 bytes of SECRET, the AES-128 key the client uses
// for payloads, the sensor blob and the anti-cheat client header.
func (a AuthConfig) AESKey() (string, error) {
	if len(a.Secret) < 16 {
		return "", fmt.Errorf("%w: SECRET must supply a 16-byte AES key", ErrConfig)
	}
	return a.Secret[:16], nil
}

// IsTokenExpired reports whether the JWT in Token is missing, unparsable or
// past its exp claim.
func (a AuthConfig) IsTokenExpired() bool {
	return isJWTExpired(a.Token, time.Now())
}

// MissingSessionFields lists absent credential fields.
func (a AuthConfig) MissingSessionFields() []string {
	return missing(map[string]string{"TOKEN": a.Token, "SECRET": a.Secret})
}

// MissingDeviceFields lists absent device fingerprint fields.
func (a AuthConfig) MissingDeviceFields() []string {
	return missing(map[string]string{"imei": a.IMEI, "User-Agent": a.UserAgent})
}

func missing(fields map[string]string) []string {
	names := make([]string, 0, len(fields))
	for name, value := range fields {
		if strings.TrimSpace(value) == "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// HeadersConfigured reports whether a signer can be built from this config.
func (a AuthConfig) HeadersConfigured() error {
	if absent := append(a.MissingSessionFields(), a.MissingDeviceFields()...); len(absent) > 0 {
		return fmt.Errorf("%w: missing %s", ErrConfig, strings.Join(absent, ", "))
	}
	if a.TokenHeader != "" && a.TokenHeader != "cgAuthorization" {
		return fmt.Errorf("%w: token header must be cgAuthorization", ErrConfig)
	}
	if _, err := a.AESKey(); err != nil {
		return err
	}
	return nil
}

func parseFloat(value string) (float64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return math.NaN(), errors.New("empty number")
	}
	return strconv.ParseFloat(trimmed, 64)
}
