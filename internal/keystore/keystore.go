// Package keystore encrypts the recovery root used by the local QDAY Swap
// application. Chain and relay keys are derived only after a successful
// unlock; the plaintext root is never written to disk.
package keystore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/petoshi/qday-swap/internal/walletroot"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	formatVersion = 1
	keyLength     = 32
	maxFileSize   = 16 << 10
)

var (
	authenticatedData = []byte("QDAY_SWAP_KEYSTORE_V1")
	ErrWrongPassword  = errors.New("wrong password or damaged keystore")
)

type parameters struct {
	Name        string `json:"name"`
	Time        uint32 `json:"time"`
	MemoryKiB   uint32 `json:"memoryKiB"`
	Parallelism uint8  `json:"parallelism"`
	Salt        string `json:"salt"`
}

type file struct {
	Version    int        `json:"version"`
	KDF        parameters `json:"kdf"`
	Cipher     string     `json:"cipher"`
	Nonce      string     `json:"nonce"`
	Ciphertext string     `json:"ciphertext"`
}

func defaultParameters() parameters {
	return parameters{Name: "argon2id", Time: 3, MemoryKiB: 64 * 1024, Parallelism: 2}
}

func Create(path, password string, root walletroot.Root) error {
	if len(password) < 12 {
		return errors.New("wallet password must contain at least 12 bytes")
	}
	if _, err := os.Lstat(path); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create keystore directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("protect keystore directory: %w", err)
	}

	encoded, err := seal(password, root)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".keystore-*")
	if err != nil {
		return fmt.Errorf("create temporary keystore: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err == nil {
		_, err = temporary.Write(encoded)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write keystore: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return fmt.Errorf("install keystore: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func Open(path, password string) (walletroot.Root, error) {
	var root walletroot.Root
	input, err := os.Open(path)
	if err != nil {
		return root, err
	}
	defer input.Close()
	encoded, err := io.ReadAll(io.LimitReader(input, maxFileSize+1))
	if err != nil {
		return root, fmt.Errorf("read keystore: %w", err)
	}
	if len(encoded) > maxFileSize {
		return root, errors.New("keystore file is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var stored file
	if err := decoder.Decode(&stored); err != nil {
		return root, fmt.Errorf("decode keystore: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return root, errors.New("keystore must contain one JSON value")
	}
	if err := validate(stored); err != nil {
		return root, err
	}
	salt, _ := hex.DecodeString(stored.KDF.Salt)
	nonce, _ := hex.DecodeString(stored.Nonce)
	ciphertext, _ := base64.RawStdEncoding.DecodeString(stored.Ciphertext)
	key := argon2.IDKey([]byte(password), salt, stored.KDF.Time, stored.KDF.MemoryKiB, stored.KDF.Parallelism, keyLength)
	defer clear(key)
	cipher, err := chacha20poly1305.NewX(key)
	if err != nil {
		return root, err
	}
	plaintext, err := cipher.Open(nil, nonce, ciphertext, authenticatedData)
	if err != nil || len(plaintext) != len(root) {
		clear(plaintext)
		return root, ErrWrongPassword
	}
	copy(root[:], plaintext)
	clear(plaintext)
	return root, nil
}

func seal(password string, root walletroot.Root) ([]byte, error) {
	params := defaultParameters()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate KDF salt: %w", err)
	}
	params.Salt = hex.EncodeToString(salt)
	key := argon2.IDKey([]byte(password), salt, params.Time, params.MemoryKiB, params.Parallelism, keyLength)
	defer clear(key)
	cipher, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate cipher nonce: %w", err)
	}
	plaintext := append([]byte(nil), root[:]...)
	ciphertext := cipher.Seal(nil, nonce, plaintext, authenticatedData)
	clear(plaintext)
	stored := file{
		Version: formatVersion, KDF: params, Cipher: "xchacha20-poly1305",
		Nonce: hex.EncodeToString(nonce), Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	encoded, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func validate(stored file) error {
	if stored.Version != formatVersion {
		return fmt.Errorf("unsupported keystore version %d", stored.Version)
	}
	if stored.KDF.Name != "argon2id" || stored.Cipher != "xchacha20-poly1305" {
		return errors.New("unsupported keystore cryptography")
	}
	if stored.KDF.Time < 1 || stored.KDF.Time > 10 || stored.KDF.MemoryKiB < 16*1024 || stored.KDF.MemoryKiB > 256*1024 || stored.KDF.Parallelism < 1 || stored.KDF.Parallelism > 16 {
		return errors.New("keystore KDF parameters are outside safe limits")
	}
	salt, err := hex.DecodeString(stored.KDF.Salt)
	if err != nil || len(salt) != 16 {
		return errors.New("keystore salt is invalid")
	}
	nonce, err := hex.DecodeString(stored.Nonce)
	if err != nil || len(nonce) != chacha20poly1305.NonceSizeX {
		return errors.New("keystore nonce is invalid")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(stored.Ciphertext)
	if err != nil || base64.RawStdEncoding.EncodeToString(ciphertext) != stored.Ciphertext || len(ciphertext) != 32+chacha20poly1305.Overhead {
		return errors.New("keystore ciphertext is invalid")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync keystore directory: %w", err)
	}
	return nil
}
