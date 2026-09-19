package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/petoshi/qday-swap/internal/app"
	"github.com/petoshi/qday-swap/internal/localui"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "qday-swap:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("qday-swap", flag.ContinueOnError)
	dataDir := flags.String("data", app.DefaultDataDir(), "private application data directory")
	walletdBinary := flags.String("walletd", "", "path to the bundled qday-walletd binary")
	listen := flags.String("listen", "127.0.0.1:0", "local browser interface")
	relayURL := flags.String("relay", "https://dex.pqday.com", "public signed order relay")
	noOpen := flags.Bool("no-open", false, "print the local URL without opening a browser")
	showVersion := flags.Bool("version", false, "print version")
	if err := flags.Parse(arguments); err != nil {
		return err
	} else if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	} else if *showVersion {
		fmt.Println("qday-swap", version)
		return nil
	}

	listener, err := loopbackListener(*listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.New(ctx, app.Config{
		DataDir: *dataDir, WalletdBinary: *walletdBinary,
		Network: "mainnet", RelayURL: *relayURL,
	})
	if err != nil {
		return err
	}
	defer application.Close()
	interfaceServer, err := localui.New(application, listener.Addr().String())
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: interfaceServer, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 70 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	openURL := interfaceServer.OpenURL()
	fmt.Println("QDAY Swap local interface:", openURL)
	if !*noOpen {
		if err := openBrowser(openURL); err != nil {
			fmt.Fprintln(os.Stderr, "Open this URL in a browser:", openURL)
		}
	}

	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func loopbackListener(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("local interface must listen on a loopback address")
	}
	return net.Listen("tcp", address)
}

func openBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		command = exec.Command("open", url)
	default:
		if _, err := exec.LookPath("xdg-open"); err == nil {
			command = exec.Command("xdg-open", url)
		} else {
			command = exec.Command("gio", "open", url)
		}
	}
	return command.Start()
}
