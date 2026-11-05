# QDrive

夸克网盘命令行工具，桌面端计划后续再考虑。

目前仅初始化了 CLI 入口，网盘功能尚未实现。

```sh
go run ./cmd/qdrive --help
go build -o qdrive ./cmd/qdrive
```

构建 Linux 版本：

```sh
GOOS=linux GOARCH=amd64 go build -o qdrive-linux-amd64 ./cmd/qdrive
```
