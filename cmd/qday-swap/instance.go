package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type alreadyRunningError struct{ URL string }

func (e *alreadyRunningError) Error() string { return "QDAY Swap is already running" }

type instanceLock struct {
	file         *os.File
	endpointPath string
	publishedURL string
}

func acquireInstance(dataDir string) (*instanceLock, error) {
	parent := filepath.Dir(dataDir)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, fmt.Errorf("create QDAY Swap data parent: %w", err)
	}
	digest := sha256.Sum256([]byte(dataDir))
	name := ".qday-swap-" + hex.EncodeToString(digest[:6])
	lockPath := filepath.Join(parent, name+".lock")
	endpointPath := filepath.Join(parent, name+".endpoint")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open QDAY Swap instance lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		deadline := time.Now().Add(20 * time.Second)
		for {
			if endpoint, readErr := readInstanceEndpoint(endpointPath); readErr == nil {
				return nil, &alreadyRunningError{URL: endpoint}
			}
			if time.Now().After(deadline) {
				return nil, errors.New("QDAY Swap is already starting; reopen it shortly")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	_ = os.Remove(endpointPath)
	return &instanceLock{file: file, endpointPath: endpointPath}, nil
}

func (i *instanceLock) Publish(endpoint string) error {
	if i == nil || i.file == nil {
		return errors.New("QDAY Swap instance lock is unavailable")
	}
	if err := validateInstanceEndpoint(endpoint); err != nil {
		return err
	}
	temporary := fmt.Sprintf("%s.%d.tmp", i.endpointPath, os.Getpid())
	if err := os.WriteFile(temporary, []byte(endpoint+"\n"), 0600); err != nil {
		return fmt.Errorf("write QDAY Swap endpoint: %w", err)
	}
	if err := os.Rename(temporary, i.endpointPath); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish QDAY Swap endpoint: %w", err)
	}
	i.publishedURL = endpoint
	return nil
}

func (i *instanceLock) Close() error {
	if i == nil || i.file == nil {
		return nil
	}
	if current, err := readInstanceEndpoint(i.endpointPath); err == nil && current == i.publishedURL {
		_ = os.Remove(i.endpointPath)
	}
	err := i.file.Close()
	i.file = nil
	return err
}

func readInstanceEndpoint(path string) (string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimSpace(string(encoded))
	if err := validateInstanceEndpoint(endpoint); err != nil {
		return "", err
	}
	return endpoint, nil
}

func validateInstanceEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid QDAY Swap instance endpoint")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return errors.New("invalid QDAY Swap instance endpoint")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("invalid QDAY Swap instance endpoint")
	}
	token := strings.TrimPrefix(parsed.Path, "/open/")
	if len(token) != 64 || len(parsed.Path) != len("/open/")+64 {
		return errors.New("invalid QDAY Swap instance endpoint")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return errors.New("invalid QDAY Swap instance endpoint")
	}
	return nil
}
