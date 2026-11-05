package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestDisplayName(t *testing.T) {
	if got := displayName("文件.txt"); got != "文件.txt" {
		t.Fatalf("normal name changed: %q", got)
	}
	if got := displayName("evil\x1b[2J\n"); strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("control characters remain: %q", got)
	}
}

func TestHelpAndUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "qdrive ls") {
		t.Fatal("help does not list ls")
	}
	if !strings.Contains(out.String(), "qdrive upload") {
		t.Fatal("help does not list upload")
	}
	if err := run(context.Background(), []string{"unknown"}, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("unknown command should fail")
	}
	if err := run(context.Background(), []string{"upload"}, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("upload without a file should fail")
	}
}

func TestLoginAndLogout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	var out, errOut bytes.Buffer
	const cookie = "__pus=example-secret"
	if err := run(context.Background(), []string{"login"}, strings.NewReader(cookie+"\n"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String()+errOut.String(), cookie) {
		t.Fatal("login printed the cookie")
	}
	if _, err := newClient(); err != nil {
		t.Fatalf("newClient after login: %v", err)
	}
	if err := run(context.Background(), []string{"logout"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if _, err := newClient(); err == nil {
		t.Fatal("newClient after logout should fail")
	}
}

func TestDownloadDoesNotOverwrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"login"}, strings.NewReader("__pus=example\n"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	file := home + "/existing"
	if err := os.WriteFile(file, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"download", "/file", file}, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("download should refuse to overwrite")
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "existing" {
		t.Fatalf("existing file changed: %q, %v", got, err)
	}
}
