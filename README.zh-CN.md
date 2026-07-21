# codexm

[English](README.md) | 简体中文

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.22+](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](https://go.dev/)

`codexm` 是一个 OpenAI Codex CLI 多账号（多帐号）管理与切换、多项目管理和
账号隔离工具。

它通过为每个账号分配独立的 `CODEX_HOME`，隔离账号登录凭证、`config.toml`、会话历史、日志和缓存；再通过“项目目录绑定”自动选择正确账号。

> 这是第三方开源工具，不是 OpenAI 官方产品。它不会读取、上传或保存你的 ChatGPT 密码，也不会自行处理 OAuth token；实际登录仍由官方 `codex login` 完成。

## 核心能力

- 多账号独立管理：一个账号一个 `CODEX_HOME`
- 默认使用文件存储隔离 ChatGPT 登录凭据和 MCP OAuth 凭据
- 跨账号共享 MCP 定义，支持按账号排除、同步和独立 OAuth 登录
- 兼容 Codex 的全局与项目级 Skills 发现机制
- 项目目录绑定：进入项目后直接运行 `codexm run`
- 显式启用的仓库级 Session 镜像，支持个人多设备迁移和团队交接
- 聚合全部账号和项目的只读浏览器 Dashboard 与终端 TUI
- 按需启动、按账号隔离的 Codex app-server，实时展示任务状态
- 子目录自动继承：绑定项目根目录后，其所有子目录自动使用同一账号
- 支持显式指定账号：`codexm run account1`
- 支持透传全部 Codex 参数
- 登录、退出、状态检查、默认账号、诊断
- 支持 macOS、Linux、Windows 原生终端
- 不在管理配置中保存任何 token、密码或 API Key

## 工作原理

Codex CLI 默认把本地状态存放在 `~/.codex`。`codexm` 为每个账号创建单独目录，并在启动 Codex 时注入：

```text
CODEX_HOME=<该账号的独立目录>
```

示例：

```text
account1 -> ~/.local/share/codexm/profiles/account1
account2 -> ~/.local/share/codexm/profiles/account2
```

不同平台的默认目录会遵循各自系统惯例，也可以通过环境变量自定义。

## 前置条件

1. 已安装 OpenAI Codex CLI，并确保终端可执行：

```bash
codex --version
```

2. 使用预编译的 `codexm`，或安装 Go 1.22+ 后从源码构建。

## 快速开始

### 1. 添加两个账号配置

```bash
codexm add --description "项目1账号" account1
codexm add --description "项目2账号" account2
```

第一个账号会自动成为默认账号，可以随时修改：

```bash
codexm default account1
```

### 2. 分别登录

```bash
codexm login account1
codexm login account2
```

浏览器授权时，请确认选择了对应的 ChatGPT 账号。

远程服务器或浏览器回调不可用时：

```bash
codexm login --device account1
```

### 3. 绑定项目

macOS / Linux：

```bash
codexm bind account1 ~/Projects/project1
codexm bind account2 ~/Projects/project2
```

Windows PowerShell：

```powershell
codexm bind account1 D:\Projects\project1
codexm bind account2 D:\Projects\project2
```

### 4. 在项目中使用

```bash
cd ~/Projects/project1
codexm run
```

`codexm` 会识别当前目录属于 `project1`，然后使用 `account1` 启动 Codex。

另一个终端：

```bash
cd ~/Projects/project2
codexm run
```

会自动使用 `account2`。

### 5. 添加所有账号共享的 MCP

STDIO MCP：

```bash
codexm mcp add context7 -- npx -y @upstash/context7-mcp
```

远程 MCP：

```bash
codexm mcp add docs \
  --url https://example.com/mcp \
  --bearer-token-env-var DOCS_TOKEN
```

`codexm` 使用官方 `codex mcp` 命令维护共享定义，并自动同步到已有账号。后续新建账号也会自动继承。

## 常用命令

### 查看账号配置

```bash
codexm list
codexm list --status
codexm show account1
```

### 查看当前目录会选择哪个账号

```bash
codexm current
```

输出示例：

```text
account1    /Users/you/Projects/project1
```

### 显式指定账号启动 Codex

```bash
codexm run account1
codexm run account2
```

### 透传 Codex 参数

在 `--` 后面的内容会原样传给 Codex：

```bash
codexm run account1 -- --model gpt-5.6
codexm run account1 -- exec "检查这个项目的代码质量"
codexm run -- resume --all
```

不指定账号时，将按以下优先级选择：

1. 当前目录或最近父目录的项目绑定
2. 默认账号
3. 都不存在时提示错误

### 在指定账号环境中打开 Shell

```bash
codexm shell account1
```

新 Shell 中的 `CODEX_HOME` 已设置为 `account1`，因此可以直接运行：

```bash
codex
codex login status
codex resume --all
```

退出该 Shell 后，原终端环境不受影响。

`codexm shell` 会刻意绕过项目 Session 自动同步。如果项目已经启用 Session
镜像，通过该 Shell 使用 Codex 后需要手动执行 `codexm session sync`。

### 登录状态

```bash
codexm status account1
codexm status --all
```

## 只读可视化监控

终端 TUI 与浏览器 Dashboard 默认聚合全部账号，也可以只查看某个账号或项目：

```bash
codexm ui
codexm ui --profile account1 --project ~/Projects/project1

codexm dashboard
codexm dashboard --profile account1 --project ~/Projects/project1
```

两种界面共用同一份监控模型，展示账号套餐与限额窗口、帐号用量摘要、项目绑定与
Git 信息、项目 Session 镜像健康状态、Session 元数据、活动/等待/空闲/错误任务
以及 subagent 层级。界面根据系统 locale 自动使用简体中文或英文，并通过官方
Codex App Server 协议实时刷新。

Dashboard 的 Session 视图使用服务端搜索、排序、筛选和分页，支持按帐号、项目、
状态、归档、来源和模型过滤。帐号卡片会逐个展示已配置 MCP 服务的认证与启动
健康状态。同一个 ChatGPT 帐号即使登录到多个 profile，也仍按 profile 分开展示，
不会把限额相加。

帐号服务用量与实时观察到的 thread token 总量是两类独立指标，都不代表账单金额。
App Server 当前不会通过 `thread/list` 返回历史 thread 的模型和 token 总量；
在实时 settings/reroute 或 token 通知到达前，codexm 会明确显示“不可用”，不会
用零冒充真实数据。

所有监控能力严格只读：TUI、JSON API 和 SSE 都不能启动、中断、批准、归档或
删除任务。监控内存只保留元数据和第一条提示的短摘要；完整 transcript、工具
输出和命令输出不会被载入监控模型，也不会从 Dashboard API 暴露。

Dashboard 默认监听随机的 `127.0.0.1` 端口，生成私有访问 token，并自动打开
一个用于建立 HttpOnly 会话 Cookie 的带 token 地址。需要从局域网设备访问时：

```bash
codexm dashboard --lan --listen 0.0.0.0:7443 --no-open
codexm dashboard --lan --listen 0.0.0.0:7443 --rotate-token
```

局域网模式必须显式启用。它会生成包含本机地址 SAN 的自签 HTTPS 证书和高熵
访问 token；每台客户端需要明确信任该证书。底层 Codex app-server 始终只监听
回环地址，不会暴露到局域网。

### 托管 app-server

交互式 `codexm run` 会按需启动账号独享的 app-server，并通过带 capability
token 的回环 WebSocket 连接。Dashboard/TUI 退出后 daemon 仍继续运行：

```bash
codexm daemon start account1
codexm daemon start --all
codexm daemon status --all
codexm daemon stop account1
codexm daemon stop --all --force
```

存在活动 thread 时，普通 stop 会拒绝；`--force` 是明确覆盖。运行状态、
capability token、证书和私有日志保存在 codexm 管理目录的 `runtime/` 下，并
使用私有权限，不会写入账号配置目录中的公开文件或项目仓库。

托管远程模式适用于交互式 Codex、`resume`、`fork`、`archive`、`delete` 和
`unarchive`。`exec`、`review` 等不支持 `--remote` 的命令仍直接运行，并在
监控中显示为“未托管”。托管模式会将所选项目目录显式传给 Codex，即使
app-server daemon 是从其他目录启动的也不会改变会话的默认目录。需要保留旧
行为时可以显式使用：

```bash
codexm run --unmanaged account1 -- resume --last
```

自定义子进程 `--remote` 必须与 `--unmanaged` 同时使用。`codexm shell` 以及
绕过 codexm 启动的 Codex 进程也属于未托管。旧版 Codex 未声明远程能力时会
继续直连；已经声明能力但托管服务启动失败时，运行会停止并给出错误，不会静默
切换执行模型。

Codex app-server WebSocket 与远程 TUI 当前仍是实验接口。codexm 使用能力探测，
并允许可选方法缺失、未知字段、有界服务过载、断线重连和单账号故障降级；未来
Codex 版本仍可能需要兼容性更新。参见官方
[Codex App Server 文档](https://learn.chatgpt.com/docs/app-server.md)和
[Codex CLI 命令说明](https://learn.chatgpt.com/docs/developer-commands?surface=cli)。

## 复用 MCP 与 Skills

### 共享 MCP

查看共享服务器：

```bash
codexm mcp list
codexm mcp get context7
```

添加或删除共享服务器后，`codexm` 会自动同步所有账号。也可以手工同步：

```bash
codexm mcp sync --all
codexm mcp sync account1
```

某个账号不需要特定服务器时：

```bash
codexm mcp exclude account2 production-db
codexm mcp include account2 production-db
```

共享的是服务器定义，不是 OAuth 凭据。需要 OAuth 的 MCP 应按账号登录：

```bash
codexm mcp login account1 github --scopes repo
codexm mcp login account2 github --scopes repo
codexm mcp logout account2 github
```

共享 MCP 配置由一个独立的 shared `CODEX_HOME` 保存。查看路径：

```bash
codexm mcp path
```

同步时只更新账号 `config.toml` 中带有 `codexm` 标记的管理区块：

- 账号自己的模型、sandbox、features、注释不会被重写。
- 账号本地存在同名 `[mcp_servers.NAME]` 时，本地定义优先。
- `exclude` 的排除列表保存在 `codexm` 的 `config.json` 中。
- `run` 和 `shell` 会在启动前检查并同步配置漂移。

尽量使用 `bearer_token_env_var`、`env_vars` 或 `env_http_headers` 引用环境变量。不要把真实 token 写入共享 MCP 配置；`codex mcp add --env KEY=VALUE` 会把该值作为配置的一部分保存并同步。

### 共享 Skills

个人通用 Skills 使用 Codex 官方的用户级目录：

```text
$HOME/.agents/skills/<skill-name>/SKILL.md
```

`codexm` 不修改 `HOME`，所以这里的 Skills 会被所有账号发现，不需要复制或同步。

只适用于某个项目的 Skills 放在仓库中：

```text
<repo>/.agents/skills/<skill-name>/SKILL.md
```

项目专用 MCP 也建议直接配置在可信仓库的：

```text
<repo>/.codex/config.toml
```

不要软链接各账号 `CODEX_HOME/skills/.system` 或整个 `config.toml`；前者由 Codex 管理，后者还包含账号自己的运行配置。

## 可迁移的项目 Session

项目 Session 镜像默认关闭，需要在项目根目录显式启用：

```bash
codexm session init
```

该命令会创建稳定、与账号无关的项目 ID 以及 `.codexm/` 镜像。默认从空镜像
开始。如果需要导入当前账号中初始工作目录位于项目内的既有活动和归档会话，
必须明确指定：

```bash
codexm session init --import-existing
```

初始化后，`codexm run` 会在启动 Codex 前导入项目变更，并在 Codex 退出后
再次导出；Codex 非零退出或中断时也会尝试导出。新会话、续写、重命名、
归档、恢复和删除都会往返同步。另一台设备可以绑定自己的本地账号继续会话：

```bash
git clone <私有仓库> project
cd project
codexm bind my-local-profile .
codexm run -- resume
```

项目识别使用最近父目录中的 `.codexm/project.json`，因此单体仓库中可以存在
嵌套项目。`--profile` 只覆盖本次命令使用的本地账号，不会写入仓库。

不启动 Codex 时可以查看或执行同步：

```bash
codexm session status
codexm session sync
```

提交 `.codexm/` 前可以执行隐私审计：

```bash
codexm session audit
codexm session audit --strict
codexm session audit --json
```

审计会检查常见凭据格式、高熵字符串、超大 transcript、损坏的 JSONL，以及没有
sidecar 映射的绝对 `cwd`。输出只报告位置和类型，不会回显匹配到的秘密。默认在
发现明确错误时返回非零状态；`--strict` 也会阻断警告，适合 pre-commit hook。
仓库提供了可直接使用的 `scripts/pre-commit-session-audit.sh` 示例。

同一个 Session 的一端是另一端的严格 JSONL 前缀时，较长端胜出。如果两端都
独立追加，同步会在启动 Codex 前停止。此时必须显式选择版本：

```bash
codexm session resolve --use project SESSION_ID
codexm session resolve --use profile SESSION_ID
```

落选副本会先备份到本机私有的 `CODEXM_HOME/session-backups/`。团队并行工作
建议使用 Codex `fork`，让不同分支使用不同 Session ID。

仓库镜像只包含可重放的会话数据：

```text
.codexm/
├── project.json
├── .gitattributes
├── sessions/YYYY/MM/DD/rollout-*.jsonl[.zst]
├── archived_sessions/rollout-*.jsonl[.zst]
├── metadata/<session-id>.json
└── tombstones/<session-id>.json
```

它不会复制 `auth.json`、登录凭据、`config.toml`、日志、history 或 SQLite。
导入时只将结构化的 `session_meta.cwd` 和 `turn_context.cwd` 映射到当前检出
路径，其他会话内容保持原样。受管 `.gitattributes` 区块会阻止 Git 静默拼接
同一个 Session 的两个版本。

> Session 文件未加密，可能包含提示词、命令输出、绝对路径、源码片段或秘密。
> 请优先使用私有仓库，并在每次提交前审查 `.codexm/`。`codexm` 不会自动执行
> `git add`、提交或推送。

## 账号管理

### 解绑项目

```bash
codexm unbind ~/Projects/project1
```

在项目根目录执行时，可简写为：

```bash
codexm unbind
```

### 删除账号配置

只删除 `codexm` 中的记录，保留账号目录：

```bash
codexm remove account1
```

同时删除账号目录及其中的本地凭证、历史和配置：

```bash
codexm remove --delete-home --yes account1
```

该操作不可恢复。

### 诊断

```bash
codexm doctor
```

会检查：

- Codex CLI 是否在 `PATH` 中
- `codexm` 配置是否可读取
- 各账号 `CODEX_HOME` 是否存在
- ChatGPT 和 MCP OAuth 是否使用独立的文件凭证存储
- 共享 MCP 配置是否有效、各账号是否已经同步
- 项目绑定是否指向有效账号

## 接管已有的 CODEX_HOME

之前已经手工创建了账号目录时，可以直接纳入管理：

```bash
codexm add --home ~/.codex-account1 account1
codexm add --home ~/.codex-account2 account2
```

`codexm` 会保留已有 `config.toml` 内容，并补充或更新：

```toml
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "file"
```

由旧版 `codexm` 创建且尚未设置 MCP OAuth 存储的账号，会在下一次 `run`、`shell` 或 MCP 同步时按现有 ChatGPT 凭据存储策略补齐该设置；已经显式配置的 MCP OAuth 存储不会被覆盖。

如果原凭证存放在系统钥匙串，切换为文件存储后可能需要重新执行一次登录。

## 复制非敏感配置

新账号需要沿用另一个账号的模型、沙盒或功能配置时：

```bash
codexm add --clone-config account1 account2
```

仅复制 `config.toml`，不会复制 `auth.json`。随后仍需单独登录：

```bash
codexm login account2
```

## 安装

### Homebrew（macOS 和 Linux）

```bash
brew install iamcc30/tap/codexm
```

后续升级：

```bash
brew update
brew upgrade iamcc30/tap/codexm
```

### 使用预编译程序

从 [GitHub Releases](https://github.com/iamcc30/codexm/releases) 下载对应平台的发布包：

- macOS Apple Silicon：`darwin-arm64`
- macOS Intel：`darwin-amd64`
- Linux x86_64：`linux-amd64`
- Linux ARM64：`linux-arm64`
- Windows x86_64：`windows-amd64`
- Windows ARM64：`windows-arm64`

解压后，把 `codexm` 或 `codexm.exe` 放入 `PATH`。

### Windows 安装教程

1. 打开 [GitHub Releases](https://github.com/iamcc30/codexm/releases/latest)。
   普通 Intel/AMD Windows 电脑下载文件名以 `_windows_amd64.zip` 结尾的
   发布包；Windows on ARM 设备下载以 `_windows_arm64.zip` 结尾的发布包。
2. 解压 ZIP，进入包含 `codexm.exe` 的目录，然后在该目录打开 PowerShell。
3. 执行以下命令，将程序复制到当前用户的应用目录，并自动加入用户级
   `PATH`：

```powershell
$installDir = Join-Path $env:LOCALAPPDATA "Programs\codexm"
New-Item -ItemType Directory -Force -Path $installDir | Out-Null
Copy-Item .\codexm.exe "$installDir\codexm.exe" -Force
Unblock-File "$installDir\codexm.exe"

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$pathEntries = @($userPath -split ";" | Where-Object { $_ })
if ($pathEntries -notcontains $installDir) {
    [Environment]::SetEnvironmentVariable(
        "Path",
        (($pathEntries + $installDir) -join ";"),
        "User"
    )
}
$env:Path = "$env:Path;$installDir"
```

验证安装：

```powershell
codexm version
codex --version
```

第二条命令用于确认依赖的 OpenAI Codex CLI 也已正确安装。升级 `codexm`
时，下载新版本 ZIP，再次执行复制命令覆盖
`%LOCALAPPDATA%\Programs\codexm\codexm.exe` 即可。

### 从源码构建

```bash
git clone https://github.com/iamcc30/codexm.git
cd codexm
go test ./...
go build -o codexm ./cmd/codexm
```

macOS / Linux：

```bash
./scripts/install.sh
```

Windows PowerShell：

```powershell
.\scripts\install.ps1
```

### 使用 Go 安装

直接安装最新发布版本：

```bash
go install github.com/iamcc30/codexm/cmd/codexm@latest
```

从本地源码目录安装：

```bash
go install ./cmd/codexm
```

确保 Go 的 `bin` 目录位于 `PATH`。

## 环境变量

| 环境变量 | 用途 |
|---|---|
| `CODEXM_HOME` | 覆盖 `codexm` 自身配置目录 |
| `CODEXM_PROFILES_HOME` | 覆盖新账号的默认 `CODEX_HOME` 根目录 |
| `CODEXM_CODEX_BIN` | 指定 Codex CLI 可执行文件 |

示例：

```bash
CODEXM_PROFILES_HOME=/data/codex-profiles codexm add account1
```

## 默认存储位置

### macOS

```text
配置：~/Library/Application Support/codexm/config.json
共享 MCP：~/Library/Application Support/codexm/shared/config.toml
账号：~/Library/Application Support/codexm/profiles/<name>
```

### Linux

```text
配置：~/.config/codexm/config.json
共享 MCP：~/.config/codexm/shared/config.toml
账号：~/.local/share/codexm/profiles/<name>
```

支持 `XDG_CONFIG_HOME` 和 `XDG_DATA_HOME`。

### Windows

```text
配置：%APPDATA%\codexm\config.json
共享 MCP：%APPDATA%\codexm\shared\config.toml
账号：%LOCALAPPDATA%\codexm\profiles\<name>
```

## 安全说明

- `codexm` 的 `config.json` 只记录账号元数据、项目绑定和共享 MCP 排除项，不保存登录凭证。
- 使用文件存储时，Codex 官方凭证位于每个账号目录下的 `auth.json`。
- MCP OAuth 默认也使用各账号 `CODEX_HOME` 下的文件存储；共享 MCP 同步不会复制其凭据。
- `auth.json` 应按密码对待，不要提交到 Git，不要上传，不要发送给他人。
- macOS/Linux 下，`codexm` 创建账号目录时使用 `0700` 权限，配置文件使用 `0600` 权限。
- 不建议多账号模式使用 `keyring`，因为系统凭证存储可能绕过 `CODEX_HOME` 的目录隔离。
- 删除账号前建议先运行 `codexm logout PROFILE`。
- 仓库中的 `.codexm/` Session 镜像虽然不是登录凭据，但属于未加密会话记录，
  仍可能包含敏感内容；提交前务必审查并优先使用私有仓库。

## 完整命令

```text
codexm init
codexm add [--home PATH] [--description TEXT] [--bind PATH]
           [--credential-store file|auto|keyring]
           [--clone-config PROFILE] NAME
codexm remove [--delete-home --yes] NAME
codexm list [--status]
codexm show NAME
codexm default [NAME|--clear]
codexm bind PROFILE [PATH]
codexm unbind [PATH]
codexm current [PATH]
codexm login [--device] PROFILE
codexm logout PROFILE
codexm status [PROFILE|--all]
codexm mcp add [CODEX_MCP_ADD_ARGS...]
codexm mcp remove NAME
codexm mcp list
codexm mcp get NAME
codexm mcp sync [PROFILE|--all]
codexm mcp exclude PROFILE SERVER
codexm mcp include PROFILE SERVER
codexm mcp login PROFILE NAME [CODEX_MCP_LOGIN_ARGS...]
codexm mcp logout PROFILE NAME
codexm mcp path
codexm session init [--project PATH] [--profile PROFILE] [--import-existing]
codexm session sync [--project PATH] [--profile PROFILE]
codexm session status [--project PATH] [--profile PROFILE]
codexm session audit [--project PATH] [--strict] [--json]
                     [--max-file-size-mb N]
codexm session resolve [--project PATH] [--profile PROFILE]
                       --use project|profile SESSION_ID
codexm run [--project PATH] [PROFILE] -- [CODEX_ARGS...]
codexm shell PROFILE
codexm doctor
codexm config-path
codexm version
```

## 开发与测试

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/build-all.sh 0.1.0
```

跨平台构建产物会写入 `dist/`。

## 发布版本

发布 workflow 同时支持手动触发和推送语义化版本 tag。发布前，先把 [CHANGELOG.md](CHANGELOG.md) 中 `Unreleased` 下的待发布内容移动到新的版本号和日期章节，并在顶部保留空的 `Unreleased` 章节，然后提交并推送。

同时发布 GitHub Release 和对应 Homebrew Formula 的最快方式是：

```bash
./scripts/release.sh 0.2.1
```

该脚本会确认 `main` 工作区干净且已推送，等待 GitHub Release 发布完成，
然后更新 `iamcc30/homebrew-tap`。预发布版本不会发布到 Homebrew。

如果只需要发布 GitHub Release，可运行：

```bash
gh workflow run release.yml -f version=0.2.1
gh run watch
```

Tap 也会每天自动检查一次最新稳定版本。如需手动立即更新，可运行：

```bash
gh workflow run update.yml --repo iamcc30/homebrew-tap -f version=0.2.1
```

也可以在 GitHub 打开 **Actions → release → Run workflow**，输入 `0.2.1`。workflow 会校验版本号和 Changelog、运行测试、构建全部支持平台、创建 `v0.2.1` tag，并发布带校验文件的 GitHub Release。

仍然支持传统的 tag 触发方式：

```bash
git tag -a v0.2.1 -m "codexm v0.2.1"
git push origin v0.2.1
```

## 参与贡献

欢迎提交 Issue 和 Pull Request。提交代码前请运行：

```bash
go fmt ./...
go test ./...
go vet ./...
```

请保持改动范围清晰，并为新行为或缺陷修复补充测试。用户可见的变化请同步更新 [CHANGELOG.md](CHANGELOG.md) 和中英文 README。Bug 报告和功能建议请提交到 [GitHub Issues](https://github.com/iamcc30/codexm/issues)。

## 设计依据

- Codex 将本地状态放在 `CODEX_HOME`，默认是 `~/.codex`。
- `cli_auth_credentials_store = "file"` 会把凭证放在对应 `CODEX_HOME/auth.json`。
- `mcp_oauth_credentials_store = "file"` 会让 MCP OAuth 凭据跟随账号的 `CODEX_HOME` 隔离。
- 用户级 Skills 位于 `$HOME/.agents/skills`，项目级 Skills 位于 `.agents/skills`。
- MCP 可以放在用户 `config.toml` 或可信项目的 `.codex/config.toml` 中。
- Session 记录位于 `CODEX_HOME/sessions`，归档记录位于
  `CODEX_HOME/archived_sessions`；SQLite 元数据可以从 rollout 文件回填。
- `codex login`、`codex login --device-auth`、`codex login status` 和 `codex logout` 均由官方 CLI 执行。

官方参考：

- https://learn.chatgpt.com/docs/auth
- https://learn.chatgpt.com/docs/config-file/config-advanced
- https://learn.chatgpt.com/docs/developer-commands?surface=cli
- https://learn.chatgpt.com/docs/build-skills
- https://learn.chatgpt.com/docs/extend/mcp
- https://github.com/openai/codex/blob/rust-v0.144.5/codex-rs/thread-store/src/local/mod.rs
- https://github.com/openai/codex/blob/rust-v0.144.5/codex-rs/rollout/src/metadata.rs

## 许可证

本项目使用 [MIT License](LICENSE)。
