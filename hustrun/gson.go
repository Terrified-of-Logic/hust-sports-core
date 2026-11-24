package hustrun

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// BuildPayloadJSON renders the savesports jsonsports plaintext: the exact field
// set and field order of the official UploadJsonSports.build(bean, true)
// product, serialized the way Gson does (compact, HTML-safe escaping).
func BuildPayloadJSON(record *SportRecord, auth AuthConfig) (string, error) {
	if record == nil {
		return "", fmt.Errorf("%w: record is nil", ErrInvalidRecord)
	}
	return serializePayload(record, auth, true)
}

// BuildPrejudgmentJSON renders the lighter end-of-run payload. The official
// end page asks for the pre-check before the coordinate snapshot is persisted,
// so coordinate, beganPoint and endPoint are absent from that body.
func BuildPrejudgmentJSON(record *SportRecord, auth AuthConfig) (string, error) {
	if record == nil {
		return "", fmt.Errorf("%w: record is nil", ErrInvalidRecord)
	}
	return serializePayload(record, auth, false)
}

func serializePayload(record *SportRecord, auth AuthConfig, withGeometry bool) (string, error) {
	var out jsonWriter
	out.beginObject()

	out.key("activeTime")
	out.stringValue(activeTimeText(record.DLTime))

	out.key("alreadyPassPoint")
	out.stringValue(record.AlreadyPassPoint)

	out.key("alreadyPassPointResult")
	out.stringValue("")

	out.key("avgPace")
	out.stringValue(record.AvgPace)

	out.key("avgSpeed")
	out.stringValue(record.AvgSpeed)

	if withGeometry {
		out.key("beganPoint")
		out.stringValue(pointText(record.Points, 0))
	}

	out.key("beginTime")
	out.stringValue(record.StartDate)

	out.key("calorie")
	out.floatValue(record.Cal)

	if withGeometry {
		out.key("coordinate")
		out.beginArray()
		for _, point := range record.Points {
			value := point.Coordinate()
			out.beginObject()
			out.key("a")
			out.floatValue(value.A)
			out.key("ac")
			out.floatValue(value.AC)
			out.key("d")
			out.floatValue(value.D)
			out.key("da")
			out.floatValue(value.DA)
			out.key("o")
			out.floatValue(value.O)
			out.key("s")
			out.floatValue(value.S)
			out.key("st")
			out.intValue(int64(value.ST))
			out.key("sta")
			out.intValue(int64(value.STA))
			out.key("t")
			out.intValue(value.T)
			out.key("v")
			out.intValue(int64(value.V))
			out.endObject()
		}
		out.endArray()
	}

	if withGeometry {
		out.key("endPoint")
		out.stringValue(pointText(record.Points, -1))
	}

	out.key("endTime")
	out.stringValue(record.EndDate)

	out.key("indoor")
	out.intValue(int64(record.Indoor))

	out.key("isValid")
	out.intValue(int64(record.Result))

	out.key("isValidReason")
	out.stringValue("")

	out.key("lastOdometerTime")
	out.stringValue(record.LastOdometerTime)

	out.key("maxSpeedPerHour")
	out.stringValue(record.MaxSpeedPerHour)

	out.key("minSpeedPerHour")
	out.stringValue(record.MinSpeedPerHour)

	out.key("minuteSpeed")
	out.beginArray()
	for _, entry := range record.MinuteSpeed {
		out.beginObject()
		out.key("min")
		out.stringValue(entry.Min)
		out.key("v")
		out.stringValue(entry.V)
		out.endObject()
	}
	out.endArray()

	out.key("modementMode")
	out.stringValue(record.SportType)

	out.key("name")
	out.stringValue(firstNonEmpty(record.Name, auth.Name))

	out.key("needPassPointCount")
	out.stringValue("0")

	out.key("odometer")
	out.stringValue(odometerText(record))

	out.key("pace")
	out.beginArray()
	for _, entry := range record.Pace {
		out.beginObject()
		out.key("km")
		out.stringValue(entry.Km)
		out.key("kmTotallength")
		out.floatValue(entry.KmTotalLength)
		out.key("kmdlsj")
		out.intValue(int64(entry.KmDLSJ))
		out.key("t")
		out.stringValue(entry.T)
		out.endObject()
	}
	out.endArray()

	out.key("phoneVersion")
	out.stringValue(auth.PhoneVersion)

	out.key("planRouteName")
	out.stringValue(record.RouteName)

	out.key("routeId")
	out.stringValue(record.RouteID)

	out.key("sportId")
	out.stringValue(record.SportID)

	out.key("stepCount")
	out.intValue(int64(record.TotalStep))

	out.key("stepMinute")
	out.stringValue(record.StepMinute)

	out.key("xh")
	out.stringValue(firstNonEmpty(record.XH, auth.XH))

	if record.RoutePolylineBH != "" {
		out.key("routePolylineBh")
		out.stringValue(record.RoutePolylineBH)
	}

	// Random check-in points belong to sport type 5 only; a normal run must not
	// be turned into an empty check-in activity.
	if record.SportType == "5" {
		out.key("randomPointStr")
		out.beginArray()
		out.endArray()
	}

	out.endObject()
	return out.string(), nil
}

func activeTimeText(value string) string {
	replaced := strings.ReplaceAll(value, "'", ":")
	replaced = strings.ReplaceAll(replaced, `"`, "")
	return strings.TrimSpace(replaced)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func pointText(points []TrackPoint, index int) string {
	if len(points) == 0 {
		return ""
	}
	if index < 0 {
		index = len(points) + index
	}
	if index < 0 || index >= len(points) {
		return ""
	}
	point := points[index]
	return FormatFloat(point.A) + "|" + FormatFloat(point.O)
}

func odometerText(record *SportRecord) string {
	if record.DLLC != "" && record.DLLC != "0" && record.DLLC != "0.0" {
		return record.DLLC
	}
	if len(record.Points) > 0 {
		return FormatFloat(record.Points[len(record.Points)-1].DA / 1000.0)
	}
	return record.DLLC
}

// FormatFloat renders a float the way Python's repr does for the value ranges
// this protocol carries: shortest round-tripping decimal, always with a
// fractional part.
func FormatFloat(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	absolute := math.Abs(value)
	var text string
	if absolute != 0 && (absolute < 1e-4 || absolute >= 1e16) {
		text = strconv.FormatFloat(value, 'g', -1, 64)
	} else {
		text = strconv.FormatFloat(value, 'f', -1, 64)
	}
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return text
}

// FormatDecimal renders value with digits decimals and strips trailing zeros,
// matching the reference's f"{value:.2f}".rstrip("0").rstrip(".").
func FormatDecimal(value float64, digits int) string {
	text := strconv.FormatFloat(value, 'f', digits, 64)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(text, "0")
		text = strings.TrimRight(text, ".")
	}
	return text
}

// jsonWriter emits compact JSON with a fixed key order.
type jsonWriter struct {
	buf   bytes.Buffer
	empty []bool
}

func (w *jsonWriter) beginObject() {
	w.separate()
	w.buf.WriteByte('{')
	w.empty = append(w.empty, true)
}

func (w *jsonWriter) endObject() {
	w.buf.WriteByte('}')
	w.empty = w.empty[:len(w.empty)-1]
}

func (w *jsonWriter) beginArray() {
	w.separate()
	w.buf.WriteByte('[')
	w.empty = append(w.empty, true)
}

func (w *jsonWriter) endArray() {
	w.buf.WriteByte(']')
	w.empty = w.empty[:len(w.empty)-1]
}

func (w *jsonWriter) key(name string) {
	w.separate()
	appendJSONString(&w.buf, name)
	w.buf.WriteByte(':')
	// The value that follows must not emit a separator of its own.
	w.empty[len(w.empty)-1] = true
}

func (w *jsonWriter) stringValue(value string) {
	w.separate()
	appendJSONString(&w.buf, value)
}

func (w *jsonWriter) intValue(value int64) {
	w.separate()
	w.buf.WriteString(strconv.FormatInt(value, 10))
}

func (w *jsonWriter) floatValue(value float64) {
	w.separate()
	w.buf.WriteString(FormatFloat(value))
}

func (w *jsonWriter) rawValue(value string) {
	w.separate()
	w.buf.WriteString(value)
}

// separate writes a comma when the current container already holds an element,
// then marks the container non-empty.
func (w *jsonWriter) separate() {
	if len(w.empty) == 0 {
		return
	}
	top := len(w.empty) - 1
	if !w.empty[top] {
		w.buf.WriteByte(',')
	}
	w.empty[top] = false
}

func (w *jsonWriter) string() string { return w.buf.String() }

// appendJSONString writes a JSON string literal using the official app's
// escaping: standard JSON escapes plus Gson's HTML-safe replacements for
// ', <, >, & and =, and the JavaScript line separators.
func appendJSONString(buf *bytes.Buffer, value string) {
	buf.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\'':
			buf.WriteString(`\u0027`)
		case '<':
			buf.WriteString(`\u003c`)
		case '>':
			buf.WriteString(`\u003e`)
		case '&':
			buf.WriteString(`\u0026`)
		case '=':
			buf.WriteString(`\u003d`)
		case '\u2028':
			buf.WriteString(`\u2028`)
		case '\u2029':
			buf.WriteString(`\u2029`)
		default:
			if char < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, char)
				continue
			}
			buf.WriteRune(char)
		}
	}
	buf.WriteByte('"')
}
