// Package fake provides in-memory docker, prober and nginx for tests.
package fake

import (
	"context"
	"errors"
	"io"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/runtime"
)

type Container struct {
	Config  runtime.ContainerConfig
	Running bool
	Exit    int
}

type Docker struct {
	mu       sync.Mutex
	ByName   map[string]*Container
	Local    map[string]bool // local images
	Removed  []string
	MemTotal int64
	Vols     map[string]map[string]string // volume name → labels
	Networks map[string][]string          // network → connected containers
	// ExecFn answers Exec; nil = empty output, exit 0. Execs records every call.
	ExecFn func(container string, cmd []string) (string, int, error)
	Execs  []string
	// Crash makes containers of these images exit right after start.
	Crash map[string]bool
}

func NewDocker() *Docker {
	return &Docker{ByName: map[string]*Container{}, Local: map[string]bool{}, Crash: map[string]bool{},
		Vols: map[string]map[string]string{}, Networks: map[string][]string{}}
}

func (f *Docker) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{MemTotal: f.MemTotal}, nil
}

func (f *Docker) ImageInspect(_ context.Context, ref string) (runtime.ImageInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Local[ref] {
		return runtime.ImageInspect{}, &runtime.APIError{Status: 404}
	}
	return runtime.ImageInspect{ID: ref}, nil
}

func (f *Docker) ImagePull(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Local[ref] = true
	return nil
}

func (f *Docker) ImageRemove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Local, ref)
	f.Removed = append(f.Removed, ref)
	return nil
}

func (f *Docker) ContainerCreate(_ context.Context, c runtime.ContainerConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ByName[c.Name]; ok {
		return "", &runtime.APIError{Status: 409, Message: "name in use"}
	}
	f.ByName[c.Name] = &Container{Config: c}
	return c.Name, nil
}

func (f *Docker) ContainerStart(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.ByName[id]
	if !ok {
		return &runtime.APIError{Status: 404}
	}
	c.Running = !f.Crash[c.Config.Image]
	if !c.Running {
		c.Exit = 1
	}
	return nil
}

func (f *Docker) ContainerStop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.ByName[id]; ok {
		c.Running = false
	}
	return nil
}

func (f *Docker) ContainerRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ByName, id)
	return nil
}

func (f *Docker) ContainerInspect(_ context.Context, id string) (runtime.ContainerInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.ByName[id]
	if !ok {
		return runtime.ContainerInspect{}, &runtime.APIError{Status: 404, Message: "no such container"}
	}
	var ci runtime.ContainerInspect
	ci.Name = "/" + id
	ci.Config.Image = c.Config.Image
	ci.Config.Labels = c.Config.Labels
	ci.State.Running = c.Running
	ci.State.ExitCode = c.Exit
	ci.NetworkSettings.Networks = map[string]struct {
		IPAddress string
		Aliases   []string
	}{c.Config.Network: {IPAddress: id}}
	return ci, nil
}

// ContainerLogs returns one stdout line in docker's multiplexed format.
func (f *Docker) ContainerLogs(_ context.Context, id string, _ bool, _ string, _ time.Time) (io.ReadCloser, error) {
	line := "2026-01-01T00:00:00Z hello from " + id + "\n"
	hdr := []byte{1, 0, 0, 0, 0, 0, 0, byte(len(line))}
	return io.NopCloser(strings.NewReader(string(hdr) + line)), nil
}

// AddImage makes ref available locally.
func (f *Docker) AddImage(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Local[ref] = true
}

func (f *Docker) Containers(_ context.Context, labels ...string) ([]runtime.ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.ContainerSummary
	for name, c := range f.ByName {
		if !matches(c.Config.Labels, labels) {
			continue
		}
		state := "exited"
		if c.Running {
			state = "running"
		}
		out = append(out, runtime.ContainerSummary{ID: name, Names: []string{"/" + name}, State: state, Image: c.Config.Image, Labels: c.Config.Labels})
	}
	return out, nil
}

func (f *Docker) RunningWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, c := range f.ByName {
		if strings.HasPrefix(n, prefix) && c.Running {
			out = append(out, n)
		}
	}
	return out
}

// Prober: a container is healthy unless its image is in sick.
type Prober struct {
	D    *Docker
	Sick map[string]bool
}

func (p Prober) Probe(_ context.Context, u string) error {
	pu, _ := url.Parse(u)
	name := pu.Hostname()
	p.D.mu.Lock()
	defer p.D.mu.Unlock()
	c := p.D.ByName[name]
	if c == nil || p.Sick[c.Config.Image] {
		return errors.New("connection refused")
	}
	return nil
}

type Nginx struct{ Reloads int }

func (n *Nginx) Test(context.Context) (string, error) { return "", nil }
func (n *Nginx) Reload(context.Context) error         { n.Reloads++; return nil }

func (f *Docker) NetworkEnsure(context.Context, string, map[string]string) error { return nil }

func (f *Docker) NetworkConnect(_ context.Context, network, container string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.Networks[network], container) {
		f.Networks[network] = append(f.Networks[network], container)
	}
	return nil
}

func (f *Docker) VolumeEnsure(_ context.Context, name string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Vols[name]; !ok {
		f.Vols[name] = labels
	}
	return nil
}

func (f *Docker) Volumes(_ context.Context, labels ...string) ([]runtime.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Volume
	for name, vl := range f.Vols {
		if matches(vl, labels) {
			out = append(out, runtime.Volume{Name: name, Labels: vl})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Docker) VolumeRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Vols[name]; !ok {
		return &runtime.APIError{Status: 404, Message: "no such volume"}
	}
	delete(f.Vols, name)
	return nil
}

func (f *Docker) Exec(_ context.Context, id string, cmd []string) (string, int, error) {
	f.mu.Lock()
	f.Execs = append(f.Execs, id+": "+strings.Join(cmd, " "))
	fn := f.ExecFn
	f.mu.Unlock()
	if fn != nil {
		return fn(id, cmd)
	}
	return "", 0, nil
}

// Images lists local images whose tag matches reference (a glob like clowdr.local/*/*).
func (f *Docker) Images(_ context.Context, reference string) ([]runtime.ImageSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.ImageSummary
	for ref := range f.Local {
		repo := ref
		if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
			repo = ref[:i]
		}
		if ok, _ := path.Match(reference, repo); reference == "" || ok {
			out = append(out, runtime.ImageSummary{ID: ref, RepoTags: []string{ref}})
		}
	}
	return out, nil
}

func matches(have map[string]string, labels []string) bool {
	for _, l := range labels {
		k, v, hasV := strings.Cut(l, "=")
		if got, ok := have[k]; !ok || hasV && got != v {
			return false
		}
	}
	return true
}
