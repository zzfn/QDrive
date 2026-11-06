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
	"sync"
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
			return nil, withCode("USAGE", errors.New("用法: qdrive upload <本地路径> [网盘目录]"))
		}
		remote := "/"
		if len(args) == 3 {
			remote = args[2]
		}
		info, err := os.Lstat(args[1])
		if err != nil {
			return nil, withCode("UPLOAD_FAILED", fmt.Errorf("本地路径: %w", err))
		}
		client, err := newClient()
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			var progress func(string, int64, int64)
			if !jsonMode {
				progress = newTransferProgress(errOut, "上传").report
			}
			err = client.UploadDir(ctx, args[1], remote, progress)
		} else {
			var progress func(int64, int64)
			if !jsonMode {
				progress = progressPrinter(errOut, "上传")
			}
			err = client.UploadProgress(ctx, args[1], remote, progress)
		}
		if err != nil {
			return nil, manageError("UPLOAD_FAILED", fmt.Errorf("上传失败: %w", err))
		}
		fmt.Fprintln(out, "已上传:", displayName(args[1]))
		return map[string]any{"local": args[1], "remote_dir": remote, "is_dir": info.IsDir()}, nil
	case "download":
		positional, workers, workersSet, err := parseDownloadArgs(args[1:])
		if err != nil {
			return nil, withCode("USAGE", err)
		}
		remotePath := positional[0]
		local := path.Base(remotePath)
		if len(positional) == 2 {
			local = positional[1]
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
		remote, err := client.Stat(ctx, remotePath)
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
				dirProgress = newTransferProgress(errOut, "下载").report
			}
			err = client.DownloadDirWorkers(ctx, remotePath, local, workers, dirProgress)
		} else if workersSet {
			err = client.DownloadFileWorkers(ctx, remotePath, local, workers, progress)
		} else {
			err = client.DownloadFileProgress(ctx, remotePath, local, progress)
		}
		if err != nil {
			code := "DOWNLOAD_FAILED"
			if errors.Is(err, quark.ErrLocalExists) {
				code = "LOCAL_FILE_EXISTS"
			}
			return nil, withCode(code, fmt.Errorf("下载失败: %w", err))
		}
		fmt.Fprintln(out, "已下载:", local)
		return map[string]any{"remote": remotePath, "local": local, "is_dir": remote.IsDir}, nil
	default:
		return nil, fmt.Errorf("未知命令: %s", args[0])
	}
}

// parseDownloadArgs accepts a bounded worker count for directory or
// explicitly requested single-file parallel downloads.
func parseDownloadArgs(args []string) ([]string, int, bool, error) {
	workers := 4
	var positional []string
	var workersSet, endOptions bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !endOptions && arg == "--" {
			endOptions = true
			continue
		}
		if endOptions || (arg != "--workers" && !strings.HasPrefix(arg, "--workers=")) {
			positional = append(positional, arg)
			continue
		}
		if workersSet {
			return nil, 0, false, errors.New("--workers 只能指定一次")
		}
		var value string
		if arg == "--workers" {
			if i+1 >= len(args) {
				return nil, 0, false, errors.New("--workers 需要一个 1 至 16 的整数")
			}
			i++
			value = args[i]
		} else {
			value = strings.TrimPrefix(arg, "--workers=")
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 16 {
			return nil, 0, false, errors.New("--workers 必须是 1 至 16 的整数")
		}
		workers, workersSet = n, true
	}
	if len(positional) < 1 || len(positional) > 2 {
		return nil, 0, false, errors.New("用法: qdrive download <网盘路径> [本地路径] [--workers 1-16]")
	}
	return positional, workers, workersSet, nil
}

// progressPrinter reports single-file progress on stderr without flooding logs.
func progressPrinter(out io.Writer, label string) func(int64, int64) {
	p := newTransferProgress(out, label)
	p.single = true
	return func(done, total int64) { p.report("", done, total) }
}

const (
	progressInterval   = time.Second
	progressMinElapsed = 500 * time.Millisecond
	progressMaxEntries = 256 // 同时跟踪的文件上限，防止失败/未知长度文件残留导致内存增长
	progressMaxDone    = 64  // 最近完成的文件名，用于抑制重复的完成回调
)

// fileProgress 记录单个文件本次运行的观测状态。baseline 是首次观测（或回退后重新观测）
// 时的已传输字节，速率只按 baseline 之后的增量计算，因此续传偏移不会抬高速率。
type fileProgress struct {
	baseline  int64
	baseAt    time.Time
	done      int64
	updatedAt time.Time
}

// transferProgress 线程安全：目录传输的多个 worker 可并发回调。
type transferProgress struct {
	mu        sync.Mutex
	out       io.Writer
	label     string
	single    bool
	files     map[string]*fileProgress
	finished  []string
	lastPrint time.Time
	now       func() time.Time
}

func newTransferProgress(out io.Writer, label string) *transferProgress {
	return &transferProgress{out: out, label: label, files: make(map[string]*fileProgress), now: time.Now}
}

func (p *transferProgress) report(name string, done, total int64) {
	if p == nil || p.out == nil {
		return
	}
	if done < 0 {
		done = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	complete := total >= 0 && done >= total

	st, ok := p.files[name]
	if !ok {
		if complete && p.recentlyFinished(name) {
			return // 完成后的重复回调不再打印
		}
		if len(p.files) >= progressMaxEntries {
			p.evictOldest()
		}
		st = &fileProgress{baseline: done, baseAt: now}
		p.files[name] = st
	} else if done < st.done {
		// 临时失败重试导致进度回退：重建基线，避免负速率或伪造速率。
		st.baseline, st.baseAt = done, now
	}
	st.done, st.updatedAt = done, now

	if complete {
		delete(p.files, name)
		p.markFinished(name)
	} else if !p.lastPrint.IsZero() && now.Sub(p.lastPrint) < progressInterval {
		return
	}
	p.lastPrint = now
	fmt.Fprintln(p.out, p.format(name, st, total, now, complete))
}

func (p *transferProgress) format(name string, st *fileProgress, total int64, now time.Time, complete bool) string {
	var b strings.Builder
	b.WriteString(p.label)
	if !p.single {
		b.WriteString(" ")
		b.WriteString(displayName(name))
	}
	b.WriteString(": ")
	switch {
	case total > 0:
		pct := float64(st.done) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
		fmt.Fprintf(&b, "%5.1f%% %s / %s", pct, formatBytes(st.done), formatBytes(total))
	case total == 0:
		b.WriteString("100.0% 0 B / 0 B")
	default:
		fmt.Fprintf(&b, "%s / 未知大小", formatBytes(st.done))
	}
	elapsed := now.Sub(st.baseAt)
	delta := st.done - st.baseline
	speed := 0.0
	if delta > 0 && elapsed >= progressMinElapsed {
		speed = float64(delta) / elapsed.Seconds()
	}
	if speed > 0 {
		fmt.Fprintf(&b, "  %s", formatRate(speed))
	} else if !complete {
		b.WriteString("  -- /s")
	}
	if complete {
		b.WriteString("  已传输")
	} else if total > 0 && speed > 0 {
		b.WriteString("  剩余 ")
		b.WriteString(formatETA(float64(total-st.done) / speed))
	} else {
		b.WriteString("  剩余 --")
	}
	return b.String()
}

func (p *transferProgress) evictOldest() {
	var oldestName string
	var oldest time.Time
	first := true
	for n, st := range p.files {
		if first || st.updatedAt.Before(oldest) {
			oldestName, oldest, first = n, st.updatedAt, false
		}
	}
	if !first {
		delete(p.files, oldestName)
	}
}

func (p *transferProgress) markFinished(name string) {
	if len(p.finished) >= progressMaxDone {
		copy(p.finished, p.finished[1:])
		p.finished = p.finished[:len(p.finished)-1]
	}
	p.finished = append(p.finished, name)
}

func (p *transferProgress) recentlyFinished(name string) bool {
	for _, n := range p.finished {
		if n == name {
			return true
		}
	}
	return false
}

func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return formatScaledBytes(float64(n))
}

func formatRate(bytesPerSecond float64) string {
	if bytesPerSecond < 1024 {
		return fmt.Sprintf("%.1f B/s", bytesPerSecond)
	}
	return formatScaledBytes(bytesPerSecond) + "/s"
}

func formatScaledBytes(n float64) string {
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	v := n / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func formatETA(sec float64) string {
	if sec != sec || sec < 0 || sec > 99*24*3600 { // NaN、负数或过大
		return "--"
	}
	s := int64(sec + 0.5)
	h, m := s/3600, (s%3600)/60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s%60)
	}
	return fmt.Sprintf("%dm%02ds", m, s%60)
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
	fmt.Fprintln(out, "  qdrive upload <本地路径> [网盘目录]  上传文件或目录")
	fmt.Fprintln(out, "  qdrive download <网盘路径> [本地路径] [--workers 1-16]  下载文件或目录")
	fmt.Fprintln(out, "  qdrive share <网盘路径> <天数> <true|false>  创建分享")
	fmt.Fprintln(out, "  qdrive share-list                   查看分享")
	fmt.Fprintln(out, "  qdrive share-save --yes <链接> [提取码|--passcode-stdin] [目标目录]  转存分享")
	fmt.Fprintln(out, "  qdrive share-delete --yes <分享 ID>  取消分享")
	fmt.Fprintln(out, "选项:")
	fmt.Fprintln(out, "  --json  对非登录操作输出 JSON（写在命令前或紧跟命令后）")
}
