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

// assertPrivateDACL checks that all ACEs grant only the current user access.
// Windows may represent inherited permissions with multiple ACEs.
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
	if dacl.AceCount == 0 {
		t.Fatalf("%s: empty DACL", path)
	}
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF
	var effective, fileInheritance, directoryInheritance bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("%s: ACE %d type = %d", path, i, ace.Header.AceType)
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !aceSID.Equals(me) {
			t.Errorf("%s: ACE %d SID = %s, want %s (flags=%#x mask=%#x)", path, i, aceSID, me, ace.Header.AceFlags, ace.Mask)
		}
		full := ace.Mask&fileAllAccess == fileAllAccess || ace.Mask&windows.GENERIC_ALL != 0
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 && full {
			effective = true
		}
		if ace.Header.AceFlags&windows.OBJECT_INHERIT_ACE != 0 && full {
			fileInheritance = true
		}
		if ace.Header.AceFlags&windows.CONTAINER_INHERIT_ACE != 0 && full {
			directoryInheritance = true
		}
	}
	if !effective {
		t.Errorf("%s: no effective full-access ACE for current user", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() && (!fileInheritance || !directoryInheritance) {
		t.Errorf("%s: missing inherited full access (file=%v directory=%v)", path, fileInheritance, directoryInheritance)
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
