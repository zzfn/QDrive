package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/skip2/go-qrcode"

	"github.com/zzfn/QDrive/internal/credential"
	"github.com/zzfn/QDrive/internal/qrlogin"
	"github.com/zzfn/QDrive/internal/quark"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		usage(out)
		return nil
	}
	switch args[0] {
	case "login":
		if len(args) == 2 && args[1] == "--qr" {
			cookie, err := qrlogin.Login(ctx, func(scanURL string) error {
				qr, err := qrcode.New(scanURL, qrcode.Medium)
				if err != nil {
					return errors.New("生成二维码失败")
				}
				if _, err := fmt.Fprintln(out, qr.ToSmallString(false)); err != nil {
					return errors.New("显示二维码失败")
				}
				_, err = fmt.Fprintln(errOut, "请用夸克 App 扫码登录，不要将二维码分享给他人。")
				return err
			})
			if err != nil {
				return fmt.Errorf("扫码登录失败: %w", err)
			}
			if err := credential.Save(cookie); err != nil {
				return fmt.Errorf("保存 Cookie: %w", err)
			}
			fmt.Fprintln(out, "登录信息已保存；请用 qdrive ls / 验证。")
			return nil
		}
		if len(args) != 1 {
			return errors.New("用法: qdrive login [--qr]")
		}
		fmt.Fprintln(errOut, "请从标准输入提供夸克网盘 Cookie；直接粘贴到终端时内容可能可见。")
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), 64*1024)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("读取 Cookie: %w", err)
			}
			return errors.New("未读取到 Cookie")
		}
		if err := credential.Save(strings.TrimSpace(scanner.Text())); err != nil {
			return fmt.Errorf("保存 Cookie: %w", err)
		}
		fmt.Fprintln(out, "Cookie 已保存；尚未验证有效性。")
		return nil
	case "logout":
		if len(args) != 1 {
			return errors.New("用法: qdrive logout")
		}
		if err := credential.Clear(); err != nil {
			return fmt.Errorf("清除登录信息: %w", err)
		}
		fmt.Fprintln(out, "本地登录信息已清除。")
		return nil
	case "ls":
		if len(args) > 2 {
			return errors.New("用法: qdrive ls [网盘目录]")
		}
		remote := "/"
		if len(args) == 2 {
			remote = args[1]
		}
		client, err := newClient()
		if err != nil {
			return err
		}
		files, err := client.List(ctx, remote)
		if err != nil {
			return err
		}
		for _, file := range files {
			kind := "文件"
			if file.IsDir {
				kind = "目录"
			}
			fmt.Fprintf(out, "%s\t%d\t%s\n", kind, file.Size, displayName(file.Name))
		}
		return nil
	case "upload":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("用法: qdrive upload <本地文件> [网盘目录]")
		}
		remote := "/"
		if len(args) == 3 {
			remote = args[2]
		}
		client, err := newClient()
		if err != nil {
			return err
		}
		if err := client.Upload(ctx, args[1], remote); err != nil {
			return fmt.Errorf("上传失败: %w", err)
		}
		fmt.Fprintln(out, "已上传:", args[1])
		return nil
	case "download":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("用法: qdrive download <网盘文件> [本地文件]")
		}
		local := path.Base(args[1])
		if len(args) == 3 {
			local = args[2]
		}
		if local == "." || local == "/" || local == "" || filepath.Base(local) == "." {
			return errors.New("无效的本地文件名")
		}
		client, err := newClient()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("创建本地文件: %w", err)
		}
		err = client.Download(ctx, args[1], file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			if removeErr := os.Remove(local); removeErr != nil {
				return fmt.Errorf("下载失败: %v; 清理未完成文件失败: %w", errors.Join(err, closeErr), removeErr)
			}
			return fmt.Errorf("下载失败: %w", errors.Join(err, closeErr))
		}
		fmt.Fprintln(out, "已下载:", local)
		return nil
	default:
		return fmt.Errorf("未知命令: %s", args[0])
	}
}

func displayName(name string) string {
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return strconv.QuoteToGraphic(name)
		}
	}
	return name
}

func newClient() (*quark.Client, error) {
	cookie, err := credential.Load()
	if err != nil {
		return nil, fmt.Errorf("读取登录信息（请先运行 qdrive login）: %w", err)
	}
	return quark.New(cookie), nil
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "QDrive - 夸克网盘命令行工具")
	fmt.Fprintln(out, "用法:")
	fmt.Fprintln(out, "  qdrive login                       从标准输入读取 Cookie 并保存在本地")
	fmt.Fprintln(out, "  qdrive login --qr                  使用夸克 App 扫码登录")
	fmt.Fprintln(out, "  qdrive logout                      清除本地登录信息")
	fmt.Fprintln(out, "  qdrive ls [网盘目录]               列出文件")
	fmt.Fprintln(out, "  qdrive upload <本地文件> [网盘目录]  上传单个文件")
	fmt.Fprintln(out, "  qdrive download <网盘文件> [本地文件]  下载单个文件")
}
