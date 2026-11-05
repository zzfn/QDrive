# QDrive

夸克网盘命令行工具，用 Go 编写，支持 macOS、Linux 和 Windows。桌面 App 留待后续开发。

> QDrive 使用非官方接口，接口变动或账号风控可能导致功能失效。扫码获取二维码已验证，扫码后的登录、列表和文件传输尚未使用真实账号完整验证。

## Linux 安装

首个 GitHub Release 发布后，可以用以下命令安装最新版本，支持 Linux amd64 和 arm64：

```sh
curl -fsSL https://raw.githubusercontent.com/zzfn/QDrive/main/scripts/install.sh | sh
```

默认安装到 `~/.local/bin/qdrive`。安装脚本会校验发布包的 SHA-256，并在目标文件已存在时替换它；如果 `~/.local/bin` 不在 `PATH` 中，按脚本提示添加。也可以指定版本或安装目录：

```sh
curl -fsSL https://raw.githubusercontent.com/zzfn/QDrive/main/scripts/install.sh | QDRIVE_VERSION=v0.1.0 sh
curl -fsSL https://raw.githubusercontent.com/zzfn/QDrive/main/scripts/install.sh | QDRIVE_INSTALL_DIR="$HOME/bin" sh
```

目前尚未发布首个版本，安装命令在首个 Release 发布前无法使用。

## Windows 安装

首个 GitHub Release 发布后，在 PowerShell 中运行（支持 Windows x64 和 ARM64）：

```powershell
irm https://raw.githubusercontent.com/zzfn/QDrive/main/scripts/install.ps1 | iex
```

默认安装到 `%USERPROFILE%\.local\bin\qdrive.exe`。脚本会校验发布包的 SHA-256，不会修改系统 PATH；若目标文件已存在，安装成功时会替换它。可在执行前设置 `$env:QDRIVE_VERSION = 'v0.1.0'` 指定版本，或设置 `$env:QDRIVE_INSTALL_DIR` 指定目录。

未将安装目录加入 PATH 时，可用完整路径启动：

```powershell
& "$env:USERPROFILE\.local\bin\qdrive.exe" login --qr
& "$env:USERPROFILE\.local\bin\qdrive.exe" ls /
```

Windows 安装脚本和登录、传输流程仍需在真实设备上验证。首个 Release 发布前安装命令无法使用。

## 从源码构建

需要 Go 1.27.3 或更高版本：

```sh
git clone https://github.com/zzfn/QDrive.git
cd QDrive
go build -o qdrive ./cmd/qdrive
./qdrive --help
```

交叉编译 Linux 版本：

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o qdrive-linux-amd64 ./cmd/qdrive
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o qdrive-linux-arm64 ./cmd/qdrive
```

交叉编译 Windows 版本：

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o qdrive-windows-amd64.exe ./cmd/qdrive
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -o qdrive-windows-arm64.exe ./cmd/qdrive
```

Linux 和 Windows 二进制已验证可以编译，尚未在对应系统的真实设备上完整验证。

## 登录

使用夸克 App 扫码：

```sh
./qdrive login --qr
```

二维码包含临时登录信息，不要分享。登录后可运行 `./qdrive ls /` 检查是否成功。

也可以从夸克网盘网页版取得 Cookie，运行 `./qdrive login` 并从标准输入提供一行 Cookie。直接粘贴到终端时内容可能可见；以下方式可在 Bash 中隐藏输入：

```bash
read -rs -p 'Cookie: ' QDRIVE_COOKIE; echo
printf '%s\n' "$QDRIVE_COOKIE" | ./qdrive login
unset QDRIVE_COOKIE
```

手动登录只保存 Cookie，不验证有效性。Cookie 以明文保存在本机用户配置目录下的 `qdrive/credentials.json` 中；Unix 下目录权限为 `0700`，文件权限为 `0600`。Windows 下会将凭据目录和文件的访问控制列表限制为当前用户，这可能影响以其他账户运行的备份或安全工具。不要把 Cookie、二维码或凭据文件发给别人，也不要提交到仓库。

## 使用

```sh
# 查看根目录或指定目录
./qdrive ls /
./qdrive ls /目录名

# 上传单个文件到根目录或指定目录
./qdrive upload ./本地文件
./qdrive upload ./本地文件 /目录名

# 下载单个文件；不指定本地路径时保存到当前目录
./qdrive download /目录名/文件名
./qdrive download /目录名/文件名 ./保存路径

# 清除本地登录信息
./qdrive logout
```

路径包含空格时请用引号包起来。`logout` 只清除本地 Cookie，不会撤销服务端会话。操作时可以按 Ctrl+C 取消。

## 当前限制

- 只支持单文件上传和下载，不支持目录传输、断点续传或上传空文件。
- 上传前会检查目标目录中是否有同名文件；下载不会覆盖本地已有文件。失败时会尝试清理本地未完成的下载文件。
- 为保护凭据和本地文件，传输只接受 HTTPS 的 `quark.cn` 域名；服务端返回其他域名时会拒绝。
- 大文件下载重试、异步下载及上传流程有模拟测试，但尚未用真实账号验证。

## Agent Skill

仓库内置 [QDrive Skill](skills/qdrive/SKILL.md)，供支持 Skill 的智能体了解 CLI 用法和凭据安全边界。请按所用智能体客户端的方式安装或加载；放在仓库中不会自动对所有客户端生效。

## 测试

```sh
go test ./...
go vet ./...
sh scripts/test-install.sh
```

推送 `v*` 标签会触发 GitHub Actions，构建 Linux 和 Windows 的 amd64、arm64 安装包及校验文件，并创建对应的 GitHub Release。Windows 安装脚本也可在 Windows 上运行 `powershell -NoProfile -ExecutionPolicy Bypass -File scripts/test-install.ps1` 进行离线测试。

网盘协议和 CLI 自行实现；终端二维码渲染使用 [go-qrcode](https://github.com/skip2/go-qrcode)。
