# codexm

`codexm` 是一个面向 OpenAI Codex CLI 的多账号与多项目管理器。

它通过为每个账号分配独立的 `CODEX_HOME`，隔离账号登录凭证、`config.toml`、会话历史、日志和缓存；再通过“项目目录绑定”自动选择正确账号。

> 这是第三方开源工具，不是 OpenAI 官方产品。它不会读取、上传或保存你的 ChatGPT 密码，也不会自行处理 OAuth token；实际登录仍由官方 `codex login` 完成。

## 核心能力

- 多账号独立管理：一个账号一个 `CODEX_HOME`
- 默认使用 `cli_auth_credentials_store = "file"`，避免多个账号共用系统钥匙串
- 项目目录绑定：进入项目后直接运行 `codexm run`
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

### 登录状态

```bash
codexm status account1
codexm status --all
```

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
- 是否使用独立的文件凭证存储
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
```

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

### 使用预编译程序

从发布包中选择对应平台：

- macOS Apple Silicon：`darwin-arm64`
- macOS Intel：`darwin-amd64`
- Linux x86_64：`linux-amd64`
- Linux ARM64：`linux-arm64`
- Windows x86_64：`windows-amd64`
- Windows ARM64：`windows-arm64`

解压后，把 `codexm` 或 `codexm.exe` 放入 `PATH`。

### 从源码构建

```bash
git clone <your-repository-url>
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

### 使用 Go 直接安装本地源码

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
账号：~/Library/Application Support/codexm/profiles/<name>
```

### Linux

```text
配置：~/.config/codexm/config.json
账号：~/.local/share/codexm/profiles/<name>
```

支持 `XDG_CONFIG_HOME` 和 `XDG_DATA_HOME`。

### Windows

```text
配置：%APPDATA%\codexm\config.json
账号：%LOCALAPPDATA%\codexm\profiles\<name>
```

## 安全说明

- `codexm` 的 `config.json` 只记录账号名称、目录和项目绑定，不保存登录凭证。
- 使用文件存储时，Codex 官方凭证位于每个账号目录下的 `auth.json`。
- `auth.json` 应按密码对待，不要提交到 Git，不要上传，不要发送给他人。
- macOS/Linux 下，`codexm` 创建账号目录时使用 `0700` 权限，配置文件使用 `0600` 权限。
- 不建议多账号模式使用 `keyring`，因为系统凭证存储可能绕过 `CODEX_HOME` 的目录隔离。
- 删除账号前建议先运行 `codexm logout PROFILE`。

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
codexm run [--project PATH] [PROFILE] -- [CODEX_ARGS...]
codexm shell PROFILE
codexm doctor
codexm config-path
codexm version
```

## 开发与测试

```bash
go test ./...
./scripts/build-all.sh 0.1.0
```

跨平台构建产物会写入 `dist/`。

## 设计依据

- Codex 将本地状态放在 `CODEX_HOME`，默认是 `~/.codex`。
- `cli_auth_credentials_store = "file"` 会把凭证放在对应 `CODEX_HOME/auth.json`。
- `codex login`、`codex login --device-auth`、`codex login status` 和 `codex logout` 均由官方 CLI 执行。

官方参考：

- https://learn.chatgpt.com/docs/auth
- https://learn.chatgpt.com/docs/config-file/config-advanced
- https://learn.chatgpt.com/docs/developer-commands?surface=cli
