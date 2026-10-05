// Command clowdr is both the server and the CLI: installed as `clowdrd` it runs the daemon,
// under any other name (`clowctl`) it is the client.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/veksa/clowdr/internal/cli"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/daemon"
)

// version is set by the release build (-ldflags "-X main.version=v0.1.0").
var version = "dev"

func main() {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if strings.HasPrefix(name, "clowdrd") {
		os.Exit(runDaemon(os.Args[1:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Exit(runDaemon(os.Args[2:]))
	}
	os.Exit(cli.Execute(version))
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("clowdrd", flag.ExitOnError)
	o := daemon.Options{Version: version}
	fs.StringVar(&o.PlatformPath, "config", config.DefaultPlatformPath, "platform config")
	fs.StringVar(&o.StateDir, "state-dir", "/var/lib/clowdr", "state directory")
	fs.StringVar(&o.Containerd, "containerd", "/run/containerd/containerd.sock", "containerd socket used by docker")
	fs.StringVar(&o.TLSListen, "tls-listen", ":7443", "HTTPS listen address when no domain is configured")
	fs.StringVar(&o.RegistryDir, "registry-dir", "", "development: keep pushed blobs in a directory instead of containerd")
	debug := fs.Bool("debug", false, "debug logging")
	showVersion := fs.Bool("version", false, "print version")
	fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, o, log); err != nil {
		log.Error("clowdrd stopped", "err", err)
		return 1
	}
	return 0
}
