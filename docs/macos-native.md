# macOS 原生部署指南（Apple Silicon）

本指南记录 manoiio/STRMhub fork 的原生 macOS 部署方式。它适用于 Apple Silicon Mac，不需要 Docker、OrbStack、Colima 或 Linux 虚拟机。Docker 仍是 README 中的标准快速部署方式；本指南覆盖经过本 fork 验证的原生运行路径。

## 环境与目录

- macOS Apple Silicon；项目要求 Go 1.25.0 或更新版本。
- 当前 Go SQLite 驱动支持 CGO_ENABLED=0。
- 运行时工作目录必须是源码仓库 `/Volumes/XD20/Developer/STRMhub`：服务从仓库内的 `./web` 读取 HTML、CSS、JavaScript 和 vendor 资源。
- ffmpeg / ffprobe 不是 STRM 生成和 302 基本链路的必需项。观影门户转封装和媒体信息补全需要安装相应命令，并确保 LaunchAgent 的 PATH 可找到它们。

二进制、凭据、数据库和日志都放在仓库的 `.runtime/` 目录中；该目录已加入 `.gitignore`，不会进入 Git。程序不从 `~/Library/Application Support/STRMhub` 加载运行文件。

    /Volumes/XD20/Developer/STRMhub/
    ├── web/                         # 服务直接读取的前端静态资源
    └── .runtime/
        ├── bin/strmhub              # 本机构建的可执行文件
        ├── config/                  # 账号凭据、115 Cookie、应用配置、JWT 密钥
        ├── data/                    # SQLite 数据库
        └── logs/                    # app.log

macOS 要求用户级 LaunchAgent 注册文件放在 `~/Library/LaunchAgents`；plist 中的程序、工作目录、配置、数据库和日志路径都指向上面的源码仓库。媒体输出目录仍由「账号管理 / 账号同步」中的本地媒体目录配置决定，与程序运行目录分开；迁移程序时不要擅自改动该媒体路径。

源码仓库位于外接卷时，首次启动可能触发 macOS 的“可移动宗卷”访问提示；允许 STRMhub 访问该卷后再确认服务启动。LaunchAgent 不设置外接卷上的 stdout/stderr 重定向，应用日志由程序直接写入 `.runtime/logs/app.log`。

## 构建与手动启动

从源码仓库根目录执行：

    ROOT="/Volumes/XD20/Developer/STRMhub"
    RUNTIME="$ROOT/.runtime"
    mkdir -p "$RUNTIME/bin" "$RUNTIME/config" "$RUNTIME/data" "$RUNTIME/logs"
    chmod 700 "$RUNTIME" "$RUNTIME/config" "$RUNTIME/data"

    cd "$ROOT"
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "$RUNTIME/bin/strmhub" .

`main.go` 使用相对路径读取 `./web`，所以手动启动和 LaunchAgent 的工作目录都必须是源码仓库根目录。手动启动示例：

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
        <string>/Volumes/XD20/Developer/STRMhub/.runtime/bin/strmhub</string>
      </array>
      <key>WorkingDirectory</key>
      <string>/Volumes/XD20/Developer/STRMhub</string>
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

## 更新现有安装

先停止 LaunchAgent，再从源码仓库构建运行时二进制。服务直接读取源码仓库的 `web/`，配置与 SQLite 保存在仓库的 `.runtime/` 中：

    launchctl bootout "gui/$(id -u)/com.strmhub.native"
    ROOT="/Volumes/XD20/Developer/STRMhub"
    cd "$ROOT"
    RUNTIME="$ROOT/.runtime"
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "$RUNTIME/bin/strmhub" .
    launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.strmhub.native.plist"

更新后确认管理页、115 登录状态和 STRMhub 日志正常。不要用源码更新覆盖 `.runtime/config`、`.runtime/data` 或媒体目录。
