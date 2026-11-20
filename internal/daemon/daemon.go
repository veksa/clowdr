// Package daemon wires clowdrd together: state, docker, registry, nginx and the API.
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/acme"
	"github.com/veksa/clowdr/internal/analytics"
	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/monitor"
	"github.com/veksa/clowdr/internal/notify"
	"github.com/veksa/clowdr/internal/npm"
	"github.com/veksa/clowdr/internal/registry"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/store"
)

type Options struct {
	PlatformPath string
	StateDir     string // /var/lib/clowdr
	// RegistryDir switches the registry to a plain directory (development without the
	// containerd image store: pushed images are not visible to docker).
	RegistryDir    string
	Containerd     string // containerd socket
	TLSListen      string // used without a domain, e.g. ":7443"
	Version        string
	MinDockerMajor int
}

func Run(ctx context.Context, o Options, log *slog.Logger) error {
	if o.MinDockerMajor == 0 {
		o.MinDockerMajor = 29
	}
	plat, err := loadPlatform(o.PlatformPath, log)
	if err != nil {
		return err
	}
	if !config.VersionedImage(plat.Nginx.Image) {
		log.Warn("nginx.image in platform.yml has no version: clowdrd pulls it only when it is missing, so it never gets security fixes; pin a version and bump it by hand",
			"image", plat.Nginx.Image, "example", config.DefaultNginxImage)
	}
	for _, dir := range []string{ingress.ProjectsDir, ingress.CertDir, ingress.RunDir, api.ACMEDir, o.StateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	st, err := store.Open(filepath.Join(o.StateDir, "state.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := seedProjects(st, plat); err != nil {
		return err
	}

	docker, err := runtime.New()
	if err != nil {
		return err
	}
	info, err := docker.Info(ctx)
	if err != nil {
		return fmt.Errorf("docker is not reachable: %w", err)
	}
	if err := checkDocker(info, o, log); err != nil {
		return err
	}
	tuneConntrack(log)
	if o.RegistryDir == "" { // not on a development machine
		go keepMetadataBlocked(ctx, log)
	}

	var regStore registry.Store
	regKind := "containerd"
	if o.RegistryDir != "" {
		regStore, regKind = &registry.FSStore{Dir: o.RegistryDir}, "fs"
		log.Warn("registry uses a plain directory: pushed images are NOT visible to docker", "dir", o.RegistryDir)
	} else {
		cs, err := registry.NewContainerdStore(o.Containerd, info.Driver)
		if err != nil {
			return err
		}
		defer cs.Close()
		regStore = cs
	}

	current := plat
	ing := &ingress.Controller{Dir: ingress.ConfDir, Nginx: deploy.NginxExec{Docker: docker}}
	eng := deploy.New(docker, st, func() *config.Platform { return current }, ing, log)

	// Logs: containers -> fluentd protocol, nginx -> syslog socket; buffered in tmpfs, flushed
	// to disk in compressed batches.
	mon := &monitor.Monitor{Docker: docker, Store: st, Prober: eng.Prober, Platform: func() *config.Platform { return current }, StateDir: o.StateDir, Log: log}
	mon.Init()
	// Product analytics (docs/analytics.md): records from the same log pipeline, indexed and
	// rolled up per project. Closed after the logs stop, so the last lines reach the index.
	am := &analytics.Manager{Dir: filepath.Join(o.StateDir, "analytics"), Log: log}
	defer am.Close()
	onLine := func(k logs.Key, l logs.Line) {
		mon.OnLine(k, l)
		am.OnLine(k, l)
	}
	logStore, stopLogs, err := startLogs(ctx, o, st, func() *config.Platform { return current }, onLine, log)
	if err != nil {
		return err
	}
	defer stopLogs()
	am.Logs = logStore
	am.Exists = func(project string) bool { _, err := st.Project(project); return err == nil }
	am.Retention = func() time.Duration { return current.Logs.Analytics.Retention.D() }
	am.RollupCache = func() int64 { return int64(current.Analytics.RollupCache) }
	am.AllowPrivate = func() []string { return current.Analytics.Archive.AllowPrivate }
	eng.Syslog = true
	eng.LogTail = logStore.Tail
	eng.MoveLogs = func(old, name string) {
		am.RenameProject(old, name)
		logStore.RenameProject(old, name)
	}
	eng.LogConfig = fluentdLogConfig
	events := &monitor.Events{Logs: logStore}
	if tg := plat.Notify.Telegram; tg.Token != "" && tg.ChatID != "" {
		events.Alert = (&notify.Telegram{Token: tg.Token, ChatID: tg.ChatID, Log: log}).Send
	}
	eng.OnEvent = func(ctx context.Context, project, service, kind string, failed bool, msg string) {
		sev := monitor.Info
		if failed {
			sev = monitor.Error
		}
		events.Emit(ctx, monitor.Event{Project: project, Service: service, Kind: kind, Severity: sev, Message: msg}, "")
	}
	mon.Logs, mon.Events = logStore, events
	nginxDown := make(chan struct{}, 1)
	mon.NginxDown = func() {
		select {
		case nginxDown <- struct{}{}:
		default:
		}
	}
	go mon.Run(ctx)
	go am.Run(ctx)

	// nginx: make sure the container runs on the current config (checked reload if it already ran).
	if err := eng.StartNginx(ctx); err != nil {
		return err
	}
	// Finish what a killed clowdrd or a reboot left half-done, before anything deploys.
	if err := eng.Recover(ctx); err != nil {
		log.Error("recover", "err", err)
	}
	if res, err := eng.GC(ctx); err != nil {
		log.Warn("gc", "err", err)
	} else if len(res.Containers)+len(res.Images) > 0 {
		log.Info("gc", "containers", res.Containers, "images", res.Images)
	}

	if err := bootstrapAdmin(st, o.StateDir, log); err != nil {
		return err
	}

	srv := &api.Server{Store: st, Engine: eng, Log: log, Version: o.Version, RegistryKind: regKind, Logs: logStore, Events: events, StateDir: o.StateDir, Analytics: am}
	reg := &registry.Handler{Store: regStore, Owners: st, Authorize: srv.RegistryAuthorize, Log: log,
		OnTag: func(_ context.Context, repo, tag string, _ registry.Descriptor) {
			eng.Pushed(registry.ImagePrefix + repo + ":" + tag)
		},
		MaxBlob: func() int64 { return int64(current.Limits.MaxBlob) },
		LowDisk: func() error { return lowDisk(o.StateDir) }}
	srv.Registry = reg
	srv.NPM = &npm.Handler{Store: st, Dir: filepath.Join(o.StateDir, "npm"), Log: log,
		Config:  func() config.NPMConf { return current.NPM },
		Plugins: func() config.PluginsConf { return current.Plugins },
		Domain:  func() string { return current.Domain }}
	handler := srv.Handler()

	errc := make(chan error, 3)
	var servers []*http.Server
	serve := func(l net.Listener, what string) {
		hs := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
		servers = append(servers, hs)
		log.Info("listening", "on", what)
		go func() {
			if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", what, err)
			}
		}()
	}

	// Unix socket: nginx proxies deploy.<domain> and ACME challenges here.
	os.Remove(ingress.APISocket)
	ul, err := net.Listen("unix", ingress.APISocket)
	if err != nil {
		return err
	}
	os.Chmod(ingress.APISocket, 0o666) // nginx workers run unprivileged; every request still needs a token
	serve(ul, "unix:"+ingress.APISocket)

	if plat.Listen != "" {
		tl, err := net.Listen("tcp", plat.Listen)
		if err != nil {
			return err
		}
		serve(tl, "http://"+plat.Listen)
	}

	if plat.Domain == "" && o.TLSListen != "" {
		cert, fp, err := selfSignedCert(filepath.Join(o.StateDir, "tls"))
		if err != nil {
			return err
		}
		tl, err := tls.Listen("tcp", o.TLSListen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		serve(tl, "https://"+o.TLSListen)
		log.Info("no domain configured: API and registry use a self-signed certificate", "fingerprint", fp)
		os.WriteFile(filepath.Join(o.StateDir, "tls", "fingerprint"), []byte(fp+"\n"), 0o644)
	}

	// Certificates: deploy.<domain> and `tls: direct` domains. Checked on start, after route
	// changes and twice a day; nginx reloads when a certificate was issued.
	certs := &acme.Manager{
		Directory: plat.ACMEDirectory, Email: plat.ACMEEmail, StateDir: o.StateDir,
		CertDir: ingress.CertDir, ChallengeDir: api.ACMEDir, Log: log,
	}
	kick := make(chan struct{}, 1)
	eng.RoutesChanged = func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	go func() {
		t := time.NewTicker(12 * time.Hour)
		defer t.Stop()
		for {
			issued, err := certs.Ensure(ctx, eng.CertDomains())
			if err != nil {
				log.Error("acme", "err", err)
			}
			if len(issued) > 0 {
				if err := eng.RenderAll(ctx); err != nil {
					log.Error("nginx after new certificates", "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-kick:
			}
		}
	}()

	// nginx watchdog: docker restarts a crashed nginx itself, but not one that was stopped or
	// removed. Check shortly after it went down, retrying until it runs.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-nginxDown:
			}
			for i := 0; i < 12; i++ {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				err := eng.EnsureNginx(ctx)
				if err == nil {
					break
				}
				log.Error("nginx", "err", err)
			}
		}
	}()

	// Housekeeping.
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reg.ExpireUploads(ctx, time.Hour)
				srv.NPM.Sweep(ctx)
				if _, err := eng.GC(ctx); err != nil {
					log.Warn("gc", "err", err)
				}
				if err := eng.EnsureNginx(ctx); err != nil {
					log.Error("nginx", "err", err)
				}
			}
		}
	}()

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, hs := range servers {
		hs.Shutdown(sctx)
	}
	eng.WaitDrains()
	return err
}

func loadPlatform(path string, log *slog.Logger) (*config.Platform, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(config.DefaultPlatform), 0o600); err != nil {
			return nil, err
		}
		log.Info("wrote default platform config", "path", path)
	}
	// It may hold the Telegram bot token; install.sh and older versions created it 0644.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		os.Chmod(path, 0o600)
	}
	return config.LoadPlatform(path)
}

func seedProjects(st *store.Store, plat *config.Platform) error {
	for name, pc := range plat.Projects {
		p, err := st.Project(name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		p.Name, p.Domains, p.Addons, p.Memory, p.Seeded = name, pc.Domains, pc.Addons, int64(pc.Memory), true
		if err := st.PutProject(p); err != nil {
			return err
		}
	}
	return nil
}

func checkDocker(info runtime.Info, o Options, log *slog.Logger) error {
	major, _ := strconv.Atoi(strings.SplitN(info.ServerVersion, ".", 2)[0])
	log.Info("docker", "version", info.ServerVersion, "driver", info.Driver, "containerd_store", info.ContainerdStore())
	if o.RegistryDir != "" {
		return nil
	}
	if major < o.MinDockerMajor {
		return fmt.Errorf("docker %s is too old: clowdr needs Docker %d+ (curl -fsSL https://get.docker.com | sh)", info.ServerVersion, o.MinDockerMajor)
	}
	if !info.ContainerdStore() {
		return errors.New(`docker does not use the containerd image store; add {"features": {"containerd-snapshotter": true}} to /etc/docker/daemon.json and restart docker`)
	}
	return nil
}

// bootstrapAdmin creates the first admin token and stores it root-only so install.sh can
// print it once.
func bootstrapAdmin(st *store.Store, stateDir string, log *slog.Logger) error {
	ts, err := st.Tokens()
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.Admin {
			return nil
		}
	}
	secret, t := auth.New("", "", "bootstrap", true)
	if err := st.PutToken(t); err != nil {
		return err
	}
	p := filepath.Join(stateDir, "admin-token")
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		return err
	}
	log.Info("created admin token", "file", p)
	return nil
}

// lowDisk stops pushes before the disk holding the image store (containerd's, on the same
// filesystem as the state dir on a usual host) runs out: under 256MB or 2% free. A disk at 95%
// still takes deploys (the monitor alerts at disk_free_alert); a push that would empty it does not.
func lowDisk(stateDir string) error {
	dir := stateDir
	if _, err := os.Stat("/var/lib/containerd"); err == nil {
		dir = "/var/lib/containerd"
	}
	free, total, ok := monitor.DiskFree(dir)
	if !ok || total == 0 {
		return nil
	}
	if free < 256<<20 || free*100/total < 2 {
		return fmt.Errorf("server disk is almost full (%s free): pushes are paused", config.FormatBytes(free))
	}
	return nil
}
