# macOS 原生部署指南（Apple Silicon）

本指南记录 manoiio/STRMhub fork 的原生 macOS 部署方式。它适用于 Apple Silicon Mac，不需要 Docker、OrbStack、Colima 或 Linux 虚拟机。Docker 仍是 README 中的标准快速部署方式；本指南覆盖经过本 fork 验证的原生运行路径。

## 环境与目录

- macOS Apple Silicon；项目要求 Go 1.26.0 或更新版本。
- 当前 Go SQLite 驱动支持 CGO_ENABLED=0。
- 运行时工作目录必须包含 `./web`：服务从其中读取 HTML、CSS、JavaScript 和 vendor 资源。本机 LaunchAgent 使用用户目录中的静态文件副本。
- ffmpeg / ffprobe 不是 STRM 生成和 302 基本链路的必需项。观影门户转封装需要相应命令，并确保 LaunchAgent 的 PATH 可找到它们；Emby 媒体信息探测由 Emby 自己的 ffprobe 完成。

配置、数据库和日志保留在仓库的 `.runtime/` 目录中；该目录已加入 `.gitignore`，不会进入 Git。LaunchAgent 从用户目录加载二进制和 `web/` 静态文件副本。

    /Volumes/XD20/Developer/STRMhub/
    ├── web/                         # 前端静态资源源码
    └── .runtime/
        ├── bin/strmhub              # 本机构建的可执行文件副本
        ├── config/                  # 账号凭据、115 Cookie、应用配置、JWT 密钥
        ├── data/                    # SQLite 数据库
        └── logs/                    # app.log

    /Users/xlt/Library/Application Support/STRMhub/
    ├── bin/strmhub                  # LaunchAgent 使用的可执行文件
    └── web/                         # 与当前源码同步的静态文件

macOS 要求用户级 LaunchAgent 注册文件放在 `~/Library/LaunchAgents`；plist 中的程序和工作目录指向用户目录，配置、数据库和日志仍指向仓库。媒体输出目录仍由「账号管理 / 账号同步」中的本地媒体目录配置决定，与程序运行目录分开；迁移程序时不要擅自改动该媒体路径。

源码仓库位于外接卷时，LaunchAgent 需要能读取该卷上的配置和媒体文件；如启动卡在文件访问阶段，应在 macOS「隐私与安全」中检查 STRMhub 的磁盘访问授权。本机更换构建后的可执行文件时，曾需要重新切换一次该授权才能启动，应以健康接口和日志确认。LaunchAgent 不设置外接卷上的 stdout/stderr 重定向，应用日志由程序直接写入 `.runtime/logs/app.log`。

## 构建与手动启动

从源码仓库根目录执行：

    ROOT="/Volumes/XD20/Developer/STRMhub"
    RUNTIME="$ROOT/.runtime"
    mkdir -p "$RUNTIME/bin" "$RUNTIME/config" "$RUNTIME/data" "$RUNTIME/logs"
    chmod 700 "$RUNTIME" "$RUNTIME/config" "$RUNTIME/data"

    cd "$ROOT"
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "$RUNTIME/bin/strmhub" .

`main.go` 使用相对路径读取 `./web`。从源码仓库手动启动时，工作目录为仓库根目录；LaunchAgent 使用包含 `web/` 副本的用户目录。手动启动示例：

    cd "$ROOT"
    CONFIG_DIR="$RUNTIME/config" DATA_DIR="$RUNTIME/data" \
      STRMHUB_LOG_FILE="$RUNTIME/logs/app.log" "$RUNTIME/bin/strmhub"

默认管理端口为 6060，302 代理端口为 6086，观影门户端口为 6688。启动后访问 http://127.0.0.1:6060。

若没有设置管理员环境变量且配置目录尚无管理员账号，首次启动会生成随机凭据并写入应用日志。也可在首次启动时设置 AUTH_USER 与 AUTH_PASSWORD。把凭据放进 plist 时，plist 会以明文保存；应限制权限并且不要提交到 Git。JWT 密钥会自动写入 config/jwt.key。

## LaunchAgent 示例

先创建 `~/Library/LaunchAgents/com.strmhub.native.plist`。launchd 不会展开 `~`、`$HOME` 或 Shell 变量，所以所有路径都必须写成绝对路径。

    <?xml version="1.0" encoding="UTF-8"?>
    <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
    <plist version="1.0">
    <dict>
      <key>Label</key>
      <string>com.strmhub.native</string>
      <key>ProgramArguments</key>
      <array>
        <string>/Users/xlt/Library/Application Support/STRMhub/bin/strmhub</string>
      </array>
      <key>WorkingDirectory</key>
      <string>/Users/xlt/Library/Application Support/STRMhub</string>
      <key>EnvironmentVariables</key>
      <dict>
        <key>CONFIG_DIR</key>
        <string>/Volumes/XD20/Developer/STRMhub/.runtime/config</string>
        <key>DATA_DIR</key>
        <string>/Volumes/XD20/Developer/STRMhub/.runtime/data</string>
        <key>STRMHUB_LOG_FILE</key>
        <string>/Volumes/XD20/Developer/STRMhub/.runtime/logs/app.log</string>
      </dict>
      <key>RunAtLoad</key>
      <true/>
      <key>KeepAlive</key>
      <true/>
    </dict>
    </plist>

校验并加载：

    plutil -lint "$HOME/Library/LaunchAgents/com.strmhub.native.plist"
    launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.strmhub.native.plist"
    launchctl print "gui/$(id -u)/com.strmhub.native"

重启与停止：

    launchctl kickstart -k "gui/$(id -u)/com.strmhub.native"
    launchctl bootout "gui/$(id -u)/com.strmhub.native"

如需设置 AUTH_USER / AUTH_PASSWORD 或其他敏感变量，建议把 plist 权限限制为仅当前用户可读写，并且不要把真实 plist 放进仓库。launchd 的 PATH 很精简；若使用 ffmpeg / ffprobe，可在 EnvironmentVariables 中添加完整 PATH，例如 Homebrew 安装路径加系统路径。

## 网络与播放

- 管理端默认 6060，STRM 播放代理默认 6086，门户默认 6688；需要从局域网访问时，应按实际网络环境配置防火墙和监听方式。
- 账号同步页面的本地媒体目录必须指向实际可写位置。Emby 媒体库添加同一个媒体目录；Infuse 通过 Emby 访问媒体库。
- 标准播放链路应由 STRMhub 返回 302，把播放器引向 115 CDN。是否真正由客户端直连 CDN，要结合 Emby 的 Direct Play 状态、STRMhub 请求日志和播放器播放行为确认。
- 默认情况下，空 User-Agent 请求允许服务器端中转回退。设置 DISABLE_STREAM_PROXY=true 后禁用该回退；无法按 302 获取直链的这类请求会返回 503。此设置不会关闭普通 302 路径。
- macOS 原生运行的默认日志位于仓库 `.runtime/logs/app.log`；LaunchAgent 示例不设置 `StandardOutPath` / `StandardErrorPath`，避免 `launchd` 无权打开外接卷上的重定向文件。网页实时日志读取当前进程实际选择的日志文件。

## 增量同步与目录移动、改名

增量同步只应用事件台账中仍为 `pending` 的事件，已完成或跳过的历史事件不会因再次出现在生活事件列表而重新执行。目录改名按事件的 `FileID` 定位改名目录，扫描范围限制在该目录；父目录 `Cid` 不用于扩大扫描范围。范围或对象类型暂时无法确认时保留事件待重试。

目录移动和改名按稳定文件 ID 更新本地路径及同步台账，保留已有文件。目标路径冲突、越过同步根目录或包含符号链接时拒绝覆盖；未完成的目录枚举不作为删除旧文件的依据。同步根目录来自当前任务的 `full.cid` 和 `full.local_path`，应先确认配置，再以小目录的移动、改名进行实际验收。

源码改动、构建产物和正在运行的 LaunchAgent 是不同状态。更新服务时必须部署包含这些改动的二进制，再查看同步日志和本地输出。Go 测试及媒体信息补齐验收不能替代真实 115 目录移动、改名的验收。

## Emby STRM 媒体信息自动补齐

Emby 扫描 `.strm` 时通常不提前探测远端媒体，所以新单集可能没有分辨率、编码和音轨信息。在「系统配置 → EMBY管理」配置 Emby 地址及 API 密钥后，开启「自动补齐 STRM 媒体信息」。STRMhub 会从 Emby API 查找缺少视频或音频轨道的 `.strm`，按每个版本自己的 MediaSourceId 逐项触发 Emby 探测，至少间隔 8 秒；已有外挂字幕的条目也在处理范围内。每轮结束后间隔 30 分钟重新扫描，因此新增媒体和被刷新后丢失媒体流信息的条目也会补齐。失败条目暂缓 2 小时再试；连续服务故障 3 项时停止本轮，等待下一轮，以免持续请求远端。

当 Emby 和 STRMhub 都在同一台 Mac 上运行时，在「STRM 直连域名」填写 `http://127.0.0.1:6086`。生成的 `.strm` 使用本机回环地址，Emby 探测不再依赖 Wi-Fi 或热点分配的局域网 IP。其他设备应通过 STRMhub 的 Emby 反向代理访问媒体；反代会将播放地址改写为客户端可访问的地址。若设备绕过反代直接读取 `.strm` 中的 `127.0.0.1` 地址，它访问的是设备自身，无法连接这台 Mac。

结果保存在 Emby 自己的 `data/library.db`，不写入 `.strm` 或 115。管理页可刷新查看本轮总数、成功数和失败数，关闭开关会在当前请求结束后停止后续探测。启用本功能时，移除 Emby 的「Process Strm targets」计划任务触发器，避免两个批量任务同时请求 `/d/`。STRMhub 原有的 `/d/` 每 IP 限流仍然生效；如果看到 429，先检查是否有其他探测任务并发运行。

### ISO 直链播放与自动补齐媒体信息

ISO 的 `.iso.strm` 始终使用原始 `/d/{pickcode}.iso` 地址。Infuse 通过 Emby 反代获取 115 的完整 ISO 直链，保留完整镜像内容；播放不经过主片 M2TS 中转。菜单是否可用取决于播放器，不能由直链检查确认。

安装 `tools/emby-iso-media-info/Plugin.cs` 编译出的 `STRMhub.IsoMediaInfo.dll` 至 Emby 插件目录（本机为 `~/.config/emby-server/plugins/`），重启 Emby。该插件使用当前安装的 Emby 原生探测及数据库接口。编译可使用 .NET SDK 和安装目录中的程序集引用；本机使用 Microsoft.Net.Compilers.Toolset 4.8.0 与 Emby 自带 .NET 6 运行时构建：`python3 tools/emby-iso-media-info/build_macos.py /path/to/compiler.nupkg --output STRMhub.IsoMediaInfo.dll`。编译器包应从 NuGet 官方源取得。

开启自动补齐后，工作程序会自动处理缺少媒体信息的 ISO，包含后来新增且已经被 Emby 扫描的 ISO，无需维护文件名单。它按 HTTP Range 读取远端 UDF 目录和主片少量数据，交给本机 Emby 探测，再由插件持久保存视频、音轨、字幕、时长和原始镜像大小。临时 `/iso-media/{pickcode}/main.m2ts` 仅允许持有短期探测租约的本机 ffprobe 访问；普通 `/d/` 播放路径始终保持原始 ISO。不会下载整份镜像或修改 115 原文件。

目前自动识别支持有明确单一主片 M2TS、且主播放列表验证一致的蓝光 UDF ISO。DVD、多片段拼接、多版本或无法确定主片的镜像会明确失败，保留原始直链播放。断网失败会按既有重试规则处理；回环地址不依赖热点分配的 IP，115 链接失效时重新取链。更换网络时正在播放的连接仍可能中断，需要播放器重新连接。

### 本机验收记录（2026-09-30）

在 Emby 4.10.0.40 上，《当幸福来敲门》的蓝光 ISO 经插件和正式 LaunchAgent 自动补齐：后台状态为总数 1、成功 1、失败 0。Emby 返回 1920×1080 H.264、117 分 27 秒、10 条音轨和 22 条字幕，结果持久保存在 `MediaStreams2`；原始 STRM 内容未改变。

Infuse User-Agent 请求的播放地址返回 302，115 CDN 的小范围读取返回 206，镜像总长为 49,221,140,480 字节，检查到 ISO 签名。这里只读取少量字节，未下载整个镜像。这证明完整 ISO 直链仍可用；Infuse 设备的实际画面、蓝光菜单、切轨和拖动未在此次验收中验证。

普通检查：`go test ./internal/api -count=1`。远端读取检查需显式设置 `STRMHUB_ISO_PROBE_LIVE=1`；原生 Emby 导入检查需设置 `STRMHUB_ISO_PROBE_EMBY_LIVE=1` 和 `STRMHUB_ISO_PROBE_ITEM_ID`。后者会向指定 ISO 条目保存媒体信息，只应对已授权的本机样本运行。

## 更新现有安装

先停止 LaunchAgent，再从源码仓库构建二进制，并同步用户目录中的程序与静态文件。配置与 SQLite 保存在仓库的 `.runtime/` 中：

    launchctl bootout "gui/$(id -u)/com.strmhub.native"
    ROOT="/Volumes/XD20/Developer/STRMhub"
    cd "$ROOT"
    RUNTIME="$ROOT/.runtime"
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "$RUNTIME/bin/strmhub" .
    INSTALL="$HOME/Library/Application Support/STRMhub"
    mkdir -p "$INSTALL/bin"
    cp "$RUNTIME/bin/strmhub" "$INSTALL/bin/strmhub.next"
    mv -f "$INSTALL/bin/strmhub.next" "$INSTALL/bin/strmhub"
    ditto "$ROOT/web" "$INSTALL/web"
    launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.strmhub.native.plist"

更新后确认管理页、115 登录状态和 STRMhub 日志正常。不要用源码更新覆盖 `.runtime/config`、`.runtime/data` 或媒体目录。
