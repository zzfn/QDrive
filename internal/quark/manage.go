package quark

// File management: mkdir, rename, move, copy and delete.
//
// Protocol (as observed in the reference client; all POST JSON bodies sent
// through the same authenticated envelope as List):
//
//	mkdir   POST /1/clouddrive/file         {pdir_fid, file_name, dir_path:"", dir_init_lock:false}
//	rename  POST /1/clouddrive/file/rename  {fid, file_name}
//	move    POST /1/clouddrive/file/move    {action_type:1, exclude_fids:[], filelist:[fid], to_pdir_fid}
//	copy    POST /1/clouddrive/file/copy    {action_type:1, exclude_fids:[], filelist:[fid], to_pdir_fid}
//	delete  POST /1/clouddrive/file/delete  {action_type:1, exclude_fids:[], filelist:[fid]}
//
// The reference client treats an OK envelope as success. This client also
// re-checks the expected post-condition by listing the affected directories.
// If it cannot confirm the outcome, ErrUnconfirmed is returned. It does not
// assume that file-operation tasks use download-task status codes.
//
// Conflicts are never resolved silently: an existing entry with the target
// name in the target directory yields ErrExists before any request is sent.
// The root directory can never be the source of rename/move/delete.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	mkdirPath  = "/1/clouddrive/file"
	renamePath = "/1/clouddrive/file/rename"
	movePath   = "/1/clouddrive/file/move"
	copyPath   = "/1/clouddrive/file/copy"
	deletePath = "/1/clouddrive/file/delete"

	verifyAttempts = 5
	maxNameBytes   = 255 // conservative client-side limit; server limit unknown
)

var (
	// ErrRoot reports an attempt to rename, move or delete the root directory.
	ErrRoot = errors.New("quark: operation not allowed on root directory")
	// ErrInvalidName reports an unusable file name.
	ErrInvalidName = errors.New("quark: invalid file name")
	// ErrTypeMismatch reports that the source is not of the expected kind.
	ErrTypeMismatch = errors.New("quark: unexpected file type")
	// ErrInvalidTarget reports a move/copy into the source itself, one of
	// its descendants, or its current directory.
	ErrInvalidTarget = errors.New("quark: invalid target")
	// ErrNeedRecursive reports deleting a directory without Recursive.
	ErrNeedRecursive = errors.New("quark: directory delete requires recursive")
	// ErrUnconfirmed reports that a mutating request was sent but its
	// outcome could not be confirmed. The remote state may or may not have
	// changed; callers must not treat this as success or as a clean failure.
	ErrUnconfirmed = errors.New("quark: operation outcome unconfirmed")
)

// Kind constrains the type of a source path.
type Kind int

const (
	AnyKind  Kind = iota // file or directory
	FileKind             // regular file only
	DirKind              // directory only
)

func (k Kind) check(f File, p string) error {
	switch {
	case k == FileKind && f.IsDir:
		return fmt.Errorf("%w: %s is a directory", ErrTypeMismatch, p)
	case k == DirKind && !f.IsDir:
		return fmt.Errorf("%w: %s is not a directory", ErrTypeMismatch, p)
	}
	return nil
}

// ManageOptions are shared by Rename, Move and Copy.
type ManageOptions struct {
	// Expect restricts the source kind; AnyKind accepts both.
	Expect Kind
}

// DeleteOptions control Delete.
type DeleteOptions struct {
	// Expect restricts the target kind; AnyKind accepts both.
	Expect Kind
	// Recursive must be set to delete a directory (with all its content).
	Recursive bool
}

// Mkdir creates the directory at remotePath. The parent must already exist
// and must be a directory; an existing entry with that name yields ErrExists.
func (c *Client) Mkdir(ctx context.Context, remotePath string) (File, error) {
	parentPath, name, err := splitParent(remotePath)
	if err != nil {
		return File{}, err
	}
	parent, err := c.resolveDir(ctx, parentPath)
	if err != nil {
		return File{}, err
	}
	if err := c.ensureAbsent(ctx, parent.ID, name, joinPath(parentPath, name)); err != nil {
		return File{}, err
	}
	body, _ := json.Marshal(map[string]any{
		"pdir_fid":      parent.ID,
		"file_name":     name,
		"dir_path":      "",
		"dir_init_lock": false,
	})
	if _, err := c.mutate(ctx, "mkdir", mkdirPath, body); err != nil {
		return File{}, err
	}
	var out File
	err = c.verify(ctx, "mkdir", func() (bool, error) {
		m, err := c.children(ctx, parent.ID, name)
		if err != nil || len(m) != 1 || !m[0].IsDir {
			return false, err
		}
		out = m[0]
		return true, nil
	})
	return out, err
}

// Rename renames the entry at remotePath to newName within the same
// directory. It does not move entries; use Move for that.
func (c *Client) Rename(ctx context.Context, remotePath, newName string, opt ManageOptions) (File, error) {
	if err := validName(newName); err != nil {
		return File{}, err
	}
	parentPath, oldName, err := splitParent(remotePath)
	if err != nil {
		return File{}, err
	}
	if oldName == newName {
		return File{}, fmt.Errorf("%w: new name equals old name", ErrInvalidTarget)
	}
	parent, src, err := c.resolveSource(ctx, parentPath, oldName, opt.Expect)
	if err != nil {
		return File{}, err
	}
	if err := c.ensureAbsent(ctx, parent.ID, newName, joinPath(parentPath, newName)); err != nil {
		return File{}, err
	}
	body, _ := json.Marshal(map[string]any{"fid": src.ID, "file_name": newName})
	if _, err := c.mutate(ctx, "rename", renamePath, body); err != nil {
		return File{}, err
	}
	var out File
	err = c.verify(ctx, "rename", func() (bool, error) {
		entries, err := c.listDir(ctx, parent.ID)
		if err != nil {
			return false, err
		}
		var hit []File
		for _, e := range entries {
			if e.ID == src.ID && e.Name != newName {
				return false, nil // still under the old name
			}
			if e.Name == newName {
				hit = append(hit, e)
			}
		}
		if len(hit) != 1 || hit[0].IsDir != src.IsDir {
			return false, nil
		}
		out = hit[0]
		return true, nil
	})
	return out, err
}

// Move moves the entry at srcPath into the existing directory dstDir,
// keeping its name.
func (c *Client) Move(ctx context.Context, srcPath, dstDir string, opt ManageOptions) (File, error) {
	return c.transfer(ctx, "move", movePath, srcPath, dstDir, opt)
}

// Copy copies the entry at srcPath into the existing directory dstDir,
// keeping its name. dstDir may not be the source's own directory, since the
// copy would collide with the source.
func (c *Client) Copy(ctx context.Context, srcPath, dstDir string, opt ManageOptions) (File, error) {
	return c.transfer(ctx, "copy", copyPath, srcPath, dstDir, opt)
}

func (c *Client) transfer(ctx context.Context, op, apiPath, srcPath, dstDir string, opt ManageOptions) (File, error) {
	parentPath, name, err := splitParent(srcPath)
	if err != nil {
		return File{}, err
	}
	src := joinPath(parentPath, name)
	dstParts, err := splitPath(dstDir)
	if err != nil {
		return File{}, err
	}
	dst := "/" + strings.Join(dstParts, "/")
	if dst == src || strings.HasPrefix(dst, strings.TrimSuffix(src, "/")+"/") {
		return File{}, fmt.Errorf("%w: %s into %s", ErrInvalidTarget, src, dst)
	}
	parent, f, err := c.resolveSource(ctx, parentPath, name, opt.Expect)
	if err != nil {
		return File{}, err
	}
	target, err := c.resolveDir(ctx, dst)
	if err != nil {
		return File{}, err
	}
	if target.ID == parent.ID {
		return File{}, fmt.Errorf("%w: %s is already in %s", ErrInvalidTarget, src, dst)
	}
	if target.ID == f.ID {
		return File{}, fmt.Errorf("%w: %s into itself", ErrInvalidTarget, src)
	}
	if err := c.ensureAbsent(ctx, target.ID, name, joinPath(dst, name)); err != nil {
		return File{}, err
	}
	body, _ := json.Marshal(map[string]any{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{f.ID},
		"to_pdir_fid":  target.ID,
	})
	if _, err := c.mutate(ctx, op, apiPath, body); err != nil {
		return File{}, err
	}
	var out File
	err = c.verify(ctx, op, func() (bool, error) {
		m, err := c.children(ctx, target.ID, name)
		if err != nil || len(m) != 1 || m[0].IsDir != f.IsDir {
			return false, err
		}
		if op == "copy" {
			if m[0].ID == f.ID {
				return false, nil
			}
		} else {
			gone, err := c.absentID(ctx, parent.ID, f.ID)
			if err != nil || !gone {
				return false, err
			}
		}
		out = m[0]
		return true, nil
	})
	return out, err
}

// Delete deletes the entry at remotePath. Directories require
// opt.Recursive. Whether the server moves the entry to a recycle bin or
// removes it permanently is not known from the protocol reference.
func (c *Client) Delete(ctx context.Context, remotePath string, opt DeleteOptions) error {
	parentPath, name, err := splitParent(remotePath)
	if err != nil {
		return err
	}
	parent, f, err := c.resolveSource(ctx, parentPath, name, opt.Expect)
	if err != nil {
		return err
	}
	if f.IsDir && !opt.Recursive {
		return fmt.Errorf("%w: %s", ErrNeedRecursive, joinPath(parentPath, name))
	}
	body, _ := json.Marshal(map[string]any{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{f.ID},
	})
	if _, err := c.mutate(ctx, "delete", deletePath, body); err != nil {
		return err
	}
	return c.verify(ctx, "delete", func() (bool, error) {
		return c.absentID(ctx, parent.ID, f.ID)
	})
}

// splitParent splits a non-root path into its parent path and base name.
func splitParent(p string) (string, string, error) {
	parts, err := splitPath(p)
	if err != nil {
		return "", "", err
	}
	if len(parts) == 0 {
		return "", "", ErrRoot
	}
	name := parts[len(parts)-1]
	if err := validName(name); err != nil {
		return "", "", err
	}
	return "/" + strings.Join(parts[:len(parts)-1], "/"), name, nil
}

func joinPath(dir, name string) string {
	return strings.TrimSuffix(dir, "/") + "/" + name
}

func validName(n string) error {
	switch {
	case strings.TrimSpace(n) == "", n == ".", n == "..":
		return fmt.Errorf("%w: %q", ErrInvalidName, n)
	case len(n) > maxNameBytes:
		return fmt.Errorf("%w: name too long", ErrInvalidName)
	case strings.ContainsAny(n, "/\\"):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidName, n)
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains control characters", ErrInvalidName, n)
		}
	}
	return nil
}

func (c *Client) resolveDir(ctx context.Context, p string) (File, error) {
	d, err := c.resolve(ctx, p)
	if err != nil {
		return File{}, err
	}
	if !d.IsDir {
		return File{}, fmt.Errorf("%w: %s", ErrNotDir, p)
	}
	return d, nil
}

// resolveSource resolves parentPath and its unique child name, enforcing
// the expected kind. It never returns the root as the source.
func (c *Client) resolveSource(ctx context.Context, parentPath, name string, k Kind) (File, File, error) {
	parent, err := c.resolveDir(ctx, parentPath)
	if err != nil {
		return File{}, File{}, err
	}
	p := joinPath(parentPath, name)
	m, err := c.children(ctx, parent.ID, name)
	if err != nil {
		return File{}, File{}, err
	}
	switch {
	case len(m) == 0:
		return File{}, File{}, fmt.Errorf("%w: %s", ErrNotFound, p)
	case len(m) > 1:
		return File{}, File{}, fmt.Errorf("%w: %s", ErrAmbiguous, p)
	}
	if m[0].ID == rootID {
		return File{}, File{}, ErrRoot
	}
	if err := k.check(m[0], p); err != nil {
		return File{}, File{}, err
	}
	return parent, m[0], nil
}

func (c *Client) children(ctx context.Context, dirID, name string) ([]File, error) {
	entries, err := c.listDir(ctx, dirID)
	if err != nil {
		return nil, err
	}
	var out []File
	for _, e := range entries {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out, nil
}

func (c *Client) ensureAbsent(ctx context.Context, dirID, name, p string) error {
	m, err := c.children(ctx, dirID, name)
	if err != nil {
		return err
	}
	if len(m) > 0 {
		return fmt.Errorf("%w: %s", ErrExists, p)
	}
	return nil
}

func (c *Client) absentID(ctx context.Context, dirID, fid string) (bool, error) {
	entries, err := c.listDir(ctx, dirID)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.ID == fid {
			return false, nil
		}
	}
	return true, nil
}

// mutate sends a mutating request. An explicit API rejection is returned as
// is (the server refused, nothing changed as far as we know). Any other
// failure after the request may have reached the server is reported as
// ErrUnconfirmed. Confirmation is based on the subsequent directory listing;
// task status semantics for file operations have not been verified.
func (c *Client) mutate(ctx context.Context, op, apiPath string, body []byte) (json.RawMessage, error) {
	return c.mutateWithQuery(ctx, op, apiPath, nil, body)
}

func (c *Client) mutateWithQuery(ctx context.Context, op, apiPath string, q url.Values, body []byte) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.call(ctx, op, http.MethodPost, apiPath, q, body, &raw)
	var ae *APIError
	switch {
	case err == nil:
	case errors.Is(err, errMissingData):
		return nil, nil // OK envelope without data; rely on verification
	case errors.As(err, &ae) && ae.Status < 500:
		return nil, err
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%w: %s: %v", ErrUnconfirmed, op, ctx.Err())
	default:
		return nil, fmt.Errorf("%w: %s: %v", ErrUnconfirmed, op, err)
	}
	return raw, nil
}

// verify re-checks the post-condition a few times to tolerate listing lag.
func (c *Client) verify(ctx context.Context, op string, check func() (bool, error)) error {
	interval, _ := c.pollParams()
	var lastErr error
	for i := 0; i < verifyAttempts; i++ {
		if i > 0 {
			if err := sleepCtx(ctx, interval); err != nil {
				return fmt.Errorf("%w: %s: %v", ErrUnconfirmed, op, err)
			}
		}
		ok, err := check()
		if ok && err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return fmt.Errorf("%w: %s: verification failed: %v", ErrUnconfirmed, op, lastErr)
	}
	return fmt.Errorf("%w: %s: expected result not observed", ErrUnconfirmed, op)
}

func (c *Client) pollParams() (time.Duration, int) {
	interval, limit := c.pollInterval, c.maxPolls
	if interval <= 0 {
		interval = defaultPollInterval
	}
	if limit <= 0 {
		limit = defaultMaxPolls
	}
	return interval, limit
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
