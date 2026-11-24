// Command hustrun is the desktop client for the campus running upload chain:
// CAS login, record generation, prepare/submit and offline payload dumps.
//
// Usage:
//
//	hustrun login    [-c config.yaml] [-u 学号] [-p 密码] [--debug]
//	hustrun check    [-c config.yaml]
//	hustrun generate -r route.json [-d 2150] [--seed N] [--out payload.json]
//	hustrun upload   -r route.json [-d 2150] [--fast] [--skip-sensor] [--out payload.json]
//	hustrun prepare  -r route.json [-d 2150] [--out payload.json]
//	hustrun submit   [--session file.json] [--skip-sensor] [--no-wait]
//	hustrun routes   [目录]
//	hustrun mutate   -i sport.json [-o out.json] [--date today|YYYY-MM-DD] [--seed N]
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"hust-sports-core/hustrun"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	command, rest := args[0], args[1:]
	switch command {
	case "login":
		return runLogin(ctx, rest)
	case "check":
		return runCheck(ctx, rest)
	case "generate":
		return runGenerate(rest)
	case "upload":
		return runUpload(ctx, rest)
	case "prepare":
		return runPrepare(ctx, rest)
	case "submit":
		return runSubmit(ctx, rest)
	case "routes":
		return runRoutes(rest)
	case "route-from-geojson":
		return runRouteFromGeoJSON(rest)
	case "multiroute":
		return runMultiRoute(rest)
	case "mutate":
		return runMutate(rest)
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", command)
		usage()
		return 1
	}
}

func usage() {
	fmt.Print(`hustrun — 校园跑上传客户端

用法:
  hustrun login    [-c config.yaml] [-u 学号] [-p 密码] [--debug]
  hustrun check    [-c config.yaml]
  hustrun generate -r route.json [-d 2150] [--seed N] [--out payload.json]
  hustrun upload   -r route.json [-d 2150] [--fast] [--skip-sensor] [--out payload.json]
  hustrun prepare  -r route.json [-d 2150] [--out payload.json]
  hustrun submit   [--session file.json] [--skip-sensor] [--no-wait]
  hustrun routes   [目录]
  hustrun mutate   -i sport.json [-o out.json] [--date today|YYYY-MM-DD] [--seed N]

流程:
  login  → 换取 TOKEN/SECRET 写入 HEADERS.xml
  upload → 申请 sportId、按路线生成记录、等记录时间走完、提交、补传传感器
  prepare/submit → 把「领取 + 生成」和「提交」拆成两步，中间可以关掉终端

退出码: 0 服务端判定有效 · 2 服务端判定无效 · 1 请求或配置失败
`)
}

const progressPrefix = ""

func progress(message string) {
	fmt.Println(progressPrefix + message)
}

// flags shared by the record commands.
type recordFlags struct {
	config     string
	route      string
	distance   float64
	seed       int64
	hasSeed    bool
	fast       bool
	skipSensor bool
	out        string
	waitExtra  float64
	sportType  string
	routeID    string
	routeName  string
}

func registerRecordFlags(set *flag.FlagSet, values *recordFlags) {
	set.StringVar(&values.config, "c", "config.yaml", "配置文件")
	set.StringVar(&values.route, "r", "", "路线 JSON（[[lat, lon], ...]）")
	set.Float64Var(&values.distance, "d", 2150, "目标距离（米）")
	set.Func("seed", "轨迹随机种子（默认随机）", func(value string) error {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		values.seed = parsed
		values.hasSeed = true
		return nil
	})
	set.BoolVar(&values.fast, "fast", false, "不等记录结束，立即提交（对照实验）")
	set.BoolVar(&values.skipSensor, "skip-sensor", false, "不上传传感器数据")
	set.StringVar(&values.out, "out", "", "把明文 payload 写到文件")
	set.Float64Var(&values.waitExtra, "wait-extra", 0, "记录结束后额外等待的秒数")
	set.StringVar(&values.sportType, "sport-type", "", "运动模式（默认 3）")
	set.StringVar(&values.routeID, "route-id", "", "路线 ID（默认 7）")
	set.StringVar(&values.routeName, "route-name", "", "路线名称（默认 跑步-男生）")
}

func (values *recordFlags) options(dryRun bool) *hustrun.UploadOptions {
	options := &hustrun.UploadOptions{
		RoutePath:        values.route,
		TargetDistanceM:  values.distance,
		Fast:             values.fast,
		SkipSensor:       values.skipSensor,
		DryRun:           dryRun,
		OutputPayload:    values.out,
		WaitExtraSeconds: values.waitExtra,
		SportType:        values.sportType,
		RouteID:          values.routeID,
		RouteName:        values.routeName,
	}
	if values.hasSeed {
		seed := values.seed
		options.Seed = &seed
	}
	return options
}

// parseArgsWithPositionals parses a command that accepts both flags and bare
// positional arguments in any order. The standard flag package stops at the
// first positional, so flags are collected first and handed over together.
func parseArgsWithPositionals(set *flag.FlagSet, args []string) ([]string, bool) {
	var flags, positional []string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positional = append(positional, argument)
			continue
		}
		flags = append(flags, argument)
		if strings.Contains(argument, "=") {
			continue
		}
		if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
			flags = append(flags, args[index+1])
			index++
		}
	}
	set.SetOutput(os.Stderr)
	if err := set.Parse(append(flags, positional...)); err != nil {
		return nil, false
	}
	return positional, true
}

func parseFlags(set *flag.FlagSet, args []string) bool {
	set.SetOutput(os.Stderr)
	if err := set.Parse(args); err != nil {
		return false
	}
	return true
}

func requireRoute(values *recordFlags) bool {
	if strings.TrimSpace(values.route) == "" {
		fmt.Fprintln(os.Stderr, "✗ 需要 -r 指定路线文件")
		return false
	}
	return true
}

func emitResult(result hustrun.UploadResult) int {
	switch {
	case result.Success && result.IsValid == "1":
		fmt.Printf("✓ 提交成功 sportId=%s checkStatus=%s tip=%s\n",
			result.SportID, result.CheckStatus, result.Tip)
		return 0
	case result.Success:
		fmt.Printf("✗ 服务端判定无效 isValid=%s reason=%s tip=%s\n",
			result.IsValid, result.InValidReason, result.Tip)
		return 2
	default:
		fmt.Fprintf(os.Stderr, "✗ 上传失败: %s\n", result.RawResponse)
		return 1
	}
}

func runGenerate(args []string) int {
	values := &recordFlags{}
	set := flag.NewFlagSet("generate", flag.ContinueOnError)
	registerRecordFlags(set, values)
	if !parseFlags(set, args) {
		return 1
	}
	if !requireRoute(values) {
		return 1
	}
	report := hustrun.UploadRoute(context.Background(), values.config, values.options(true), progress)
	if report.Record == nil {
		fmt.Fprintf(os.Stderr, "✗ %s\n", report.Result.RawResponse)
		return 1
	}
	fmt.Println("✓ 生成完成（dry-run，未访问网络）")
	return 0
}

func runUpload(ctx context.Context, args []string) int {
	values := &recordFlags{}
	set := flag.NewFlagSet("upload", flag.ContinueOnError)
	registerRecordFlags(set, values)
	if !parseFlags(set, args) {
		return 1
	}
	if !requireRoute(values) {
		return 1
	}
	report := hustrun.UploadRoute(ctx, values.config, values.options(false), progress)
	if report.Record != nil {
		fmt.Printf("[记录] %s → %s %s km\n",
			report.Record.StartDate, report.Record.EndDate, report.Record.DLLC)
	}
	for _, note := range report.Notes {
		fmt.Println("[提示]", note)
	}
	return emitResult(report.Result)
}

func runPrepare(ctx context.Context, args []string) int {
	values := &recordFlags{}
	set := flag.NewFlagSet("prepare", flag.ContinueOnError)
	registerRecordFlags(set, values)
	if !parseFlags(set, args) {
		return 1
	}
	if !requireRoute(values) {
		return 1
	}
	report := hustrun.PrepareRoute(ctx, values.config, values.options(false), progress)
	if report.SessionPath == "" {
		fmt.Fprintf(os.Stderr, "✗ 准备失败: %s\n", report.Result.RawResponse)
		return 1
	}
	fmt.Printf("✓ 已准备 session=%s\n", report.SessionPath)
	return 0
}

func runSubmit(ctx context.Context, args []string) int {
	set := flag.NewFlagSet("submit", flag.ContinueOnError)
	configPath := set.String("c", "config.yaml", "配置文件")
	sessionPath := set.String("session", "", "要提交的会话文件（默认取最新）")
	skipSensor := set.Bool("skip-sensor", false, "不上传传感器数据")
	noWait := set.Bool("no-wait", false, "记录还没结束就直接报错退出")
	out := set.String("out", "", "把明文 payload 写到文件")
	if !parseFlags(set, args) {
		return 1
	}
	report := hustrun.SubmitSession(ctx, *configPath, *sessionPath, hustrun.SubmitOptions{
		SkipSensor:    *skipSensor,
		NoWait:        *noWait,
		OutputPayload: *out,
	}, progress)
	if report.Record != nil {
		fmt.Printf("[记录] %s → %s %s km\n",
			report.Record.StartDate, report.Record.EndDate, report.Record.DLLC)
	}
	for _, note := range report.Notes {
		fmt.Println("[提示]", note)
	}
	return emitResult(report.Result)
}

func runCheck(ctx context.Context, args []string) int {
	set := flag.NewFlagSet("check", flag.ContinueOnError)
	configPath := set.String("c", "config.yaml", "配置文件")
	if !parseFlags(set, args) {
		return 1
	}
	auth, err := hustrun.LoadAuthConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 配置读取失败: %v\n", err)
		return 1
	}
	fmt.Printf("[配置] 学号: %s  服务端: %s\n", auth.XH, auth.ServerURL)
	fmt.Printf("[配置] TOKEN %s (%d 字符), SECRET %s, 过期=%t\n",
		present(auth.Token != ""), len(auth.Token), present(auth.Secret != ""), auth.IsTokenExpired())

	report := hustrun.CheckSession(ctx, *configPath, progress)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		fmt.Println(string(encoded))
	}
	if !report.OK {
		return 1
	}
	return 0
}

func present(ok bool) string {
	if ok {
		return "已配置"
	}
	return "缺失"
}

func runRoutes(args []string) int {
	set := flag.NewFlagSet("routes", flag.ContinueOnError)
	directory := set.String("dir", "routes", "路线目录")
	positional, ok := parseArgsWithPositionals(set, args)
	if !ok {
		return 1
	}
	if len(positional) > 0 {
		*directory = positional[0]
	}
	entries, err := os.ReadDir(*directory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 读取目录失败: %v\n", err)
		return 1
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		geometry, err := hustrun.LoadRoute(filepath.Join(*directory, name))
		if err != nil {
			fmt.Printf("%-24s 读取失败: %v\n", name, err)
			continue
		}
		fmt.Printf("%-24s %8.0f m\n", name, geometry.LengthM)
	}
	return 0
}

func runRouteFromGeoJSON(args []string) int {
	set := flag.NewFlagSet("route-from-geojson", flag.ContinueOnError)
	spacing := set.Float64("spacing", 1.0, "重采样间距（米）")
	positional, ok := parseArgsWithPositionals(set, args)
	if !ok {
		return 1
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "✗ 需要 GeoJSON 文件")
		return 1
	}
	source := positional[0]
	target := "route_drawn.json"
	if len(positional) > 1 {
		target = positional[1]
	}
	file, err := os.Open(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 打开失败: %v\n", err)
		return 1
	}
	defer file.Close()
	points, err := hustrun.RouteFromGeoJSON(file, *spacing)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 转换失败: %v\n", err)
		return 1
	}
	if err := hustrun.WriteRouteFile(target, points); err != nil {
		fmt.Fprintf(os.Stderr, "✗ 写入失败: %v\n", err)
		return 1
	}
	fmt.Printf("✓ %d 个点，%.1f m → %s\n", len(points), hustrun.RouteLengthM(points), target)
	return 0
}

func runMultiRoute(args []string) int {
	set := flag.NewFlagSet("multiroute", flag.ContinueOnError)
	distance := set.Float64("distance", 2000, "目标距离（米）")
	noise := set.Float64("noise", 0.45, "每点横向 GPS 抖动（米）")
	drift := set.Float64("drift", 1.6, "每圈横向漂移幅度（米）")
	seed := set.Int64("seed", 20261024, "随机种子")
	positional, ok := parseArgsWithPositionals(set, args)
	if !ok {
		return 1
	}
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "✗ 需要 单圈路线 与 输出文件")
		return 1
	}
	lap, err := hustrun.LoadRoutePoints(positional[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 读取失败: %v\n", err)
		return 1
	}
	points, err := hustrun.BuildMultiLapRoute(lap, hustrun.MultiLapOptions{
		DistanceM: *distance,
		NoiseM:    *noise,
		DriftM:    *drift,
		Seed:      seed,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 生成失败: %v\n", err)
		return 1
	}
	if err := hustrun.WriteRouteFile(positional[1], points); err != nil {
		fmt.Fprintf(os.Stderr, "✗ 写入失败: %v\n", err)
		return 1
	}
	fmt.Printf("✓ %d 个点，%.1f m → %s\n", len(points), hustrun.RouteLengthM(points), positional[1])
	return 0
}

func runMutate(args []string) int {
	set := flag.NewFlagSet("mutate", flag.ContinueOnError)
	input := set.String("i", "", "数据库导出的 sport_*.json")
	output := set.String("o", "", "输出文件（默认覆盖输入）")
	date := set.String("date", "today", "目标日期 today | YYYY-MM-DD | original")
	jitter := set.Float64("gps-jitter", 0, "整体坐标抖动幅度（度）")
	deformation := set.Float64("deform", 12, "轨迹形变幅度（米）")
	reference := set.Int64("reference-time", 0, "参考时刻（毫秒，默认本机时间）")
	seed := set.Int64("seed", 0, "随机种子（0 表示不使用固定种子）")
	if !parseFlags(set, args) {
		return 1
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, "✗ 需要 -i 指定导出的 JSON")
		return 1
	}
	record, err := hustrun.LoadSportJSON(*input, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 读取记录失败: %v\n", err)
		return 1
	}
	options := hustrun.DefaultMutateOptions()
	options.TargetDate = *date
	options.GPSJitterScale = *jitter
	options.TrackDeformationM = *deformation
	if *seed != 0 {
		options.Seed = seed
	}
	if *reference > 0 {
		options.ReferenceTimeMS = reference
	}
	mutated, err := hustrun.MutateRecord(record, options)
	if err != nil {
		var consistency *hustrun.RecordConsistencyError
		if errors.As(err, &consistency) {
			for _, issue := range consistency.Issues {
				fmt.Fprintf(os.Stderr, "✗ %s: %s\n", issue.Code, issue.Message)
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "✗ 变异失败: %v\n", err)
		return 1
	}
	payload, err := hustrun.BuildPayloadJSON(&mutated, hustrun.AuthConfig{
		Name:         mutated.Name,
		XH:           mutated.XH,
		PhoneVersion: hustrun.DefaultPhoneVersion,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 构造 payload 失败: %v\n", err)
		return 1
	}
	target := *output
	if target == "" {
		target = *input
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(payload), "", "  "); err == nil {
		payload = indented.String()
	}
	if err := os.WriteFile(target, []byte(payload), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "✗ 写入失败: %v\n", err)
		return 1
	}
	fmt.Printf("✓ 变异完成 %s → %s（%s km，%s）\n", mutated.SportID, target, mutated.DLLC, mutated.DLTime)
	return 0
}

func runLogin(ctx context.Context, args []string) int {
	set := flag.NewFlagSet("login", flag.ContinueOnError)
	configPath := set.String("c", "config.yaml", "配置文件")
	username := set.String("u", "", "学号（默认读 USERINFO.xml）")
	password := set.String("p", "", "密码（建议省略，交互输入）")
	debug := set.Bool("debug", false, "显示 CAS 重定向链")
	if !parseFlags(set, args) {
		return 1
	}
	auth, err := hustrun.LoadAuthConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 配置读取失败: %v\n", err)
		return 1
	}
	headerPath := credentialPath(*configPath, "header_xml")
	userInfoPath := credentialPath(*configPath, "userinfo_xml")
	hadCredentials := fileExists(headerPath)
	if err := hustrun.EnsureSPXML(headerPath); err != nil {
		fmt.Fprintf(os.Stderr, "✗ 无法创建凭证文件: %v\n", err)
		return 1
	}

	account := *username
	if account == "" {
		account = auth.XH
	}
	if account == "" {
		account = prompt("用户名/学号")
	}
	secret := *password
	if secret == "" {
		secret = prompt("密码（输入会回显）")
	}
	if account == "" || secret == "" {
		fmt.Fprintln(os.Stderr, "✗ 用户名和密码不能为空")
		return 1
	}

	fmt.Printf("[登录] 服务端: %s\n", auth.ServerURL)
	result, err := hustrun.Login(ctx, hustrun.LoginOptions{
		ServerURL: auth.ServerURL,
		Username:  account,
		Password:  secret,
		Captcha:   promptCaptcha,
		DynamicCode: func(message string) (string, error) {
			fmt.Printf("\n[登录] %s\n", message)
			return prompt("企业微信动态验证码"), nil
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ CAS 登录失败: %v\n", err)
		return 1
	}
	if !result.Success {
		fmt.Fprintf(os.Stderr, "✗ CAS 登录失败: %s\n", result.Error)
		if strings.Contains(result.Error, "403") {
			fmt.Fprintln(os.Stderr, "  说明: 学校 WAF 拦截了本次请求（常见于短时间重复登录）。"+
				"请等待 10–30 分钟，或换一个网络后重试；不要连续重试，否则封禁时间会延长。")
		}
		if *debug {
			for _, line := range result.Trace {
				fmt.Fprintln(os.Stderr, "  ", line)
			}
		}
		return 1
	}
	fmt.Println("✓ 登录成功（TOKEN/SECRET 不在终端显示）")
	if err := hustrun.SaveHeaderXML(headerPath, result, hadCredentials); err != nil {
		fmt.Fprintf(os.Stderr, "✗ 写入凭证失败: %v\n", err)
		return 1
	}
	fmt.Printf("✓ 凭证已更新: %s\n", headerPath)

	name := result.Name
	if name == "" {
		name = prompt("姓名（显示在运动记录里，可留空）")
	}
	if err := hustrun.WriteUserInfo(userInfoPath, firstNonEmpty(result.XH, account), name); err != nil {
		fmt.Fprintf(os.Stderr, "✗ 写入用户信息失败: %v\n", err)
		return 1
	}
	fmt.Printf("✓ 用户信息已更新: %s\n", userInfoPath)
	return 0
}

func credentialPath(configPath, key string) string {
	config, err := hustrun.LoadYAMLConfig(configPath)
	if err != nil {
		return filepath.Join(filepath.Dir(configPath), hustrun.DefaultHeaderXML)
	}
	switch key {
	case "header_xml":
		return config.Path("paths", "header_xml", hustrun.DefaultHeaderXML)
	case "userinfo_xml":
		return config.Path("paths", "userinfo_xml", hustrun.DefaultUserInfoXML)
	default:
		return config.Path("paths", key, key+".xml")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var stdin = bufio.NewReader(os.Stdin)

func prompt(label string) string {
	fmt.Printf("%s: ", label)
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimSpace(line)
}

func promptCaptcha(image []byte, suffix string) (string, error) {
	file, err := os.CreateTemp("", "hustrun_captcha_*"+suffix)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(image); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	defer os.Remove(path)
	fmt.Printf("\n[登录] 图形验证码: %s\n", path)
	return prompt("验证码"), nil
}
