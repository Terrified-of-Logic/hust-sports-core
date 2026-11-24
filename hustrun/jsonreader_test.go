package hustrun

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleExport = `{
  "record": {
    "sportid": "a80da7e9-0000-4000-8000-000000000001",
    "uid": "U202400001",
    "name": "测试学生",
    "routeid": 7,
    "routename": "跑步-男生",
    "sporttype": 3,
    "startdate": "2026-06-20 09:50:48",
    "enddate": "2026-06-20 10:04:17",
    "startdatelong": 1784962248000,
    "enddatelong": 1784963057000,
    "dltime": "00:13:24",
    "lastodometertime": "00:00:56",
    "dllc": "3.23",
    "totalstep": 2150,
    "avgspeed": "14.4",
    "avgpace": "04'10''",
    "cal": 180.5,
    "stepminute": "160.1",
    "maxspeedperhour": "14.4",
    "minspeedperhour": "6.0",
    "indoor": 0,
    "activitystatus": 1,
    "status": 0,
    "result": 1,
    "gyrouploadstatus": 0,
    "hasphoto": 0
  },
  "points": [
    {"a": 30.5126406, "o": 114.413853, "v": 1, "s": 0, "t": 0, "ac": 0, "da": 0, "d": 0},
    {"a": 30.5126471, "o": 114.4138542, "v": 1, "s": 15.2, "t": 1784962250000, "ac": 6.5, "da": 1.2, "d": 1.2}
  ],
  "point_count": 2,
  "pace": [{"km": "1", "t": "04'10''", "kmTotallength": 1, "kmdlsj": 250}],
  "minute_speed": [{"min": "1", "v": "14.5"}]
}`

func TestLoadSportJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sport.json")
	if err := os.WriteFile(path, []byte(sampleExport), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	record, err := LoadSportJSON(path, "")
	if err != nil {
		t.Fatalf("LoadSportJSON: %v", err)
	}
	if record.SportID != "a80da7e9-0000-4000-8000-000000000001" {
		t.Errorf("sport id = %q", record.SportID)
	}
	if record.XH != "U202400001" || record.Name != "测试学生" {
		t.Errorf("identity = %q / %q", record.XH, record.Name)
	}
	if record.RouteID != "7" || record.SportType != "3" {
		t.Errorf("route = %q / %q", record.RouteID, record.SportType)
	}
	if record.EndDateLong != 1784963057000 || record.StartDateLong != 1784962248000 {
		t.Errorf("timestamps = %d / %d", record.StartDateLong, record.EndDateLong)
	}
	if len(record.Points) != 2 || record.Points[1].DA != 1.2 {
		t.Errorf("points = %+v", record.Points)
	}
	if record.Points[0].SportbeanID != record.SportID {
		t.Errorf("foreign key = %q", record.Points[0].SportbeanID)
	}
	if len(record.Pace) != 1 || record.Pace[0].KmDLSJ != 250 {
		t.Errorf("pace = %+v", record.Pace)
	}
	if len(record.MinuteSpeed) != 1 || record.MinuteSpeed[0].V != "14.5" {
		t.Errorf("minute speed = %+v", record.MinuteSpeed)
	}
	if record.AvgPace != "04'10''" || record.Cal != 180.5 {
		t.Errorf("statistics = %q / %v", record.AvgPace, record.Cal)
	}
}

func TestLoadSportJSONValidatesSportID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sport.json")
	if err := os.WriteFile(path, []byte(sampleExport), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadSportJSON(path, "different-id"); err == nil {
		t.Error("sport id mismatch accepted")
	}
	if _, err := LoadSportJSON(path, "a80da7e9-0000-4000-8000-000000000001"); err != nil {
		t.Errorf("matching sport id rejected: %v", err)
	}
	if _, err := LoadSportJSON(filepath.Join(dir, "missing.json"), ""); err == nil {
		t.Error("missing file accepted")
	}
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte(`{"points": []}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadSportJSON(badPath, ""); err == nil {
		t.Error("export without a record accepted")
	}
}

func TestListSportJSONIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sport.json")
	if err := os.WriteFile(path, []byte(sampleExport), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ids, err := ListSportJSONIDs(path)
	if err != nil {
		t.Fatalf("ListSportJSONIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a80da7e9-0000-4000-8000-000000000001" {
		t.Errorf("ids = %v", ids)
	}
}
