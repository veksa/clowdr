package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/veksa/clowdr/internal/deploy"
	"github.com/veksa/clowdr/internal/monitor"
	"github.com/veksa/clowdr/internal/store"
)

type ScaleRequest struct {
	Replicas int    `json:"replicas"`
	Author   string `json:"author,omitempty"`
}

type ExecRequest struct {
	Cmd     []string `json:"cmd"`
	Replica int      `json:"replica,omitempty"`
}

type ExecResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

type TopResponse struct {
	Host     monitor.Host    `json:"host"`
	Services []ServiceStatus `json:"services"`
}

func (s *Server) registerOps(a func(string, http.HandlerFunc)) {
	a("POST /v1/projects/{project}/services/{service}/scale", s.scale)
	a("POST /v1/projects/{project}/services/{service}/exec", s.exec)
	a("GET /v1/projects/{project}/top", s.top)
	a("GET /v1/projects/{project}/volumes", s.volumes)
	a("DELETE /v1/projects/{project}/volumes/{volume}", s.removeVolume)
	a("GET /v1/projects/{project}/addons", s.addons)
	a("DELETE /v1/projects/{project}/addons/{addon}", s.removeAddon)
	a("POST /v1/projects/{project}/addons/{addon}/exec", s.addonExec)
}

func (s *Server) scale(w http.ResponseWriter, r *http.Request) {
	var req ScaleRequest
	if !decode(w, r, &req) {
		return
	}
	s.stream(w, r, func(ctx context.Context, p deploy.Progress) (*store.Release, error) {
		rel, err := s.Engine.Scale(ctx, r.PathValue("project"), r.PathValue("service"), req.Replicas, author(r, req.Author), p)
		if rel.ID == 0 {
			return nil, err
		}
		return &rel, err
	})
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if !decode(w, r, &req) {
		return
	}
	out, code, err := s.Engine.Exec(r.Context(), r.PathValue("project"), r.PathValue("service"), req.Replica, req.Cmd)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, 200, ExecResponse{Output: out, ExitCode: code})
}

func (s *Server) addonExec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if !decode(w, r, &req) {
		return
	}
	out, code, err := s.Engine.AddonExec(r.Context(), r.PathValue("project"), r.PathValue("addon"), req.Cmd)
	if err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	writeJSON(w, 200, ExecResponse{Output: out, ExitCode: code})
}

func (s *Server) top(w http.ResponseWriter, r *http.Request) {
	svcs, err := s.Store.Services(r.PathValue("project"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := TopResponse{Services: make([]ServiceStatus, len(svcs))}
	if s.StateDir != "" {
		resp.Host = monitor.HostStats(s.StateDir)
	}
	done := make(chan struct{}, len(svcs))
	for i, v := range svcs {
		go func() {
			resp.Services[i] = s.status(r.Context(), v, true)
			done <- struct{}{}
		}()
	}
	for range svcs {
		<-done
	}
	writeJSON(w, 200, resp)
}

func (s *Server) volumes(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Engine.Volumes(r.Context(), r.PathValue("project"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if vs == nil {
		vs = []deploy.Volume{}
	}
	writeJSON(w, 200, vs)
}

func (s *Server) removeVolume(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.RemoveVolume(r.Context(), r.PathValue("project"), r.PathValue("volume")); err != nil {
		status := 400
		if errors.Is(err, store.ErrNotFound) {
			status = 404
		}
		writeErr(w, status, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) addons(w http.ResponseWriter, r *http.Request) {
	as, err := s.Engine.Addons(r.Context(), r.PathValue("project"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if as == nil {
		as = []deploy.Addon{}
	}
	writeJSON(w, 200, as)
}

func (s *Server) removeAddon(w http.ResponseWriter, r *http.Request) {
	err := s.Engine.RemoveAddon(r.Context(), r.PathValue("project"), r.PathValue("addon"), r.URL.Query().Get("volumes") == "1")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}
