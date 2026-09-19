package walletd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/petoshi/qday-swap/internal/walletroot"
)

func TestManagerInitializesWithoutSecretArgumentsOrFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix only")
	}
	directory := t.TempDir()
	script := filepath.Join(directory, "fake-walletd")
	contents := `#!/bin/sh
set -eu
[ "$1" = "init" ]
shift
data=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -data) data="$2"; shift 2 ;;
    -secret-stdin) shift ;;
    *) exit 20 ;;
  esac
done
[ -n "$data" ]
case "$*" in *correct*|*abandon*) exit 21;; esac
input=$(cat)
case "$input" in
  *'"password":"correct horse battery staple"'*'"phrase":"'*) ;;
  *) exit 22 ;;
esac
mkdir -p "$data"
printf key > "$data/master.key"
`
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{Binary: script, DataDir: filepath.Join(directory, "walletd")})
	if err != nil {
		t.Fatal(err)
	}
	root := walletroot.Root{1, 2, 3, 4}
	if err := manager.Initialize(context.Background(), root, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if !manager.Initialized() {
		t.Fatal("walletd was not initialized")
	}
	entries, err := os.ReadDir(manager.config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "master.key" {
		t.Fatalf("unexpected files after initialization: %#v", entries)
	}
}

func TestTokenValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := readToken(path); err != nil || token != testToken {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("f", 63)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(path); err == nil {
		t.Fatal("accepted malformed token")
	}
}
