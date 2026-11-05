//go:build windows

package credential

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fakeCookie = "test-cookie=not-a-real-value"

// assertPrivateDACL reads the DACL via GetNamedSecurityInfo and checks it is
// protected and holds exactly one allow ACE for the current user.
func assertPrivateDACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if ctrl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s: DACL not protected (inheritance enabled)", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("%s: no DACL: %v", path, err)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("%s: AceCount = %d, want 1", path, dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Errorf("%s: ACE type = %d", path, ace.Header.AceType)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !aceSID.Equals(me) {
		t.Errorf("%s: ACE SID = %s, want %s", path, aceSID, me)
	}
	// GENERIC_ALL is mapped to FILE_ALL_ACCESS (0x1F01FF) when stored.
	const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF
	if ace.Mask&fileAllAccess != fileAllAccess {
		t.Errorf("%s: mask = %#x", path, ace.Mask)
	}
}

func TestWindowsSaveAppliesPrivateDACL(t *testing.T) {
	dir := isolate(t)
	if err := Save(fakeCookie); err != nil {
		t.Fatal(err)
	}
	assertPrivateDACL(t, dir)
	assertPrivateDACL(t, filepath.Join(dir, fileName))
}

func TestWindowsCreatePrivateTemp(t *testing.T) {
	dir := isolate(t)
	if err := ensureDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := createPrivateTemp(dir)
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	// Checked before any data is written.
	assertPrivateDACL(t, name)
}

func TestWindowsHardensExistingCredential(t *testing.T) {
	dir := isolate(t)
	// Pre-existing dir/file created with default (inherited) ACLs.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte(`{"cookie":"`+fakeCookie+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil || got != fakeCookie {
		t.Fatalf("Load = %q, %v", got, err)
	}
	assertPrivateDACL(t, path)
	if err := Save(fakeCookie); err != nil {
		t.Fatal(err)
	}
	assertPrivateDACL(t, dir)
	assertPrivateDACL(t, path)
}
