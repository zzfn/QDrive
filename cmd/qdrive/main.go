package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/skip2/go-qrcode"

	"github.com/zzfn/QDrive/internal/credential"
	"github.com/zzfn/QDrive/internal/qrlogin"
	"github.com/zzfn/QDrive/internal/quark"
)

// version is set by the release workflow; source builds use dev.
var version = "dev"

// 退出码：0 成功；1 任何失败（JSON 模式下细分原因见 code 字段）。
const (
	exitSuccess = 0
	exitError   = 1
)

// cliResult 与 kuake_cli 的 JSON 约定一致：success/code/message/data。
type cliResult struct {
	Success bool           `json:"success"`
	Code    string         `json:"code,omitempty"`
	Message string         `json:"message,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// jsonFile 是 ls 的单个条目。name 为原始名称（无效 UTF-8 会被替换为 U+FFFD），
// display_name 与人类可读输出一致，含非打印字符时为加引号的转义形式。
type jsonFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	IsDir       bool   `json:"is_dir"`
	Size        int64  `json:"size"`
}

// codedError 携带 JSON 错误码；Error() 保持原有中文消息，非 JSON 模式输出不变。
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

func withCode(code string, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	args, jsonMode := extractJSONFlag(os.Args[1:])
	if jsonMode {
		os.Exit(runJSON(ctx, args, os.Stdin, os.Stdout, os.Stderr))
	}
	if err := run(ctx, args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(exitError)
	}
}

// extractJSONFlag 只识别两个位置的 --json：命令前（qdrive --json ls）或紧跟命令后
// （qdrive ls --json /）。其余位置的 "--json" 仍按路径参数处理，避免改变既有路径语义。
func extractJSONFlag(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "--json" {
		return args[1:], true
	}
	if len(args) > 1 && args[1] == "--json" {
		rest := append([]string{args[0]}, args[2:]...)
		return rest, true
	}
	return args, false
}

// runJSON 执行命令并向 stdout 写出且仅写出一个 JSON 对象，返回退出码。
func runJSON(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	var result *cliResult
	if len(args) == 0 {
		result = errorResult(withCode("USAGE", errors.New("缺少命令；--json 支持非登录操作")))
	} else {
		switch args[0] {
		case "ls", "upload", "download", "logout", "create", "rename", "move", "copy", "delete", "version", "--version", "user", "share", "share-list", "share-save", "share-delete", "info":
			data, err := runCommand(ctx, args, in, io.Discard, io.Discard, true)
			if err != nil {
				result = errorResult(err)
			} else {
				result = &cliResult{Success: true, Code: "OK", Message: "操作成功", Data: data}
			}
		default:
			// login（含 --qr）与 help 不支持 JSON：前者需交互输入凭据或展示二维码，
			// 均不应进入机器可读输出。
			result = errorResult(withCode("UNSUPPORTED", fmt.Errorf("命令 %s 不支持 --json", args[0])))
		}
	}
	if err := writeJSON(out, result); err != nil {
		fmt.Fprintln(errOut, "错误: 写出 JSON 失败:", err)
		return exitError
	}
	if !result.Success {
		return exitError
	}
	return exitSuccess
}

func errorResult(err error) *cliResult {
	code := "ERROR"
	var ce *codedError
	if errors.As(err, &ce) {
		code = ce.code
	}
	if errors.Is(err, context.Canceled) {
		code = "CANCELED"
	}
	return &cliResult{Success: false, Code: code, Message: err.Error()}
}

// writeJSON 编码到缓冲区后一次性写出；encoding/json 会把控制字符转义为 \u00XX，
// 并将 U+2028/U+2029 转义，保证输出为单个合法 JSON 文档。
func writeJSON(out io.Writer, result *cliResult) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	_, err := runCommand(ctx, args, in, out, errOut, false)
	return err
}

func runCommand(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, jsonMode bool) (map[string]any, error) {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		usage(out)
		return nil, nil
	}
	switch args[0] {
	case "version", "--version":
		if len(args) != 1 {
			return nil, withCode("USAGE", errors.New("用法: qdrive version"))
		}
		fmt.Fprintln(out, version)
		return map[string]any{"version": version}, nil
	case "user":
		if len(args) != 1 {
			return nil, withCode("USAGE", errors.New("用法: qdrive user"))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		account, err := client.AccountInfo(ctx)
		if err != nil {
			return nil, withCode("USER_FAILED", err)
		}
		capacity, err := client.Capacity(ctx)
		if err != nil {
			return nil, withCode("USER_FAILED", err)
		}
		fmt.Fprintf(out, "用户: %s\n容量: %d / %d 字节\n会员: %s\n", displayName(account.Nickname), capacity.Used, capacity.Total, displayName(capacity.MemberType))
		return map[string]any{"nickname": account.Nickname, "used": capacity.Used, "total": capacity.Total, "free": capacity.Free(), "member_type": capacity.MemberType, "vip_expires_at": capacity.VIPExpAt}, nil
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
				return nil, fmt.Errorf("扫码登录失败: %w", err)
			}
			if err := credential.Save(cookie); err != nil {
				return nil, fmt.Errorf("保存 Cookie: %w", err)
			}
			fmt.Fprintln(out, "登录信息已保存；请用 qdrive ls / 验证。")
			return nil, nil
		}
		if len(args) != 1 {
			return nil, errors.New("用法: qdrive login [--qr]")
		}
		fmt.Fprintln(errOut, "请从标准输入提供夸克网盘 Cookie；直接粘贴到终端时内容可能可见。")
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), 64*1024)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("读取 Cookie: %w", err)
			}
			return nil, errors.New("未读取到 Cookie")
		}
		if err := credential.Save(strings.TrimSpace(scanner.Text())); err != nil {
			return nil, fmt.Errorf("保存 Cookie: %w", err)
		}
		fmt.Fprintln(out, "Cookie 已保存；尚未验证有效性。")
		return nil, nil
	case "logout":
		if len(args) != 1 {
			return nil, withCode("USAGE", errors.New("用法: qdrive logout"))
		}
		if err := credential.Clear(); err != nil {
			return nil, withCode("LOGOUT_FAILED", fmt.Errorf("清除登录信息: %w", err))
		}
		fmt.Fprintln(out, "本地登录信息已清除。")
		return map[string]any{"cleared": true}, nil
	case "ls":
		if len(args) > 2 {
			return nil, withCode("USAGE", errors.New("用法: qdrive ls [网盘目录]"))
		}
		remote := "/"
		if len(args) == 2 {
			remote = args[1]
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		files, err := client.List(ctx, remote)
		if err != nil {
			return nil, withCode("LIST_FAILED", err)
		}
		if jsonMode {
			entries := make([]jsonFile, 0, len(files))
			for _, file := range files {
				entries = append(entries, jsonFile{ID: file.ID, Name: file.Name, DisplayName: displayName(file.Name), IsDir: file.IsDir, Size: file.Size})
			}
			return map[string]any{"path": remote, "count": len(entries), "files": entries}, nil
		}
		for _, file := range files {
			kind := "文件"
			if file.IsDir {
				kind = "目录"
			}
			fmt.Fprintf(out, "%s\t%d\t%s\n", kind, file.Size, displayName(file.Name))
		}
		return nil, nil
	case "info":
		if len(args) != 2 {
			return nil, withCode("USAGE", errors.New("用法: qdrive info <网盘路径>"))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		file, err := client.Stat(ctx, args[1])
		if err != nil {
			return nil, withCode("INFO_FAILED", err)
		}
		kind := "文件"
		if file.IsDir {
			kind = "目录"
		}
		fmt.Fprintf(out, "%s\t%d\t%s\n", kind, file.Size, displayName(file.Name))
		return map[string]any{"path": args[1], "id": file.ID, "name": file.Name, "is_dir": file.IsDir, "size": file.Size}, nil
	case "create":
		if len(args) != 3 || args[1] == "" || args[1] == "." || args[1] == ".." || strings.ContainsAny(args[1], "/\\") {
			return nil, withCode("USAGE", errors.New("用法: qdrive create <目录名> <父目录>"))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		remote := strings.TrimRight(args[2], "/") + "/" + args[1]
		file, err := client.Mkdir(ctx, remote)
		if err != nil {
			return nil, manageError("CREATE_FAILED", err)
		}
		fmt.Fprintln(out, "已创建目录:", displayName(remote))
		return map[string]any{"path": remote, "id": file.ID}, nil
	case "rename", "move", "copy":
		if len(args) != 3 {
			return nil, withCode("USAGE", fmt.Errorf("用法: qdrive %s <源路径> <目标名称或目录>", args[0]))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		var file quark.File
		switch args[0] {
		case "rename":
			file, err = client.Rename(ctx, args[1], args[2], quark.ManageOptions{})
		case "move":
			file, err = client.Move(ctx, args[1], args[2], quark.ManageOptions{})
		case "copy":
			file, err = client.Copy(ctx, args[1], args[2], quark.ManageOptions{})
		}
		if err != nil {
			return nil, manageError(strings.ToUpper(args[0])+"_FAILED", err)
		}
		fmt.Fprintf(out, "%s 完成: %s\n", args[0], displayName(file.Name))
		return map[string]any{"source": args[1], "destination": args[2], "id": file.ID, "name": file.Name}, nil
	case "delete":
		yes, recursive, remote := false, false, ""
		for _, arg := range args[1:] {
			switch arg {
			case "--yes":
				yes = true
			case "--recursive":
				recursive = true
			default:
				if remote != "" {
					return nil, withCode("USAGE", errors.New("用法: qdrive delete --yes [--recursive] <网盘路径>"))
				}
				remote = arg
			}
		}
		if !yes || remote == "" {
			return nil, withCode("USAGE", errors.New("删除需要 --yes 确认；删除目录还需要 --recursive"))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		if err := client.Delete(ctx, remote, quark.DeleteOptions{Recursive: recursive}); err != nil {
			return nil, manageError("DELETE_FAILED", err)
		}
		fmt.Fprintln(out, "已删除:", displayName(remote))
		return map[string]any{"path": remote, "recursive": recursive}, nil
	case "share":
		if len(args) != 4 || (args[3] != "true" && args[3] != "false") {
			return nil, withCode("USAGE", errors.New("用法: qdrive share <网盘路径> <有效天数:0|1|7|30> <是否生成提取码:true|false>"))
		}
		days, err := strconv.Atoi(args[2])
		if err != nil || (days != 0 && days != 1 && days != 7 && days != 30) {
			return nil, withCode("USAGE", errors.New("有效天数必须为 0、1、7 或 30"))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		link, err := client.CreateShare(ctx, args[1], quark.ShareOptions{Expiry: quark.ShareExpiry(days), Passcode: args[3] == "true"})
		if err != nil {
			return nil, manageError("SHARE_FAILED", err)
		}
		fmt.Fprintln(out, "分享链接:", link.ShareURL)
		if link.Passcode != "" {
			fmt.Fprintln(out, "提取码:", link.Passcode)
		}
		return map[string]any{"share_id": link.ShareID, "url": link.ShareURL, "passcode": link.Passcode, "expires_at": link.ExpiresAt}, nil
	case "share-list":
		if len(args) != 1 {
			return nil, withCode("USAGE", errors.New("用法: qdrive share-list"))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		shares, err := client.ListAllMyShares(ctx)
		if err != nil {
			return nil, withCode("SHARE_LIST_FAILED", err)
		}
		items := make([]map[string]any, 0, len(shares))
		for _, share := range shares {
			fmt.Fprintf(out, "%s\t%s\n", displayName(share.ShareID), displayName(share.FirstFileName))
			items = append(items, map[string]any{"share_id": share.ShareID, "file_id": share.FirstFileID, "name": share.FirstFileName})
		}
		return map[string]any{"count": len(items), "shares": items}, nil
	case "share-save":
		if len(args) < 3 || len(args) > 5 || args[1] != "--yes" {
			return nil, withCode("USAGE", errors.New("转存的同名冲突行为由服务端决定；请确认后使用: qdrive share-save --yes <分享链接> [提取码|--passcode-stdin] [目标目录]"))
		}
		ref, err := quark.ParseShareLink(args[2])
		if err != nil {
			return nil, withCode("USAGE", err)
		}
		if len(args) >= 4 && args[3] == "--passcode-stdin" {
			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 1024), 4096)
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return nil, withCode("USAGE", fmt.Errorf("读取提取码: %w", err))
				}
				return nil, withCode("USAGE", errors.New("未读取到提取码"))
			}
			ref.Passcode = strings.TrimSpace(scanner.Text())
		} else if len(args) >= 4 && args[3] != "" {
			ref.Passcode = args[3]
		}
		dest := "/"
		if len(args) == 5 {
			dest = args[4]
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		result, err := client.SaveShare(ctx, ref, dest, quark.SaveOptions{})
		if err != nil {
			return nil, manageError("SHARE_SAVE_FAILED", err)
		}
		fmt.Fprintln(out, "转存请求已受理，完成状态尚未确认；请检查目标目录。")
		return map[string]any{"accepted": result.Accepted, "task_id": result.TaskID, "destination": dest, "completed": false}, nil
	case "share-delete":
		if len(args) != 3 || args[1] != "--yes" {
			return nil, withCode("USAGE", errors.New("用法: qdrive share-delete --yes <分享 ID>"))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		if err := client.DeleteShares(ctx, []string{args[2]}); err != nil {
			return nil, manageError("SHARE_DELETE_FAILED", err)
		}
		fmt.Fprintln(out, "已取消分享:", displayName(args[2]))
		return map[string]any{"share_id": args[2]}, nil
	case "upload":
		if len(args) < 2 || len(args) > 3 {
			return nil, withCode("USAGE", errors.New("用法: qdrive upload <本地文件> [网盘目录]"))
		}
		remote := "/"
		if len(args) == 3 {
			remote = args[2]
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		var progress func(int64, int64)
		if !jsonMode {
			progress = progressPrinter(errOut, "上传")
		}
		if err := client.UploadProgress(ctx, args[1], remote, progress); err != nil {
			return nil, withCode("UPLOAD_FAILED", fmt.Errorf("上传失败: %w", err))
		}
		fmt.Fprintln(out, "已上传:", args[1])
		return map[string]any{"local": args[1], "remote_dir": remote}, nil
	case "download":
		if len(args) < 2 || len(args) > 3 {
			return nil, withCode("USAGE", errors.New("用法: qdrive download <网盘文件> [本地文件]"))
		}
		local := path.Base(args[1])
		if len(args) == 3 {
			local = args[2]
		}
		if local == "." || local == "/" || local == "" || filepath.Base(local) == "." {
			return nil, withCode("USAGE", errors.New("无效的本地文件名"))
		}
		if info, statErr := os.Lstat(local); statErr == nil && !info.IsDir() {
			return nil, withCode("LOCAL_FILE_EXISTS", fmt.Errorf("%w: %s", quark.ErrLocalExists, local))
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, withCode("LOCAL_FILE_ERROR", statErr)
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		remote, err := client.Stat(ctx, args[1])
		if err != nil {
			return nil, withCode("DOWNLOAD_FAILED", err)
		}
		var progress func(int64, int64)
		if !jsonMode {
			progress = progressPrinter(errOut, "下载")
		}
		if remote.IsDir {
			var dirProgress func(string, int64, int64)
			if !jsonMode {
				var last time.Time
				dirProgress = func(name string, done, total int64) {
					now := time.Now()
					if done != total && !last.IsZero() && now.Sub(last) < time.Second {
						return
					}
					last = now
					fmt.Fprintf(errOut, "下载 %s: %d / %d 字节\n", displayName(name), done, total)
				}
			}
			err = client.DownloadDir(ctx, args[1], local, dirProgress)
		} else {
			err = client.DownloadFileProgress(ctx, args[1], local, progress)
		}
		if err != nil {
			code := "DOWNLOAD_FAILED"
			if errors.Is(err, quark.ErrLocalExists) {
				code = "LOCAL_FILE_EXISTS"
			}
			return nil, withCode(code, fmt.Errorf("下载失败: %w", err))
		}
		fmt.Fprintln(out, "已下载:", local)
		return map[string]any{"remote": args[1], "local": local, "is_dir": remote.IsDir}, nil
	default:
		return nil, fmt.Errorf("未知命令: %s", args[0])
	}
}

// progressPrinter reports byte counts on stderr without flooding redirected logs.
func progressPrinter(out io.Writer, label string) func(int64, int64) {
	var last time.Time
	return func(done, total int64) {
		now := time.Now()
		if done != total && !last.IsZero() && now.Sub(last) < time.Second {
			return
		}
		last = now
		fmt.Fprintf(out, "%s: %d / %d 字节\n", label, done, total)
	}
}

func manageError(code string, err error) error {
	if errors.Is(err, quark.ErrUnconfirmed) {
		return withCode("OUTCOME_UNCONFIRMED", err)
	}
	return withCode(code, err)
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
		return nil, withCode("NOT_LOGGED_IN", fmt.Errorf("读取登录信息（请先运行 qdrive login）: %w", err))
	}
	return quark.New(cookie), nil
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "QDrive - 夸克网盘命令行工具")
	fmt.Fprintln(out, "用法:")
	fmt.Fprintln(out, "  qdrive version                     查看版本")
	fmt.Fprintln(out, "  qdrive user                        查看账号及容量")
	fmt.Fprintln(out, "  qdrive login                       从标准输入读取 Cookie 并保存在本地")
	fmt.Fprintln(out, "  qdrive login --qr                  使用夸克 App 扫码登录")
	fmt.Fprintln(out, "  qdrive logout                      清除本地登录信息")
	fmt.Fprintln(out, "  qdrive ls [网盘目录]               列出文件")
	fmt.Fprintln(out, "  qdrive info <网盘路径>             查看文件或目录信息")
	fmt.Fprintln(out, "  qdrive create <目录名> <父目录>     创建目录")
	fmt.Fprintln(out, "  qdrive rename <网盘路径> <新名称>   重命名")
	fmt.Fprintln(out, "  qdrive move <网盘路径> <目标目录>   移动")
	fmt.Fprintln(out, "  qdrive copy <网盘路径> <目标目录>   复制")
	fmt.Fprintln(out, "  qdrive delete --yes [--recursive] <网盘路径>  删除")
	fmt.Fprintln(out, "  qdrive upload <本地文件> [网盘目录]  上传单个文件")
	fmt.Fprintln(out, "  qdrive download <网盘路径> [本地路径]  下载文件或目录")
	fmt.Fprintln(out, "  qdrive share <网盘路径> <天数> <true|false>  创建分享")
	fmt.Fprintln(out, "  qdrive share-list                   查看分享")
	fmt.Fprintln(out, "  qdrive share-save --yes <链接> [提取码|--passcode-stdin] [目标目录]  转存分享")
	fmt.Fprintln(out, "  qdrive share-delete --yes <分享 ID>  取消分享")
	fmt.Fprintln(out, "选项:")
	fmt.Fprintln(out, "  --json  对非登录操作输出 JSON（写在命令前或紧跟命令后）")
}
