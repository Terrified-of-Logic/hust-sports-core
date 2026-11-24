package hustrun

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// SportJSONDocument is a database export of one record plus its coordinates.
type SportJSONDocument struct {
	Record      map[string]any   `json:"record"`
	Points      []map[string]any `json:"points"`
	PointCount  int              `json:"point_count"`
	Pace        []map[string]any `json:"pace"`
	MinuteSpeed []map[string]any `json:"minute_speed"`
}

// LoadSportJSON reads a sport_*.json export (sportbean row plus
// sportlatlngbean rows) into a record. Pace and minute-speed rows are optional
// because the export normally omits those child tables.
func LoadSportJSON(path, sportID string) (SportRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SportRecord{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	var document SportJSONDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return SportRecord{}, fmt.Errorf("%w: %s: %v", ErrConfig, path, err)
	}
	if len(document.Record) == 0 {
		return SportRecord{}, fmt.Errorf("%w: JSON 中缺少 record 字段", ErrConfig)
	}
	fileSportID := jsonString(document.Record["sportid"])
	if sportID != "" && fileSportID != sportID {
		return SportRecord{}, fmt.Errorf("%w: JSON 中 sportid (%s) 与请求的 (%s) 不匹配",
			ErrConfig, fileSportID, sportID)
	}

	points := make([]TrackPoint, 0, len(document.Points))
	for _, item := range document.Points {
		points = append(points, TrackPoint{
			A:           jsonFloat(item["a"]),
			O:           jsonFloat(item["o"]),
			V:           jsonInt(item["v"], 1),
			S:           jsonFloat(item["s"]),
			T:           int64(jsonFloat(item["t"])),
			AC:          jsonFloat(item["ac"]),
			DA:          jsonFloat(item["da"]),
			D:           jsonFloat(item["d"]),
			ST:          jsonInt(item["st"], 0),
			STA:         jsonInt(item["sta"], 0),
			SportbeanID: fileSportID,
		})
	}

	record := SportRecord{
		SportID:   fileSportID,
		XH:        jsonString(document.Record["uid"]),
		Name:      jsonString(document.Record["name"]),
		RouteID:   jsonString(document.Record["routeid"]),
		RouteName: jsonString(document.Record["routename"]),
		SportType: jsonString(document.Record["sporttype"]),

		StartDate:        jsonString(document.Record["startdate"]),
		EndDate:          jsonString(document.Record["enddate"]),
		StartDateLong:    int64(jsonFloat(document.Record["startdatelong"])),
		EndDateLong:      int64(jsonFloat(document.Record["enddatelong"])),
		DLTime:           jsonString(document.Record["dltime"]),
		LastOdometerTime: jsonString(document.Record["lastodometertime"]),

		DLLC:            jsonString(document.Record["dllc"]),
		TotalStep:       jsonInt(document.Record["totalstep"], 0),
		AvgSpeed:        jsonString(document.Record["avgspeed"]),
		AvgPace:         jsonString(document.Record["avgpace"]),
		Cal:             jsonFloat(document.Record["cal"]),
		StepMinute:      jsonString(document.Record["stepminute"]),
		MaxSpeedPerHour: jsonString(document.Record["maxspeedperhour"]),
		MinSpeedPerHour: jsonString(document.Record["minspeedperhour"]),

		Indoor:           jsonInt(document.Record["indoor"], 0),
		RoutePolylineBH:  jsonString(document.Record["routepolylinebh"]),
		AlreadyPassPoint: jsonString(document.Record["alreadypasspoint"]),
		ActivityStatus:   jsonInt(document.Record["activitystatus"], 0),
		Status:           jsonInt(document.Record["status"], 0),
		Result:           jsonInt(document.Record["result"], 0),
		GyroUploadStatus: jsonInt(document.Record["gyrouploadstatus"], 0),
		HasPhoto:         jsonInt(document.Record["hasphoto"], 0),

		Points:      points,
		Pace:        parsePaceRows(document.Pace),
		MinuteSpeed: parseMinuteSpeedRows(document.MinuteSpeed),
	}
	return record, nil
}

// ListSportJSONIDs returns the sport ids a JSON export contains.
func ListSportJSONIDs(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	var document SportJSONDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrConfig, path, err)
	}
	if id := jsonString(document.Record["sportid"]); id != "" {
		return []string{id}, nil
	}
	return nil, nil
}

func parsePaceRows(rows []map[string]any) []PaceEntry {
	entries := make([]PaceEntry, 0, len(rows))
	for _, row := range rows {
		total := row["kmTotallength"]
		if total == nil {
			total = row["kmtotallength"]
		}
		entries = append(entries, PaceEntry{
			Km:            jsonString(row["km"]),
			T:             jsonString(row["t"]),
			KmTotalLength: jsonFloat(total),
			KmDLSJ:        jsonInt(row["kmdlsj"], 0),
		})
	}
	return entries
}

func parseMinuteSpeedRows(rows []map[string]any) []MinuteSpeedEntry {
	entries := make([]MinuteSpeedEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, MinuteSpeedEntry{
			Min: jsonString(row["min"]),
			V:   jsonString(row["v"]),
		})
	}
	return entries
}

func jsonString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		if typed == math.Trunc(typed) && math.Abs(typed) < 1e15 {
			return strconv.FormatInt(int64(typed), 10)
		}
		return FormatFloat(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(typed)
	}
}

func jsonFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		out, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0
		}
		return out
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0
		}
		return parsed
	}
	return 0
}

func jsonInt(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		out, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return out
		}
	}
	return fallback
}
