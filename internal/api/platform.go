package api

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/acme"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/monitor"
)

// PlatformStatus is the server overview for `clowctl platform status`.
type PlatformStatus struct {
	Version  string          `json:"version"`
	Docker   string          `json:"docker,omitempty"`
	Registry string          `json:"registry"`
	Host     monitor.Host    `json:"host"`
	Nginx    NginxStatus     `json:"nginx"`
	Certs    []CertStatus    `json:"certs,omitempty"`
	Projects []ProjectStatus `json:"projects"`
}

type NginxStatus struct {
	Image    string    `json:"image,omitempty"`
	State    string    `json:"state"` // running, exited, ..., missing
	Started  time.Time `json:"started,omitzero"`
	Restarts int       `json:"restarts"`
	// ConfigError is the `nginx -t` output when the config on disk does not load.
	ConfigError string `json:"config_error,omitempty"`
	// Configured is nginx.image in platform.yml; Image differs from it when the new image
	// could not be pulled and the old container serves on.
	Configured string `json:"configured,omitempty"`
	// Unpinned: Configured names no version, so it never gets fixes (clowdrd pulls only a
	// missing image).
	Unpinned bool `json:"unpinned,omitempty"`
	// MissingNetworks are the projects whose network nginx is not in: their sites fail.
	MissingNetworks []string `json:"missing_networks,omitempty"`
}

type CertStatus struct {
	Domain  string    `json:"domain"`
	Expires time.Time `json:"expires,omitzero"`
	Error   string    `json:"error,omitempty"`
}

type ProjectStatus struct {
	Name     string `json:"name"`
	Services int    `json:"services"`
	Running  int    `json:"running"`  // services that are not stopped
	Reserved int64  `json:"reserved"` // memory limits of the running replicas
	Quota    int64  `json:"quota,omitempty"`
}

func (s *Server) platformStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := PlatformStatus{Version: s.Version, Registry: s.RegistryKind}
	if info, err := s.Engine.Docker.Info(ctx); err == nil {
		st.Docker = info.ServerVersion
	}
	if s.StateDir != "" {
		st.Host = monitor.HostStats(s.StateDir)
	}

	ps, err := s.Store.Projects()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	st.Nginx.State = "missing"
	st.Nginx.Configured = s.Engine.Platform().Nginx.Image
	st.Nginx.Unpinned = !config.VersionedImage(st.Nginx.Configured)
	if ci, err := s.Engine.Docker.ContainerInspect(ctx, deploy.NginxContainer); err == nil {
		st.Nginx.Image, st.Nginx.State = ci.Config.Image, ci.State.Status
		st.Nginx.Started, st.Nginx.Restarts = ci.State.StartedAt, ci.RestartCount
		for _, p := range ps {
			if _, in := ci.NetworkSettings.Networks[deploy.NetworkName(p.Name)]; !in {
				st.Nginx.MissingNetworks = append(st.Nginx.MissingNetworks, p.Name)
			}
		}
		if ci.State.Running {
			if out, err := (deploy.NginxExec{Docker: s.Engine.Docker}).Test(ctx); err != nil {
				st.Nginx.ConfigError = strings.TrimSpace(err.Error() + "\n" + out)
			}
		}
	}

	for _, d := range s.Engine.CertDomains() {
		c := CertStatus{Domain: d}
		exp, err := acme.CertExpiry(ingress.CertDir, d)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			c.Error = "not issued yet"
		case err != nil:
			c.Error = err.Error()
		default:
			c.Expires = exp
		}
		st.Certs = append(st.Certs, c)
	}

	for _, p := range ps {
		svcs, err := s.Store.Services(p.Name)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		pst := ProjectStatus{Name: p.Name, Services: len(svcs), Quota: p.Memory}
		for _, v := range svcs {
			if v.Stopped {
				continue
			}
			pst.Running++
			pst.Reserved += int64(v.Spec.Replicas) * v.Spec.Memory
		}
		st.Projects = append(st.Projects, pst)
	}
	writeJSON(w, 200, st)
}
