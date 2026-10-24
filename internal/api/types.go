// Package api is the HTTP API between clowctl and clowdrd. Types here are shared by the
// server and the client.
package api

import (
	"time"

	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

type DeployRequest struct {
	Spec spec.Service `json:"spec"`
	// Tag is the tag pushed to the built-in registry (<project>/<service>:<tag>).
	Tag string `json:"tag,omitempty"`
	// Image is an external image reference, pulled by the server. Excludes Tag.
	Image   string `json:"image,omitempty"`
	Version string `json:"version,omitempty"`
	Author  string `json:"author,omitempty"`
}

type RollbackRequest struct {
	To     uint64 `json:"to,omitempty"`
	Author string `json:"author,omitempty"`
}

type RestartRequest struct {
	Author string `json:"author,omitempty"`
}

type ApplyRequest struct {
	Services []spec.Service `json:"services"`
	Author   string         `json:"author,omitempty"`
}

// Event is one line of a streamed response (NDJSON). The last line has Done set.
type Event struct {
	Time    time.Time      `json:"time"`
	Message string         `json:"msg,omitempty"`
	Error   string         `json:"error,omitempty"`
	Release *store.Release `json:"release,omitempty"`
	Done    bool           `json:"done,omitempty"`
}

type ProjectRequest struct {
	Name       string   `json:"name"`
	Domains    []string `json:"domains"`
	Addons     []string `json:"addons,omitempty"`
	Memory     int64    `json:"memory,omitempty"`
	NginxExtra bool     `json:"nginx_extra,omitempty"`
	// AdoptVolumes lets a new project take over the volumes of a removed one of the same name.
	AdoptVolumes bool `json:"adopt_volumes,omitempty"`
}

type TokenRequest struct {
	Project string `json:"project,omitempty"`
	Env     string `json:"env,omitempty"`
	Name    string `json:"name,omitempty"`
	Admin   bool   `json:"admin,omitempty"`
}

type TokenResponse struct {
	Token string      `json:"token"` // shown once
	Info  store.Token `json:"info"`
}

type EnvRequest struct {
	Set   map[string]string `json:"set,omitempty"`
	Unset []string          `json:"unset,omitempty"`
}

type ServiceStatus struct {
	Name       string            `json:"name"`
	Stopped    bool              `json:"stopped,omitempty"`
	Replicas   int               `json:"replicas"`
	Memory     int64             `json:"memory,omitempty"`
	Domains    []string          `json:"domains,omitempty"`
	Release    *store.Release    `json:"release,omitempty"`
	Containers []ContainerStatus `json:"containers"`
	Spec       spec.Service      `json:"spec"`
}

type ContainerStatus struct {
	Name     string    `json:"name"`
	State    string    `json:"state"`
	Health   string    `json:"health,omitempty"`
	Started  time.Time `json:"started,omitzero"`
	Restarts int       `json:"restarts"`
	OOM      bool      `json:"oom,omitempty"`
	MemUsage int64     `json:"mem_usage,omitempty"`
	CPU      float64   `json:"cpu,omitempty"`
}

type LogLine struct {
	Time      string `json:"t,omitempty"`
	Container string `json:"c"`
	Stderr    bool   `json:"e,omitempty"`
	Line      string `json:"l"`
}

type Version struct {
	Version   string `json:"version"`
	Docker    string `json:"docker,omitempty"`
	Registry  string `json:"registry"`
	Principal string `json:"principal,omitempty"`
}

type Error struct {
	Error string `json:"error"`
}
