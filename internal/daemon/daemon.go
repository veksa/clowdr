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

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/notify"
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
	if tg := plat.Notify.Telegram; tg.Token != "" && tg.ChatID != "" {
		bot := &notify.Telegram{Token: tg.Token, ChatID: tg.ChatID, Log: log}
		eng.Notify = bot.Send
	}

	// nginx: write config, make sure the container runs, then a checked reload.
	if err := bootstrapNginx(ctx, eng, ing, st); err != nil {
		return err
	}
	if err := cleanupOrphans(ctx, docker, st, log); err != nil {
		log.Warn("orphan cleanup", "err", err)
	}

	if err := bootstrapAdmin(st, o.StateDir, log); err != nil {
		return err
	}

	srv := &api.Server{Store: st, Engine: eng, Log: log, Version: o.Version, RegistryKind: regKind}
	reg := &registry.Handler{Store: regStore, Owners: st, Authorize: srv.RegistryAuthorize, Log: log}
	srv.Registry = reg
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
		if err := os.WriteFile(path, []byte(config.DefaultPlatform), 0o644); err != nil {
			return nil, err
		}
		log.Info("wrote default platform config", "path", path)
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

func bootstrapNginx(ctx context.Context, eng *deploy.Engine, ing *ingress.Controller, st *store.Store) error {
	opts := eng.IngressOptions()
	files := []ingress.File{
		{Path: "nginx.conf", Content: ingress.RenderMain(opts)},
		{Path: "proxy_defaults.conf", Content: ingress.ProxyDefaultsConf},
	}
	if err := ing.Write(files...); err != nil {
		return err
	}
	if err := eng.EnsureNginx(ctx); err != nil {
		return err
	}
	if err := eng.RenderAll(ctx); err != nil {
		return fmt.Errorf("nginx config: %w", err)
	}
	return nil
}

// cleanupOrphans removes service containers that no release references any more (e.g. a
// clowdrd crash in the middle of a deploy).
func cleanupOrphans(ctx context.Context, d *runtime.Docker, st *store.Store, log *slog.Logger) error {
	cs, err := d.Containers(ctx, runtime.LabelManaged, runtime.LabelRole+"=service")
	if err != nil {
		return err
	}
	svcs, err := st.Services("")
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, v := range svcs {
		if r, err := st.Release(v.Project, v.Name, v.Current); err == nil && v.Current != 0 {
			for _, c := range r.Containers {
				keep[c] = true
			}
		}
	}
	for _, c := range cs {
		if keep[c.Name()] {
			continue
		}
		log.Info("removing orphan container", "name", c.Name())
		d.ContainerRemove(ctx, c.ID)
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
