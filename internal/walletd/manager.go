package walletd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/petoshi/qday-swap/internal/walletroot"
)

type ManagerConfig struct {
	Binary          string
	DataDir         string
	Listen          string
	P2P             string
	Peers           string
	NetworkManifest string
	LogPath         string
}

type Manager struct {
	config ManagerConfig

	mu       sync.Mutex
	command  *exec.Cmd
	done     chan error
	client   *Client
	endpoint string
	log      *os.File
}

func NewManager(config ManagerConfig) (*Manager, error) {
	if config.Binary == "" {
		return nil, errors.New("qday-walletd binary path is required")
	} else if config.DataDir == "" {
		return nil, errors.New("qday-walletd data directory is required")
	}
	if config.P2P == "" {
		config.P2P = "127.0.0.1:0"
	}
	if config.LogPath == "" {
		config.LogPath = filepath.Join(config.DataDir, "qday-walletd.log")
	}
	return &Manager{config: config}, nil
}

func (m *Manager) Initialized() bool {
	info, err := os.Stat(filepath.Join(m.config.DataDir, "master.key"))
	return err == nil && info.Mode().IsRegular()
}

// Initialize imports the shared recovery root through an anonymous pipe. The
// password and phrase are never passed in arguments, environment variables or
// plaintext files. qday-walletd v0.2.1 or newer is required.
func (m *Manager) Initialize(ctx context.Context, root walletroot.Root, password string) error {
	if len(password) < 12 {
		return errors.New("wallet password must contain at least 12 bytes")
	} else if m.Initialized() {
		return os.ErrExist
	}
	phrase, err := root.Phrase()
	if err != nil {
		return err
	}
	input, err := json.Marshal(struct {
		Password string `json:"password"`
		Phrase   string `json:"phrase"`
	}{Password: password, Phrase: phrase})
	phrase = ""
	if err != nil {
		return err
	}
	defer clear(input)
	if err := os.MkdirAll(m.config.DataDir, 0700); err != nil {
		return fmt.Errorf("create walletd directory: %w", err)
	}
	if err := os.Chmod(m.config.DataDir, 0700); err != nil {
		return fmt.Errorf("protect walletd directory: %w", err)
	}
	command := exec.CommandContext(ctx, m.config.Binary, "init", "-data", m.config.DataDir, "-secret-stdin")
	command.Stdin = bytes.NewReader(input)
	command.Stdout = io.Discard
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("initialize qday-walletd: %s", message)
	}
	if !m.Initialized() {
		return errors.New("qday-walletd initialization completed without creating master.key")
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.command != nil {
		return m.client, nil
	} else if !m.Initialized() {
		return nil, errors.New("qday-walletd is not initialized")
	}
	listen := m.config.Listen
	if listen == "" {
		var err error
		listen, err = freeLoopbackAddress()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(m.config.LogPath), 0700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(m.config.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open qday-walletd log: %w", err)
	}
	arguments := []string{"run", "-data", m.config.DataDir, "-listen", listen, "-p2p", m.config.P2P}
	if m.config.Peers != "" {
		arguments = append(arguments, "-peers", m.config.Peers)
	}
	if m.config.NetworkManifest != "" {
		arguments = append(arguments, "-network", m.config.NetworkManifest)
	}
	command := exec.CommandContext(ctx, m.config.Binary, arguments...)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start qday-walletd: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	m.command, m.done, m.endpoint, m.log = command, done, "http://"+listen, logFile

	client, err := m.waitForClient(ctx, done)
	if err != nil {
		if command.ProcessState == nil || !command.ProcessState.Exited() {
			_ = terminate(command, done, 3*time.Second)
		}
		_ = logFile.Close()
		m.command, m.done, m.endpoint, m.log = nil, nil, "", nil
		return nil, err
	}
	m.client = client
	return client, nil
}

func (m *Manager) waitForClient(ctx context.Context, done <-chan error) (*Client, error) {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastError error
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-done:
			if err == nil {
				err = errors.New("process exited")
			}
			return nil, fmt.Errorf("qday-walletd stopped during startup: %w", err)
		case <-deadline.C:
			if lastError == nil {
				lastError = errors.New("health endpoint did not become available")
			}
			return nil, fmt.Errorf("qday-walletd startup timed out: %w", lastError)
		case <-ticker.C:
			token, err := readToken(filepath.Join(m.config.DataDir, "api.token"))
			if err != nil {
				lastError = err
				continue
			}
			client, err := NewClient(m.endpoint, token)
			if err == nil {
				healthCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				err = client.Health(healthCtx)
				cancel()
			}
			if err == nil {
				return client, nil
			}
			lastError = err
		}
	}
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.command == nil {
		return nil
	}
	err := terminate(m.command, m.done, 10*time.Second)
	if m.log != nil {
		if closeErr := m.log.Close(); err == nil {
			err = closeErr
		}
	}
	m.command, m.done, m.client, m.endpoint, m.log = nil, nil, nil, "", nil
	return err
}

func (m *Manager) Endpoint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endpoint
}

func terminate(command *exec.Cmd, done <-chan error, timeout time.Duration) error {
	if command == nil || command.Process == nil {
		return nil
	} else if command.ProcessState != nil && command.ProcessState.Exited() {
		return nil
	}
	_ = command.Process.Signal(os.Interrupt)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) {
				return err
			}
		}
		return nil
	case <-timer.C:
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-done
		return nil
	}
}

func readToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	} else if !info.Mode().IsRegular() || info.Size() > 256 {
		return "", errors.New("walletd token file is invalid")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(encoded))
	clear(encoded)
	if len(token) != 64 {
		return "", errors.New("walletd token file is invalid")
	}
	for _, character := range token {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return "", errors.New("walletd token file is invalid")
		}
	}
	return token, nil
}

func freeLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve walletd loopback port: %w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return address, nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	const maximumSize = 32 << 10
	remaining := maximumSize - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(value[:min(len(value), remaining)])
	}
	return len(value), nil
}
