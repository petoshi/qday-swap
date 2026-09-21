package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/petoshi/qday-swap/internal/walletroot"
)

func TestCreateOpenAndWrongPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "master.key")
	root := walletroot.Root{1, 2, 3, 4, 5}
	if err := Create(path, "correct horse battery staple", root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("keystore mode = %o", info.Mode().Perm())
	}
	opened, err := Open(path, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if opened != root {
		t.Fatal("opened root differs from stored root")
	}
	if _, err := Open(path, "wrong password"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password error = %v", err)
	}
	if err := Create(path, "another safe password", walletroot.Root{9}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create error = %v", err)
	}
	opened, err = Open(path, "correct horse battery staple")
	if err != nil || opened != root {
		t.Fatalf("second create altered keystore: root=%x err=%v", opened, err)
	}
}

func TestRejectsWeakPasswordAndHostileParameters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := Create(path, "short", walletroot.Root{}); err == nil {
		t.Fatal("weak password accepted")
	}
	encoded := []byte(`{"version":1,"kdf":{"name":"argon2id","time":3,"memoryKiB":4294967295,"parallelism":2,"salt":"00000000000000000000000000000000"},"cipher":"xchacha20-poly1305","nonce":"000000000000000000000000000000000000000000000000","ciphertext":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "correct horse battery staple"); err == nil {
		t.Fatal("hostile KDF parameters accepted")
	}
}

func TestTamperingFailsAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := Create(path, "correct horse battery staple", walletroot.Root{7}); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := len(encoded) - 1; index >= 0; index-- {
		if encoded[index] >= 'A' && encoded[index] <= 'Z' {
			encoded[index] = 'a'
			break
		}
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "correct horse battery staple"); err == nil {
		t.Fatal("tampered keystore opened")
	}
}
