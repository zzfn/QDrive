//go:build windows

package credential

// Windows protection model
//
// Mode bits are meaningless on NTFS, so QDrive relies on access control lists.
// Every credential object (the qdrive directory, each temporary file and the
// final credentials.json) receives a *protected* DACL containing exactly one
// ACE: GENERIC_ALL for the SID of the user owning the current process token.
//
// Effects of the change:
//   - Inherited ACEs from %AppData% (e.g. SYSTEM, Administrators) are dropped
//     from these objects; other local users, services running as SYSTEM and
//     administrators can no longer open them without first taking ownership
//     (administrators still can via SeTakeOwnershipPrivilege/backup rights).
//   - Backup/AV/indexing tools that run as a different account lose read
//     access to the cookie file.
//   - Existing directories and credentials files are rewritten to this DACL on
//     Save/Load; any manually added ACEs on them are removed.
//   - Only the DACL is modified; owner, group and SACL are left untouched.
//   - Parent directories (%AppData%) are never changed.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// currentUserSID returns the user SID of the current process token.
func currentUserSID() (*windows.SID, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("query token user: %w", err)
	}
	// Copy so the SID does not alias the token-user buffer.
	return tu.User.Sid.Copy()
}

// privateDACL builds an ACL granting full control to the current user only.
// For directories the ACE is inheritable so new children are also private.
func privateDACL(isDir bool) (*windows.ACL, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if isDir {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	// nil merge ACL => the result contains only our entry.
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, nil)
}

// privateSA returns security attributes carrying a protected private DACL,
// used so the object is never visible with a looser ACL, even briefly.
func privateSA(isDir bool) (*windows.SecurityAttributes, error) {
	acl, err := privateDACL(isDir)
	if err != nil {
		return nil, err
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		return nil, err
	}
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

// mkdirPrivate creates dir with the private DACL applied atomically.
func mkdirPrivate(dir string) error {
	sa, err := privateSA(true)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(p, sa); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return os.ErrExist
		}
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

// createPrivateTemp exclusively creates a randomly named temp file in dir
// whose DACL is set at creation, i.e. before any byte is written.
func createPrivateTemp(dir string) (*os.File, error) {
	sa, err := privateSA(false)
	if err != nil {
		return nil, err
	}
	for range 10 {
		var rnd [12]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, ".credentials-"+hex.EncodeToString(rnd[:])+".tmp")
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, // no sharing while we hold it
			sa,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
			0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "create", Path: name, Err: err}
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, errors.New("too many temp file name collisions")
}

// restrictPath replaces the DACL of an existing directory or file with the
// protected private DACL (owner/group/SACL unchanged).
func restrictPath(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	acl, err := privateDACL(fi.IsDir())
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}
