package hustrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Officially allowed exercise window (hours in the server's time zone).
const (
	AllowedHourStart = 6
	AllowedHourEnd   = 23

	// SessionVersion is the on-disk layout version of a prepared session.
	SessionVersion = 1
)

// SessionDir is where prepare/submit pass the claimed sportId and the generated
// record between processes.
var SessionDir = ".sessions"

// Progress receives human-readable progress lines. It may be nil.
type Progress func(string)

func report(progress Progress, message string) {
	if progress != nil {
		progress(message)
	}
}

// UploadOptions configures a full upload or a prepare step.
type UploadOptions struct {
	RoutePath        string
	TargetDistanceM  float64
	Seed             *int64
	Fast             bool
	SkipSensor       bool
	DryRun           bool
	OutputPayload    string
	WaitExtraSeconds float64
	SportType        string
	RouteID          string
	RouteName        string
}

func (o *UploadOptions) withDefaults() {
	if o.TargetDistanceM == 0 {
		o.TargetDistanceM = 2150
	}
	if o.SportType == "" {
		o.SportType = DefaultSportType
	}
	if o.RouteID == "" {
		o.RouteID = "7"
	}
	if o.RouteName == "" {
		o.RouteName = DefaultRouteName
	}
	if o.WaitExtraSeconds == 0 {
		o.WaitExtraSeconds = 3.0
	}
}

// SubmitOptions configures the second phase of a split upload.
type SubmitOptions struct {
	SkipSensor    bool
	NoWait        bool
	OutputPayload string
}

// FlowReport is the outcome of one pipeline run.
type FlowReport struct {
	Result            UploadResult
	Record            *SportRecord
	PrejudgmentOK     bool
	ServerSportID     string
	SportIDIssuedAtMS *int64
	WaitedSeconds     float64
	SessionPath       string
	Notes             []string
}

func failure(message string, notes ...string) FlowReport {
	return FlowReport{Result: UploadResult{RawResponse: message}, Notes: notes}
}

// UploadRoute claims a sportId, generates a record, waits until the record's
// clock has really elapsed and submits it.
func UploadRoute(ctx context.Context, configPath string, options *UploadOptions, progress Progress) FlowReport {
	if options == nil {
		return failure("缺少上传参数")
	}
	options.withDefaults()

	auth, err := LoadAuthConfig(configPath)
	if err != nil {
		return failure(err.Error())
	}
	if !options.DryRun {
		if absent := auth.MissingSessionFields(); len(absent) > 0 {
			return failure("登录凭证缺失或已过期，请先运行 login 刷新凭证", absent...)
		}
		if auth.IsTokenExpired() {
			return failure("TOKEN 已过期，请先运行 login 刷新凭证", "token_expired")
		}
	} else if absent := auth.MissingSessionFields(); len(absent) > 0 {
		return failure("登录凭证缺失，请先运行 login", absent...)
	}

	geometry, err := LoadRoute(options.RoutePath)
	if err != nil {
		return failure(err.Error())
	}
	report(progress, fmt.Sprintf("[路径] %s: %.0f m", options.RoutePath, geometry.LengthM))

	if options.DryRun {
		record, genErr := GenerateRouteRecord(geometry, generateOptionsFor(options, auth, time.Now().UnixMilli(), "00000000-0000-4000-8000-000000000000"))
		if genErr != nil {
			return failure(genErr.Error())
		}
		report(progress, "[生成] "+RouteSummary(record))
		if err := dumpPayload(record, *auth, options.OutputPayload, progress); err != nil {
			return failure(err.Error())
		}
		return FlowReport{
			Result: UploadResult{RawResponse: "dry-run"},
			Record: &record,
			Notes:  []string{"dry-run：未访问网络"},
		}
	}

	client, clientErr := NewClient(auth, NativeSigner{}, nil)
	if clientErr != nil {
		return failure(clientErr.Error())
	}
	return runNetworkFlow(ctx, client, auth, geometry, options, progress)
}

// PrepareRoute claims the sportId and persists the generated record without
// submitting it.
func PrepareRoute(ctx context.Context, configPath string, options *UploadOptions, progress Progress) FlowReport {
	if options == nil {
		return failure("缺少上传参数")
	}
	options.withDefaults()

	auth, err := LoadAuthConfig(configPath)
	if err != nil {
		return failure(err.Error())
	}
	if absent := auth.MissingSessionFields(); len(absent) > 0 {
		return failure("登录凭证缺失或已过期，请先运行 login 刷新凭证", absent...)
	}
	if auth.IsTokenExpired() {
		return failure("TOKEN 已过期，请先运行 login 刷新凭证", "token_expired")
	}
	geometry, err := LoadRoute(options.RoutePath)
	if err != nil {
		return failure(err.Error())
	}
	report(progress, fmt.Sprintf("[路径] %s: %.0f m", options.RoutePath, geometry.LengthM))

	client, clientErr := NewClient(auth, NativeSigner{}, nil)
	if clientErr != nil {
		return failure(clientErr.Error())
	}
	notes := []string{}
	claimed, abort := claimAndGenerate(ctx, client, auth, geometry, options, progress, &notes)
	if abort != nil {
		return *abort
	}
	if err := dumpPayload(claimed.record, *auth, options.OutputPayload, progress); err != nil {
		return failure(err.Error())
	}
	path, saveErr := SaveSession(filepath.Join(SessionDir,
		fmt.Sprintf("%d_%s.json", claimed.record.StartDateLong, claimed.sportID)),
		claimed.sportID, claimed.issuedAtMS, claimed.record, options)
	if saveErr != nil {
		return failure(saveErr.Error())
	}
	serverNow := time.Now().UnixMilli() + claimed.offsetMS
	remaining := float64(claimed.record.EndDateLong-serverNow) / 1000
	if remaining < 0 {
		remaining = 0
	}
	report(progress, "[会话] 已写入 "+path)
	report(progress, fmt.Sprintf("[会话] 记录 %s → %s，约 %.1f 分钟后可提交：",
		claimed.record.StartDate, claimed.record.EndDate, remaining/60))
	issued := claimed.issuedAtMS
	return FlowReport{
		Result:            UploadResult{RawResponse: "prepared"},
		Record:            &claimed.record,
		ServerSportID:     claimed.sportID,
		SportIDIssuedAtMS: &issued,
		SessionPath:       path,
		Notes:             notes,
	}
}

// SubmitSession waits for the saved record to finish and submits it.
func SubmitSession(ctx context.Context, configPath, sessionPath string, options SubmitOptions, progress Progress) FlowReport {
	path := sessionPath
	if path == "" {
		latest, err := LatestSession()
		if err != nil {
			return failure(err.Error())
		}
		if latest == "" {
			return failure("没有可提交的会话文件（" + SessionDir + " 为空），先运行 prepare")
		}
		path = latest
	}
	record, data, err := LoadSession(path)
	if err != nil {
		return failure("会话文件无法解析: " + err.Error())
	}
	auth, authErr := LoadAuthConfig(configPath)
	if authErr != nil {
		return failure(authErr.Error())
	}
	if absent := auth.MissingSessionFields(); len(absent) > 0 {
		return failure("登录凭证缺失或已过期，请先运行 login 刷新凭证", absent...)
	}
	if auth.IsTokenExpired() {
		return failure("TOKEN 已过期，请先运行 login 刷新凭证", "token_expired")
	}

	// Identity always follows the current login: the student may have logged in
	// again between prepare and submit.
	record.XH = auth.XH
	if auth.Name != "" {
		record.Name = auth.Name
	}
	sportID := data.SportID
	if sportID == "" {
		sportID = record.SportID
	}
	var issuedAt *int64
	if data.IssuedAtMS != nil {
		value := *data.IssuedAtMS
		issuedAt = &value
	}
	uploadOptions := &UploadOptions{
		RoutePath:       data.RoutePath,
		TargetDistanceM: data.TargetDistanceM,
		Seed:            data.Seed,
		SkipSensor:      options.SkipSensor,
		OutputPayload:   options.OutputPayload,
		SportType:       firstNonEmpty(data.SportType, DefaultSportType),
		RouteID:         firstNonEmpty(data.RouteID, "7"),
		RouteName:       firstNonEmpty(data.RouteName, DefaultRouteName),
	}

	client, clientErr := NewClient(auth, NativeSigner{}, nil)
	if clientErr != nil {
		return failure(clientErr.Error())
	}
	notes := []string{}
	waited := 0.0
	if probe := client.ProbeServerTime(ctx); probe.OK() {
		report(progress, "[服务器] 时间 "+FormatServerTime(probe.ServerTime))
		if options.NoWait && record.EndDateLong > probe.ServerTime {
			remaining := float64(record.EndDateLong-probe.ServerTime) / 1000
			return FlowReport{
				Result: UploadResult{RawResponse: fmt.Sprintf(
					"记录还没结束，还需 %.1f 分钟；去掉 --no-wait 可原地等待", remaining/60)},
				Record:            &record,
				ServerSportID:     sportID,
				SportIDIssuedAtMS: issuedAt,
				SessionPath:       path,
				Notes:             []string{"not_finished"},
			}
		}
	}
	if !options.NoWait {
		waited = waitForEnd(ctx, client, record, progress, 0)
	}
	if err := dumpPayload(record, *auth, uploadOptions.OutputPayload, progress); err != nil {
		return failure(err.Error())
	}
	flow := submitRecord(ctx, client, auth, record, uploadOptions, progress, &notes, sportID, issuedAt, waited)
	flow.SessionPath = path
	return flow
}

// SessionReport is the read-only self-check result.
type SessionReport struct {
	TokenPresent  bool   `json:"token_present"`
	TokenExpired  bool   `json:"token_expired"`
	SecretPresent bool   `json:"secret_present"`
	ServerTime    *int64 `json:"server_time"`
	InSportTime   *bool  `json:"in_sport_time"`
	OK            bool   `json:"ok"`
	Message       string `json:"message"`
	HTTPStatus    int    `json:"http_status"`
	APICode       string `json:"api_code"`
}

// CheckSession performs a read-only self-check: signer usable, server clock
// readable, token not expired.
func CheckSession(ctx context.Context, configPath string, progress Progress) SessionReport {
	out := SessionReport{}
	auth, err := LoadAuthConfig(configPath)
	if err != nil {
		out.Message = err.Error()
		return out
	}
	out.TokenPresent = auth.Token != ""
	out.SecretPresent = auth.Secret != ""
	out.TokenExpired = auth.IsTokenExpired()
	if !out.TokenPresent || !out.SecretPresent {
		out.Message = "缺少 TOKEN/SECRET"
		return out
	}
	client, clientErr := NewClient(auth, NativeSigner{}, nil)
	if clientErr != nil {
		out.Message = clientErr.Error()
		return out
	}
	probe := client.ProbeServerTime(ctx)
	if probe.OK() {
		value := probe.ServerTime
		out.ServerTime = &value
	}
	out.OK = probe.OK()
	out.Message = probe.Message
	out.HTTPStatus = probe.HTTPStatus
	out.APICode = probe.APICode
	report(progress, fmt.Sprintf("[自检] 服务端时间: %d ok=%t %s", probe.ServerTime, probe.OK(), probe.Message))
	if probe.OK() {
		window := client.RequestInSportTime(ctx, "7")
		out.InSportTime = window.Allowed
		switch {
		case window.Allowed == nil:
			report(progress, "[自检] 运动时段: 查询失败 ("+window.Message+")")
		case *window.Allowed:
			report(progress, "[自检] 运动时段: 允许")
		default:
			report(progress, "[自检] 运动时段: 不允许")
		}
	}
	return out
}

type claimResult struct {
	sportID    string
	issuedAtMS int64
	record     SportRecord
	offsetMS   int64
}

func generateOptionsFor(options *UploadOptions, auth *AuthConfig, startMS int64, sportID string) GenerateOptions {
	return GenerateOptions{
		StartMS:         startMS,
		SportID:         sportID,
		XH:              auth.XH,
		Name:            auth.Name,
		TargetDistanceM: options.TargetDistanceM,
		Seed:            options.Seed,
		SportType:       options.SportType,
		RouteID:         options.RouteID,
		RouteName:       options.RouteName,
	}
}

func runNetworkFlow(ctx context.Context, client *Client, auth *AuthConfig, geometry RouteGeometry, options *UploadOptions, progress Progress) FlowReport {
	notes := []string{}
	claimed, abort := claimAndGenerate(ctx, client, auth, geometry, options, progress, &notes)
	if abort != nil {
		return *abort
	}
	if err := dumpPayload(claimed.record, *auth, options.OutputPayload, progress); err != nil {
		return failure(err.Error())
	}

	waited := 0.0
	if options.Fast {
		// Comparison mode: move the whole timeline before the server clock and
		// submit immediately.
		latestEnd := time.Now().UnixMilli() + claimed.offsetMS - 2000
		if claimed.record.EndDateLong > latestEnd {
			if err := shiftRecord(&claimed.record, latestEnd-claimed.record.EndDateLong); err != nil {
				return failure(err.Error())
			}
		}
		report(progress, "[时间] fast 模式：记录时间线已整体前移，立即提交")
	} else {
		waited = waitForEnd(ctx, client, claimed.record, progress, options.WaitExtraSeconds)
	}

	shiftSeconds := float64(claimed.record.StartDateLong-claimed.issuedAtMS) / 1000
	direction := "晚"
	if shiftSeconds < 0 {
		direction = "早"
		shiftSeconds = -shiftSeconds
	}
	report(progress, fmt.Sprintf("[时间] 记录起点比 sportId 领取时刻%s %.1f 秒", direction, shiftSeconds))

	issued := claimed.issuedAtMS
	return submitRecord(ctx, client, auth, claimed.record, options, progress, &notes,
		claimed.sportID, &issued, waited)
}

// claimAndGenerate probes the server clock, asks for the exercise window, claims
// a sportId and generates the record after that claim.
func claimAndGenerate(ctx context.Context, client *Client, auth *AuthConfig, geometry RouteGeometry, options *UploadOptions, progress Progress, notes *[]string) (claimResult, *FlowReport) {
	probe := client.ProbeServerTime(ctx)
	if !probe.OK() {
		flow := failure("服务器时间检查失败: "+probe.Message, probe.Message)
		flow.Result.HTTPStatus = probe.HTTPStatus
		flow.Result.APICode = probe.APICode
		return claimResult{}, &flow
	}
	offsetMS := probe.ServerTime - time.Now().UnixMilli()
	report(progress, "[服务器] 时间 "+FormatServerTime(probe.ServerTime))

	window := client.RequestInSportTime(ctx, options.RouteID)
	if window.OK() {
		if *window.Allowed {
			report(progress, "[时段] 服务端判定: 在允许时段内")
		} else {
			report(progress, "[时段] 服务端判定: 不在允许时段")
			flow := failure("服务端判定当前不在该运动的允许时段，提交只会得到 inValidReason=4", window.Message)
			flow.Result.HTTPStatus = window.HTTPStatus
			flow.Result.APICode = window.APICode
			return claimResult{}, &flow
		}
	} else {
		*notes = append(*notes, "服务端时段查询失败，改用本地 6–23 点判断: "+window.Message)
	}

	report(progress, "[服务器] GET "+pathSportsID+" ...")
	sports := client.RequestSportsID(ctx)
	if !sports.OK() {
		flow := failure("申请 sportId 失败: "+sports.Message, sports.Message)
		flow.Result.HTTPStatus = sports.HTTPStatus
		flow.Result.APICode = sports.APICode
		return claimResult{}, &flow
	}
	sportID := sports.SportID
	issuedAtMS := time.Now().UnixMilli() + offsetMS
	report(progress, fmt.Sprintf("[服务器] sportId=%s（领取于 %s）", sportID, FormatServerTime(issuedAtMS)))

	// Probe once more so the record's start really is after the claim.
	anchorMS := probe.ServerTime
	if anchor := client.ProbeServerTime(ctx); anchor.OK() {
		anchorMS = anchor.ServerTime
	}

	record, err := GenerateRouteRecord(geometry, generateOptionsFor(options, auth, anchorMS+3000, sportID))
	if err != nil {
		flow := failure("生成记录失败: " + err.Error())
		return claimResult{}, &flow
	}
	report(progress, "[生成] "+RouteSummary(record))
	if !checkWindow(record, progress) {
		flow := failure("当前不在官方允许的运动时段内")
		flow.Record = &record
		flow.ServerSportID = sportID
		issued := issuedAtMS
		flow.SportIDIssuedAtMS = &issued
		return claimResult{}, &flow
	}
	return claimResult{sportID: sportID, issuedAtMS: issuedAtMS, record: record, offsetMS: offsetMS}, nil
}

func checkWindow(record SportRecord, progress Progress) bool {
	startHour := time.UnixMilli(record.StartDateLong).In(ServerLocation).Hour()
	endHour := time.UnixMilli(record.EndDateLong).In(ServerLocation).Hour()
	if startHour >= AllowedHourStart && startHour < AllowedHourEnd &&
		endHour >= AllowedHourStart && endHour < AllowedHourEnd {
		return true
	}
	report(progress, fmt.Sprintf("[时间] 记录 %02d:xx–%02d:xx，官方允许时段为 %02d:00–%02d:00（含结束时刻）",
		startHour, endHour, AllowedHourStart, AllowedHourEnd))
	return false
}

// shiftRecord moves a generated record's timeline by deltaMS.
func shiftRecord(record *SportRecord, deltaMS int64) error {
	record.StartDateLong += deltaMS
	record.EndDateLong += deltaMS
	for index := range record.Points {
		if record.Points[index].T > 0 {
			record.Points[index].T += deltaMS
		}
	}
	record.StartDate = FormatServerTime(record.StartDateLong)
	record.EndDate = FormatServerTime(record.EndDateLong)
	return AssertRecordConsistent(*record)
}

// waitForEnd blocks until the record's end time has passed on the server's
// clock.
func waitForEnd(ctx context.Context, client *Client, record SportRecord, progress Progress, extraSeconds float64) float64 {
	offsetMS := int64(0)
	if probe := client.ProbeServerTime(ctx); probe.OK() {
		offsetMS = probe.ServerTime - time.Now().UnixMilli()
	}
	targetLocalMS := record.EndDateLong - offsetMS + int64(extraSeconds*1000)
	started := time.Now()
	for {
		remaining := float64(targetLocalMS-time.Now().UnixMilli()) / 1000
		if remaining <= 0 {
			break
		}
		if remaining > 60 {
			report(progress, fmt.Sprintf("[等待] 记录还需 %.1f 分钟结束 ...", remaining/60))
			if err := sleepContext(ctx, min(60*time.Second, time.Duration(remaining-55)*time.Second)); err != nil {
				return time.Since(started).Seconds()
			}
			continue
		}
		if err := sleepContext(ctx, max(200*time.Millisecond, time.Duration(remaining*float64(time.Second)))); err != nil {
			return time.Since(started).Seconds()
		}
	}
	report(progress, "[等待] 记录时间已结束，开始提交")
	return time.Since(started).Seconds()
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// submitRecord validates, encrypts, pre-checks, submits and tops up the sensor
// data.
func submitRecord(ctx context.Context, client *Client, auth *AuthConfig, record SportRecord, options *UploadOptions, progress Progress, notes *[]string, sportID string, issuedAtMS *int64, waited float64) FlowReport {
	flow := FlowReport{
		Record:            &record,
		ServerSportID:     sportID,
		SportIDIssuedAtMS: issuedAtMS,
		WaitedSeconds:     waited,
		Notes:             *notes,
	}
	if err := AssertRecordConsistent(record); err != nil {
		flow.Result = UploadResult{RawResponse: "本地一致性校验失败: " + err.Error()}
		return flow
	}
	if confirm := client.ProbeServerTime(ctx); confirm.OK() {
		if err := AssertRecordNotInServerFuture(record, confirm.ServerTime, 120000); err != nil {
			flow.Result = UploadResult{RawResponse: "上传前时间校验失败: " + err.Error()}
			return flow
		}
	}

	aesKey, err := auth.AESKey()
	if err != nil {
		flow.Result = UploadResult{RawResponse: err.Error()}
		return flow
	}
	payloadJSON, err := BuildPayloadJSON(&record, *auth)
	if err != nil {
		flow.Result = UploadResult{RawResponse: err.Error()}
		return flow
	}
	encrypted, err := EncryptForUpload(payloadJSON, aesKey, auth.AESEncoding)
	if err != nil {
		flow.Result = UploadResult{RawResponse: err.Error()}
		return flow
	}
	report(progress, fmt.Sprintf("[构造] payload %d 字符，密文 %d 字符", len(payloadJSON), len(encrypted)))

	prejudgmentJSON, err := BuildPrejudgmentJSON(&record, *auth)
	if err != nil {
		flow.Result = UploadResult{RawResponse: err.Error()}
		return flow
	}
	encryptedPrejudgment, err := EncryptForUpload(prejudgmentJSON, aesKey, auth.AESEncoding)
	if err != nil {
		flow.Result = UploadResult{RawResponse: err.Error()}
		return flow
	}
	report(progress, "[预判] POST "+pathPrejudgment+" ...")
	prejudgment := client.RequestPrejudgment(ctx, encryptedPrejudgment)
	if !prejudgment.OK() {
		detail := prejudgment.Message
		if failures := prejudgment.Failures(); len(failures) > 0 {
			parts := make([]string, 0, len(failures))
			for _, check := range failures {
				parts = append(parts, check.Content)
			}
			detail = strings.Join(parts, "；")
		}
		flow.Result = UploadResult{
			RawResponse: "prejudgment 未通过: " + detail,
			HTTPStatus:  prejudgment.HTTPStatus,
			APICode:     prejudgment.APICode,
		}
		return flow
	}
	for _, check := range prejudgment.Checks {
		verdict := "失败"
		if check.IsValid == "1" {
			verdict = "通过"
		}
		report(progress, fmt.Sprintf("[预判] %s: %s", verdict, check.Content))
	}
	flow.PrejudgmentOK = true

	report(progress, "[上传] POST "+pathSaveSports+" ...")
	result := client.UploadSavesports(ctx, encrypted)
	if result.SportID == "" {
		result.SportID = sportID
	}
	report(progress, fmt.Sprintf("[上传] isValid=%s reason='%s' checkStatus=%s tip='%s'",
		result.IsValid, result.InValidReason, result.CheckStatus, result.Tip))

	if result.Success && result.IsValid == "1" && !options.SkipSensor {
		sensorText := MakeSensorTrace(SensorTraceOptions{DurationSeconds: record.DurationSeconds(), Seed: options.Seed})
		filename, zipData, sensorErr := PrepareSensorZipUpload(sensorText, result.SportID)
		if sensorErr != nil {
			flow.Notes = append(flow.Notes, "传感器数据生成失败: "+sensorErr.Error())
		} else {
			report(progress, "[传感器] POST "+pathSensorZip+" ...")
			result.SensorZipOK = client.UploadSensorZip(ctx, result.SportID, zipData, filename)
			state := "FAIL"
			if result.SensorZipOK {
				state = "OK"
			}
			report(progress, "[传感器] "+state)
			if !result.SensorZipOK {
				flow.Notes = append(flow.Notes, "savesports 成功但 sensorZipFile 上传失败")
			}
		}
	}
	flow.Result = result
	flow.ServerSportID = result.SportID
	return flow
}

func dumpPayload(record SportRecord, auth AuthConfig, path string, progress Progress) error {
	if path == "" {
		return nil
	}
	payload, err := BuildPayloadJSON(&record, auth)
	if err != nil {
		return err
	}
	// json.Indent preserves the payload's field order, which unmarshalling into a
	// map would not.
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(payload), "", "  "); err != nil {
		return err
	}
	if err := os.WriteFile(path, indented.Bytes(), 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	report(progress, "[输出] 明文 payload 已写入 "+path)
	return nil
}

// SessionFile is the persisted prepare/submit handover.
type SessionFile struct {
	Version         int         `json:"version"`
	SportID         string      `json:"sport_id"`
	IssuedAtMS      *int64      `json:"issued_at_ms"`
	CreatedAt       string      `json:"created_at"`
	RoutePath       string      `json:"route_path"`
	TargetDistanceM float64     `json:"target_distance_m"`
	Seed            *int64      `json:"seed"`
	SportType       string      `json:"sport_type"`
	RouteID         string      `json:"route_id"`
	RouteName       string      `json:"route_name"`
	Record          SportRecord `json:"record"`
}

// SaveSession persists the claimed sportId and the generated record.
func SaveSession(path, sportID string, issuedAtMS int64, record SportRecord, options *UploadOptions) (string, error) {
	issued := issuedAtMS
	session := SessionFile{
		Version:         SessionVersion,
		SportID:         sportID,
		IssuedAtMS:      &issued,
		CreatedAt:       FormatServerTime(time.Now().UnixMilli()),
		RoutePath:       options.RoutePath,
		TargetDistanceM: options.TargetDistanceM,
		Seed:            options.Seed,
		SportType:       options.SportType,
		RouteID:         options.RouteID,
		RouteName:       options.RouteName,
		Record:          record,
	}
	encoded, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return "", fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return path, nil
}

// LoadSession reads a saved session and returns the record plus the metadata.
func LoadSession(path string) (SportRecord, SessionFile, error) {
	var session SessionFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return SportRecord{}, session, err
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return SportRecord{}, session, err
	}
	if session.Version != SessionVersion {
		return SportRecord{}, session, fmt.Errorf("会话文件版本不匹配: %d != %d", session.Version, SessionVersion)
	}
	record := session.Record
	if record.SportID == "" {
		record.SportID = session.SportID
	}
	if record.Points == nil {
		record.Points = []TrackPoint{}
	}
	if record.Pace == nil {
		record.Pace = []PaceEntry{}
	}
	if record.MinuteSpeed == nil {
		record.MinuteSpeed = []MinuteSpeedEntry{}
	}
	return record, session, nil
}

// LatestSession returns the most recently written session file.
func LatestSession() (string, error) {
	entries, err := os.ReadDir(SessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var files []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		files = append(files, candidate{path: filepath.Join(SessionDir, entry.Name()), modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	if len(files) == 0 {
		return "", nil
	}
	return files[0].path, nil
}
