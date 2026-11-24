package hustrun

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleRecord() SportRecord {
	return SportRecord{
		SportID:   "00000000-0000-4000-8000-000000000001",
		XH:        "U000000000",
		Name:      "测试'学生",
		RouteID:   "7",
		RouteName: "跑步-男生",
		SportType: "3",

		StartDate:        "2026-06-20 09:50:48",
		EndDate:          "2026-06-20 10:04:17",
		StartDateLong:    1784962248000,
		EndDateLong:      1784963057000,
		DLTime:           "00:13:24",
		LastOdometerTime: "00:00:56",

		DLLC:            "3.23",
		TotalStep:       2150,
		AvgSpeed:        "14.4",
		AvgPace:         "04'10''",
		Cal:             180.5,
		StepMinute:      "160.1",
		MaxSpeedPerHour: "14.4",
		MinSpeedPerHour: "6.0",

		ActivityStatus: 1,
		Result:         1,
		Points: []TrackPoint{
			{A: 30.5126406, O: 114.413853, V: 1, T: 0},
			{A: 30.5126471, O: 114.4138542, V: 1, S: 15.2, T: 1784962250, AC: 6.5, DA: 1.2, D: 1.2},
		},
		Pace: []PaceEntry{
			{Km: "1", T: "04'10''", KmTotalLength: 1, KmDLSJ: 250},
		},
		MinuteSpeed: []MinuteSpeedEntry{
			{Min: "1", V: "14.5"},
		},
	}
}

func TestPayloadFieldOrderAndEscaping(t *testing.T) {
	record := sampleRecord()
	payload, err := BuildPayloadJSON(&record, AuthConfig{Name: "备用", PhoneVersion: "PJE110,35,15|1.0.9"})
	if err != nil {
		t.Fatalf("BuildPayloadJSON: %v", err)
	}

	order := []string{
		`"activeTime"`, `"alreadyPassPoint"`, `"alreadyPassPointResult"`, `"avgPace"`, `"avgSpeed"`,
		`"beganPoint"`, `"beginTime"`, `"calorie"`, `"coordinate"`, `"endPoint"`, `"endTime"`,
		`"indoor"`, `"isValid"`, `"isValidReason"`, `"lastOdometerTime"`, `"maxSpeedPerHour"`,
		`"minSpeedPerHour"`, `"minuteSpeed"`, `"modementMode"`, `"name"`, `"needPassPointCount"`,
		`"odometer"`, `"pace"`, `"phoneVersion"`, `"planRouteName"`, `"routeId"`, `"sportId"`,
		`"stepCount"`, `"stepMinute"`, `"xh"`,
	}
	position := 0
	for _, key := range order {
		index := strings.Index(payload[position:], key)
		if index < 0 {
			t.Fatalf("key %s missing or out of order in %s", key, truncate(payload, 200))
		}
		position += index
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if decoded["sportId"] != record.SportID {
		t.Errorf("sportId = %v", decoded["sportId"])
	}
	coordinate, ok := decoded["coordinate"].([]any)
	if !ok || len(coordinate) != len(record.Points) {
		t.Fatalf("coordinate = %v", decoded["coordinate"])
	}
	first := coordinate[0].(map[string]any)
	if _, exists := first["baseObjId"]; exists {
		t.Error("internal LitePal field leaked into the payload")
	}
	if _, exists := first["sportbean_id"]; exists {
		t.Error("foreign key leaked into the payload")
	}

	expectedFirst := `{"a":30.5126406,"ac":0.0,"d":0.0,"da":0.0,"o":114.413853,"s":0.0,"st":0,"sta":0,"t":0,"v":1}`
	coordinateStart := strings.Index(payload, `"coordinate":[`) + len(`"coordinate":[`)
	if !strings.HasPrefix(payload[coordinateStart:], expectedFirst) {
		t.Errorf("first coordinate = %s", truncate(payload[coordinateStart:], 140))
	}
	if decoded["isValid"] != float64(1) {
		t.Errorf("isValid = %v", decoded["isValid"])
	}
	if decoded["name"] != "测试'学生" {
		t.Errorf("name = %v", decoded["name"])
	}
	if !strings.Contains(payload, `测试\u0027学生`) {
		t.Error("Gson HTML-safe escaping for the apostrophe is missing")
	}
	if !strings.Contains(payload, `"avgPace":"04\u0027`) && !strings.Contains(payload, `04\u0027`) {
		t.Error("pace apostrophes must be escaped")
	}
}

func TestPayloadEscapesHTMLSensitiveCharacters(t *testing.T) {
	record := sampleRecord()
	record.Name = "a<b>c&d=e"
	payload, err := BuildPayloadJSON(&record, AuthConfig{})
	if err != nil {
		t.Fatalf("BuildPayloadJSON: %v", err)
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`, `\u003d`} {
		if !strings.Contains(payload, escaped) {
			t.Errorf("payload is missing escape %s", escaped)
		}
	}
	if strings.Contains(payload, "a<b>c") {
		t.Error("raw HTML characters leaked into the payload")
	}
}

func TestPayloadNameAndIdentityFallbacks(t *testing.T) {
	record := sampleRecord()
	record.Name = ""
	record.XH = ""
	payload, err := BuildPayloadJSON(&record, AuthConfig{Name: "认证姓名", XH: "U999"})
	if err != nil {
		t.Fatalf("BuildPayloadJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["name"] != "认证姓名" {
		t.Errorf("name fallback = %v", decoded["name"])
	}
	if decoded["xh"] != "U999" {
		t.Errorf("xh fallback = %v", decoded["xh"])
	}
}

func TestPayloadOdometerFallbackAndSportType(t *testing.T) {
	record := sampleRecord()
	record.DLLC = "0"
	record.SportType = "5"
	payload, err := BuildPayloadJSON(&record, AuthConfig{})
	if err != nil {
		t.Fatalf("BuildPayloadJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["odometer"] != "0.0012" {
		t.Errorf("odometer = %v, want the last da in kilometres", decoded["odometer"])
	}
	if _, exists := decoded["randomPointStr"]; !exists {
		t.Error("sport type 5 must carry randomPointStr")
	}

	record.SportType = "3"
	payload, err = BuildPayloadJSON(&record, AuthConfig{})
	if err != nil {
		t.Fatalf("BuildPayloadJSON: %v", err)
	}
	if strings.Contains(payload, "randomPointStr") {
		t.Error("a normal run must not carry check-in fields")
	}
}

func TestPrejudgmentOmitsGeometry(t *testing.T) {
	record := sampleRecord()
	payload, err := BuildPrejudgmentJSON(&record, AuthConfig{})
	if err != nil {
		t.Fatalf("BuildPrejudgmentJSON: %v", err)
	}
	for _, key := range []string{`"coordinate"`, `"beganPoint"`, `"endPoint"`} {
		if strings.Contains(payload, key) {
			t.Errorf("prejudgment payload must omit %s", key)
		}
	}
	for _, key := range []string{`"avgSpeed"`, `"minuteSpeed"`, `"pace"`, `"odometer"`} {
		if !strings.Contains(payload, key) {
			t.Errorf("prejudgment payload must keep %s", key)
		}
	}
}

func TestFormatHelpers(t *testing.T) {
	cases := []struct {
		value  float64
		digits int
		want   string
	}{
		{14.40, 2, "14.4"},
		{6.00, 2, "6"},
		{6.05, 2, "6.05"},
		{2.1512, 2, "2.15"},
		{0.0, 2, "0"},
	}
	for _, testCase := range cases {
		if got := FormatDecimal(testCase.value, testCase.digits); got != testCase.want {
			t.Errorf("FormatDecimal(%v, %d) = %q, want %q", testCase.value, testCase.digits, got, testCase.want)
		}
	}
	if got := FormatFloat(30.5126406); got != "30.5126406" {
		t.Errorf("FormatFloat = %q", got)
	}
	if got := FormatFloat(2.0); got != "2.0" {
		t.Errorf("FormatFloat(2.0) = %q", got)
	}
	if got := FormatPace(250); got != "04'10''" {
		t.Errorf("FormatPace(250) = %q", got)
	}
	if got := FormatDuration(804); got != "00:13:24" {
		t.Errorf("FormatDuration(804) = %q", got)
	}
}
