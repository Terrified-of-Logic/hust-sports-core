# HUST Sports Core

用于跑步轨迹导入、里程计算、操场位置核验，以及校园跑上传提交的 Go 实现。使用 Go 1.22 或更新版本，仅依赖标准库。库通过 Go 包公开 API：`sports` 负责轨迹本身，`hustrun` 负责跑后记录的上传链路；两者都能离线运行，只有显式调用网络函数或流水线时才会发请求。

```
.
├── *.go                     package sports：轨迹读写、里程、偏差评估、高德坐标转换
├── hustrun/                 package hustrun：上传提交系统（登录/签名/生成/提交/传感器）
│   ├── routes/              路线样例（[[lat, lon], ...]）
│   ├── testdata/            测试夹具
│   └── config.example.yaml  配置示例
└── cmd/hustrun/             命令行入口
```

## 本地接入

在本仓库运行 `go test ./...` 检查实现。在另一个已有 `go.mod` 的本地项目中，把下方 `/path/to/hust-sports-core` 换成此仓库的绝对路径。

```sh
go mod edit -require=hust-sports-core@v0.0.0
go mod edit "-replace=hust-sports-core=/path/to/hust-sports-core"
```

`package sports` 的导入路径为 `hust-sports-core`，包名为 `sports`；上传链路的导入路径为 `hust-sports-core/hustrun`，包名为 `hustrun`。下面的程序接收设备导出的 `run.gpx` 和以 WGS84 保存的操场跑道参考线 `stadium.geojson`。示例使用 10 米作为显示容差，调用方可根据设备和参考线精度调整。

```go
package main

import (
	"fmt"
	"log"
	"os"

	"hust-sports-core"
)

func main() {
	if err := compare(); err != nil {
		log.Fatal(err)
	}
}

func compare() error {
	gpx, err := os.Open("run.gpx")
	if err != nil {
		return err
	}
	defer gpx.Close()
	observed, err := sports.ReadGPX(gpx)
	if err != nil {
		return err
	}

	geoJSON, err := os.Open("stadium.geojson")
	if err != nil {
		return err
	}
	defer geoJSON.Close()
	reference, err := sports.ReadGeoJSON(geoJSON, sports.WGS84)
	if err != nil {
		return err
	}

	length, err := observed.LengthMeters()
	if err != nil {
		return err
	}
	result, err := sports.AssessAlignment(observed, reference, 10)
	if err != nil {
		return err
	}
	fmt.Printf("里程 %.1f 米，平均偏差 %.1f 米，最大偏差 %.1f 米，容差内 %d/%d 个采样点\n",
		length, result.MeanOffsetMeters, result.MaxOffsetMeters, result.WithinTolerance, result.PointCount)
	return nil
}
```

## 操场位置核验

GPX 和标准 GeoJSON 使用 WGS84。高德底图使用 GCJ-02。将 GPX 坐标直接叠在高德底图会产生位置偏移。GeoJSON 按 `[经度, 纬度]` 读写，GPX 则分别读取 `lon` 和 `lat` 属性。参考项目的 GeoJSON 导入脚本交换数组顺序，坐标系仍为输入数据的坐标系。 [GPX 规范](https://www.topografix.com/GPX/1/1/) · [GeoJSON 规范](https://www.rfc-editor.org/rfc/rfc7946.html#section-4) · [高德坐标系说明](https://lbs.amap.com/api/javascript-api-v2/guide/abc/basetype) · [参考项目导入脚本](https://github.com/flyingpig233-cpu/hust-nailong-sports/blob/main/scripts/route_from_geojson.py)

| 数据和底图 | 处理方式 |
| --- | --- |
| GPX 与 WGS84 参考线 | 直接调用 `AssessAlignment`。 |
| GPX 叠在高德底图 | 调用 `ConvertTrack`，使用返回值中每段的 `Point.Lon`、`Point.Lat` 绘制地图折线。 |
| GPX 与明确使用 GCJ-02 的参考线 | 用 `ReadGeoJSON(r, sports.GCJ02)` 导入参考线，再将 GPX 的显示副本与参考线传给 `AssessAlignment`。 |

需要高德地图显示时，可使用下面的函数。它从环境变量读取高德 Web 服务 API Key。调用会向高德发送经纬度点，时间戳留在本地。默认 HTTP 超时为每批 10 秒，可以用上下文设置整个操作的期限。 [高德 Web 服务坐标转换文档](https://lbs.amap.com/api/webservice/guide/api/convert)

```go
// 此函数使用 context、os、time 和 hust-sports-core。
func forAMap(ctx context.Context, observed sports.Track) (sports.Track, error) {
	converter, err := sports.NewAMapConverter(os.Getenv("AMAP_WEB_SERVICE_KEY"), nil)
	if err != nil {
		return sports.Track{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return converter.ConvertTrack(ctx, observed)
}
```

用实测或可靠地图数据提供操场参考线。若为闭合跑道，将起点作为同一段的最后一个点再次写入。位置核验以采样点到参考线的最近距离为依据。统一坐标后仍存在偏差时，可以根据实际跑道、参考线和 GPS 采样精度判断。评估返回的是几何偏差，保留实测轨迹数据。

## 轨迹 API

| API | 输入与行为 |
| --- | --- |
| `CRS`、`WGS84`、`GCJ02` | 表示轨迹的坐标系。调用方手动构造 `Track` 时需要声明 `CRS`。 |
| `Track`、`Point` | `Track.Segments` 保留分段顺序和组内点序。`Point.Lon`、`Point.Lat` 单位为度，`Point.Time` 的零值表示没有时间戳。 |
| `Track.Validate()` | 检查坐标系、分段内容和经纬度有效范围。含单点的分段有效。 |
| `ReadGPX(r)` | 读取 GPX 1.0、1.1 或省略命名空间的 GPX 轨迹。保留非空 `trkseg` 及 `trkpt` 的经纬度和带时区的 RFC 3339 时间戳。忽略航点、规划路线、海拔及扩展字段。 |
| `ReadGeoJSON(r, crs)` | 读取 `LineString`、`MultiLineString` 及 `Feature`、`FeatureCollection` 直接包含的两类线几何。每条线需至少有两个位置，位置可以带第三维海拔。导入保留二维坐标，忽略第三维、属性及 `GeometryCollection`。输入的实际坐标系由调用方确认。 |
| `WriteGeoJSON(w, track)` | 输出 WGS84 `LineString` 或 `MultiLineString`，每段至少两个点。输出为二维坐标，适用于普通跑步路线。跨 180° 经线的路线可能需要调用方在导出前进行分段。 |
| `Track.LengthMeters()` | 按平均地球半径计算球面近似里程，分段间隙不计入。实际里程应使用 WGS84 轨迹计算。 |
| `AssessAlignment(observed, reference, toleranceMeters)` | 要求双方坐标系一致、容差有限且非负，并且参考线至少有一条由两个不同点构成的边。逐采样点比较球面最近距离。GCJ-02 下的结果用于地图显示核验，米数为近似值。 |
| `Alignment` | `PointCount` 为采样点数，`WithinTolerance` 为最近距离不超过容差的点数，`MeanOffsetMeters` 和 `MaxOffsetMeters` 分别是偏差的平均值和最大值。点数按采样计，不评估完成的圈数。 |
| `NewAMapConverter(key, client)` | 接收高德 Web 服务 Key 和可选 `*http.Client`。默认客户端每批超时 10 秒，禁用重定向。 |
| `AMapConverter.ConvertTrack(ctx, track)` | 返回独立的 GCJ-02 副本。WGS84 点每批最多 40 个，已有 GCJ-02 输入直接复制。保留时间戳和分段，失败始终返回空结果。 |

读写函数接受标准库的 `io.Reader` 和 `io.Writer`，由调用方负责关闭。轨迹点保存在内存中。偏差评估逐点遍历参考线的边，适用于操场等局部路线。

可用 `errors.Is` 区分 `sports.ErrCRS`（坐标系缺失、未知或不匹配）、`sports.ErrInvalidTrack`（轨迹结构、格式、坐标或容差无效）和 `sports.ErrAMap`（高德转换失败）。上下文取消和超时使用标准 `context` 错误，写入失败返回底层写入错误。用 `go doc -all` 查看导出 API 注释。

---

# 上传提交系统（`hustrun`）

`hustrun` 是一条纯协议的上传链路，对齐 App 1.0.9 的抓包结论，不需要手机、模拟器或改包客户端：

```
CAS 登录 → 领取 sportId → 按路线生成记录 → 等记录时间真正走完 → 预判 → 本地签名并提交 → 补传传感器
```

官方时间线（抓包确认）：

| 阶段 | 请求 |
| --- | --- |
| 运动开始前 | `GET /api/l/v7/sportsId`（**服务端**分配 sportId） |
| 运动中 | 客户端逐秒记录轨迹 |
| 点“结束” | `POST /api/l/v6.1/prejudgment` |
| 倒计时内 | `POST /api/l/v7/savesports` |
| 随后 | `POST /api/l/v7/upload/sensorZipFile`（或 `sensorFile`） |

`sportId` 由服务端在运动开始前下发，所以默认流程先申请，再把记录起点设为申请成功之后，并等到记录的结束时间真正过去才提交。`fast` 只用于对照实验：它把整条时间线前移到服务端时钟之前并立刻提交。

## 命令行

```sh
cp hustrun/config.example.yaml config.yaml     # 按需改路径与设备指纹
go run ./cmd/hustrun login                      # 学号 + 密码 + 验证码/动态码
go run ./cmd/hustrun check                      # 只读自检：会话 / 服务端时间 / 时段
go run ./cmd/hustrun routes                     # 列出路线长度

# 正式提交（原地等记录时间走完，2.15 km ≈ 8 分钟）
go run ./cmd/hustrun upload -r hustrun/routes/route_track.json -d 2150

# 拆成两段，等待期间可以关掉终端
go run ./cmd/hustrun prepare -r hustrun/routes/route_track.json -d 2150
go run ./cmd/hustrun submit                     # 默认取最新 session

# 只看生成的数据（不联网、不签名）
go run ./cmd/hustrun generate -r hustrun/routes/route_track.json -d 2150 --out payload.json

# 路线工具：手绘 GeoJSON → 路线文件；单圈 → 多圈
go run ./cmd/hustrun route-from-geojson drawn.geojson route_drawn.json --spacing 1
go run ./cmd/hustrun multiroute hustrun/routes/route_track.json route_3k.json --distance 3000
```

子命令一览：`login`、`check`、`generate`、`upload`、`prepare`、`submit`、`routes`、`route-from-geojson`、`multiroute`、`mutate`。位置参数与 `--flag` 可以任意顺序混写。

退出码：`0` 服务端判定有效，`2` 服务端判定无效（附 `reason`），`1` 请求或配置失败。

## 库 API

```go
import "hust-sports-core/hustrun"

auth, err := hustrun.LoadAuthConfig("config.yaml")
client, err := hustrun.NewClient(auth, hustrun.NativeSigner{}, nil)

report := hustrun.UploadRoute(ctx, "config.yaml", &hustrun.UploadOptions{
    RoutePath:       "hustrun/routes/route_track.json",
    TargetDistanceM: 2150,
}, func(line string) { fmt.Println(line) })
```

| API | 输入与行为 |
| --- | --- |
| `LoadAuthConfig(path)` | 读 `config.yaml` 的映射子集与三个 SharedPreferences XML（`HEADER/USERINFO/SCHOOL`），路径以配置文件所在目录为基准。 |
| `AuthConfig` | `Token`、`Secret`、`ServerURL`、设备指纹与上传参数；`AESKey()` 取 `SECRET` 前 16 字节，`IsTokenExpired()` 解析 JWT 的 `exp`。 |
| `Login(ctx, LoginOptions)` | CAS 网页登录：`appLogin` 重定向 → `/cas/rsa` 公钥 → RSA/PKCS#1 v1.5 加密 `ul/pl` → 表单提交 → `getUserName` 回调取 `JWT.SECRET.LOGIN_OUT_TIME`。验证码与二次认证通过回调交给调用方。 |
| `SaveHeaderXML`、`WriteUserInfo`、`EnsureSPXML` | 把登录结果写回凭证文件，旧文件先备份；与旧 `SECRET` 绑定的派生字段会一并刷新。 |
| `DeriveSigningKey(secret)` | `SECRET` = `base64(4 字节总长 \| 16 字节 IV \| 密文)`，AES-256-CBC 解出 32 字节签名密钥。 |
| `CanonicalizeSigningTarget(target)` | 复刻 native 规范化：`?` 之前原样保留，查询段按 `(key, value)` 字节序排序后拼接，value 做一次 form 解码，不含 `=` 的尾段丢弃、中段报错。 |
| `BuildSignature(key, target, ms)` | `SHA-256(key ‖ canonical(target) ‖ ms(BE64) ‖ reverse(key))` 的 hex，并在固定位置插入 `1/9/9/s/t`，共 69 字符。 |
| `Nonce(ms)` | native 时钟 DAG 的 64 位 nonce；请求头 `timestamp` 用的就是它。 |
| `NativeSigner` | 纯 Go 签名器，与 native 的 `(timestamp, sign)` 一致，不读 APK/.so、不启动子进程。 |
| `EncryptForUpload`、`EncryptECB`、`DecryptECB` | AES-128/ECB/PKCS#7，输出 base64 或 hex，对应 `jsonsports` 字段。 |
| `EncryptClientHeader`、`V1Header` | 反作弊 `client` 头（`{heap,root,t}` 的 AES）与 `v1` 头（`MD5(seed \| 毫秒时间)`）。 |
| `LoadRoute`、`BuildGeometry`、`RouteGeometry.PointAt` | 读 `[[lat, lon], ...]`，首尾小于 20 m 自动闭合，Chaikin 圆角后累计长度。 |
| `RouteFromGeoJSON`、`LoadRoutePoints`、`WriteRouteFile`、`RouteLengthM` | 把手绘 GeoJSON 的 LineString 换成 `[lat, lon]` 并按间距重采样；读写路线文件（不做平滑）；量算折线长度。 |
| `BuildMultiLapRoute` | 单圈扩成多圈：每圈施加横向漂移与逐点 GPS 抖动，直到达到目标距离。 |
| `GenerateRouteRecord(route, GenerateOptions)` | 按 4.0–5.0 m/s 缓慢起伏的 1 Hz 轨迹生成记录，并派生配速、分钟速度、步数、步频、平均速度。 |
| `BuildPayloadJSON`、`BuildPrejudgmentJSON` | 复刻 `UploadJsonSports.build()` 的字段顺序与 Gson HTML-safe 转义（`' < > & =` → `\u00XX`）；预判包不含坐标快照。 |
| `ValidateRecordConsistency`、`AssertRecordConsistent` | 只检查 payload 自身能互相验证的关系：里程/时长/速度/配速/步频、`d` 与 `da`、坐标折线、时间窗口。 |
| `AssertRecordNotInServerFuture` | 提交前用服务端时钟确认记录确已结束（默认允许 2 分钟偏差）。 |
| `Client`（`ProbeServerTime`、`RequestInSportTime`、`RequestSportsID`、`RequestPrejudgment`、`UploadSavesports`、`UploadSensorZip`、`UploadSensorFile`） | 业务端点与拦截器头：`Content-Type`、`imei`、`timestamp`、`sign`、`User-Agent`、`Accept-Encoding: gzip`、`Connection: Keep-Alive`、`client`、`v1`、`cgAuthorization`。每个请求的签名串与实际 URL/表单一致。 |
| `UploadRoute`、`PrepareRoute`、`SubmitSession`、`CheckSession` | 流水线四步；`UploadOptions.Fast` 走对照实验，`DryRun` 完全离线。 |
| `SaveSession`、`LoadSession`、`LatestSession` | `prepare`/`submit` 之间落盘；文件与参考实现同构，可直接读取。 |
| `MakeSensorTrace`、`PrepareSensorZipUpload`、`PrepareSensorFileUpload`、`LoadReferenceSensor` | 合成 ~10 Hz 加速度幅值序列；gzip（level 6、mtime 0、OS 255）后按 76 字符分行 base64；也支持带 SHA-256 校验的真机快照。 |
| `LoadSportJSON`、`MutateRecord`、`ShiftToDate` | 读数据库导出的 `sport_*.json`，在局部米制平面做平滑形变、重排采样间隔与距离/速度序列，并重建配速与分钟速度。 |

可用 `errors.Is` 区分 `hustrun.ErrConfig`（配置/凭证/设备指纹）、`hustrun.ErrSigner`（签名密钥或签名串）、`hustrun.ErrInvalidRecord`（一致性校验）与 `hustrun.ErrProtocol`（响应形状）。

## 配置与凭证

`config.yaml` 只覆盖非敏感参数；凭证来自 `login` 写入的 SharedPreferences XML：

| 文件 | 关键字段 |
| --- | --- |
| `HEADER.xml` | `TOKEN`、`SECRET`、`LOGIN_OUT_TIME` |
| `USERINFO.xml` | `USERXH`、`USERNAME` |
| `SCHOOL.xml` | `SERVERURL`（缺省 `https://petzjk.hust.edu.cn/cgapp-server`） |

`paths.*`、`data.sensor_path` 等相对路径以配置文件所在目录为基准。`SECRET` 与签名结果不写日志，异常只报告长度或格式。

## 会话文件

`prepare` 把已领取的 sportId 与生成的记录写到 `.sessions/<start>_<sportId>.json`：

```json
{
  "version": 1,
  "sport_id": "…",
  "issued_at_ms": 1792980518965,
  "created_at": "2026-10-25 10:01:58",
  "route_path": "hustrun/routes/route_track.json",
  "target_distance_m": 2150,
  "seed": 9,
  "sport_type": "3",
  "route_id": "7",
  "route_name": "跑步-男生",
  "record": { "sport_id": "…", "points": [ … ], "pace": [ … ], "minute_speed": [ … ] }
}
```

`submit --no-wait` 在记录还没结束时直接报错退出（`not_finished`），方便交给定时任务重试；不加则原地等到结束。

## 轨迹生成与一致性

服务端判的是**轨迹是否像真人跑的**。生成器按同一统计特征产出数据：

- 速度 = 基准 + 慢正弦 + 小噪声，钳在 4.0–5.0 m/s；采样间隔 1 s 附近 ±5 % 抖动；
- 每点 `s` 是定位瞬时 km/h（与 `d / Δt` 不必相等，官方样本也是如此），`d` 由相邻坐标 haversine 计算，`da` 为累计；
- 定位精度在 4–12 m 随机，横向有约 0.7 m 的缓慢漂移，不贴着数学折线跑；
- 配速、分钟速度、平均速度、步数、步频全部由同一条轨迹派生，并通过一致性检查；
- 与改包客户端一致：`maxSpeedPerHour` 覆盖为 `avgSpeed`，`minSpeedPerHour` 钳成 `"6.0"`。

`ValidateRecordConsistency` 只断言 payload 内部可相互验证的关系，不猜测服务端风控阈值——通过校验不等于服务端一定判定有效。

## 未移植的部分

| 参考实现的功能 | 现状 |
| --- | --- |
| `hustrun/db_reader.py`（用 SQLCipher 读加密的 WCDB 数据库） | 未移植：需要 C 依赖的 SQLCipher。改用 `LoadSportJSON` 读数据库导出的 `sport_*.json`，再交给 `MutateRecord`。 |
| `qqbot/`（QQ 机器人、邀请码、多用户目录、每日定时上传） | 未移植：属于交互层与调度层。`UploadRoute`/`PrepareRoute`/`SubmitSession` 已可被任意调度器调用，定时与多用户由调用方自行编排。 |

## 限制

- 只在 06:00–23:00（服务端时区，固定 UTC+8）时段、单次 ≥ 2.0 km 的规则下有效；服务端 `inSportTime` 返回 `"0"` 时会提前终止。
- 提交后是「待审」，学校审核后才变成「有效」。
- TOKEN 约 7 天过期（业务码 `2001`），重新 `login` 即可；本地先按 JWT `exp` 判断。
- 协议对齐 App 1.0.9；服务端升级后可能需要重新抓包复核。
- 随机数发生器是实现自带的 splitmix64：同一 seed 在 Go 内可复现，但与参考实现（CPython 的 MT19937）不逐字节相同。

## 测试

```sh
go test ./...                 # 全部
go test ./hustrun/ -run Sign  # 签名向量
go test ./hustrun/ -cover     # 覆盖率
```

`hustrun` 的测试包含 native nonce 与签名向量、规范化用例表、AES/传感器打包、生成器统计特征、一致性反例，以及一个本地假服务端：它逐请求重算签名、解密 `jsonsports`，并断言调用顺序（时间 → 时段 → sportId → 预判 → 提交 → 传感器）。
