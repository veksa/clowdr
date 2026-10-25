// Package runtime is a small Docker Engine API client: only what clowdrd needs, without the
// docker SDK and its dependency tree.
package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const apiVersion = "v1.47"

// LabelManaged marks every container, network and volume clowdrd owns.
const (
	LabelManaged = "clowdr"
	LabelProject = "clowdr.project"
	LabelService = "clowdr.service"
	LabelRelease = "clowdr.release"
	LabelReplica = "clowdr.replica"
	LabelRole    = "clowdr.role" // service | nginx | addon
	LabelAddon   = "clowdr.addon"
)

var ErrNotFound = errors.New("not found")

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %s (%d)", e.Message, e.Status) }

func (e *APIError) Is(target error) bool { return target == ErrNotFound && e.Status == 404 }

type Docker struct {
	hc   *http.Client
	base string
}

// New connects to DOCKER_HOST, else /var/run/docker.sock (Docker Desktop's pipe on Windows).
func New() (*Docker, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = defaultHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST: %w", err)
	}
	tr := &http.Transport{MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second}
	base := "http://docker/" + apiVersion
	switch u.Scheme {
	case "unix":
		path := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	case "npipe":
		path := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dialPipe(ctx, path) }
	case "tcp", "http":
		base = "http://" + u.Host + "/" + apiVersion
	default:
		return nil, fmt.Errorf("DOCKER_HOST scheme %q is not supported", u.Scheme)
	}
	return &Docker{hc: &http.Client{Transport: tr}, base: base}, nil
}

func (d *Docker) do(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		if r, ok := body.(io.Reader); ok {
			rd = r
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			rd = bytes.NewReader(b)
		}
	}
	u := d.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var m struct{ Message string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(b))
		}
		return nil, &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	return resp, nil
}

// call does a request and decodes a JSON response into out (if not nil).
func (d *Docker) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := d.do(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// System

type Info struct {
	ServerVersion string
	Driver        string
	DriverStatus  [][2]string
	MemTotal      int64
	NCPU          int
}

func (d *Docker) Info(ctx context.Context) (Info, error) {
	var i Info
	return i, d.call(ctx, "GET", "/info", nil, nil, &i)
}

// ContainerdStore reports whether images live in the containerd image store, which the
// registry writes to directly.
func (i Info) ContainerdStore() bool {
	for _, kv := range i.DriverStatus {
		if kv[0] == "driver-type" && strings.HasPrefix(kv[1], "io.containerd.snapshotter") {
			return true
		}
	}
	return false
}

// Images

type ImageInspect struct {
	ID          string
	RepoTags    []string
	RepoDigests []string
	Size        int64
}

func (d *Docker) ImageInspect(ctx context.Context, ref string) (ImageInspect, error) {
	var i ImageInspect
	return i, d.call(ctx, "GET", "/images/"+ref+"/json", nil, nil, &i)
}

// ImagePull pulls ref (e.g. nginx:alpine) and waits for completion.
func (d *Docker) ImagePull(ctx context.Context, ref string) error {
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	if i := strings.Index(ref, "@"); i > 0 {
		name, tag = ref[:i], ref[i+1:]
	}
	resp, err := d.do(ctx, "POST", "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct{ Error string }
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, m.Error)
		}
	}
}

func (d *Docker) ImageRemove(ctx context.Context, ref string) error {
	return d.call(ctx, "DELETE", "/images/"+ref, nil, nil, nil)
}

type ImageSummary struct {
	ID          string
	RepoTags    []string
	RepoDigests []string
	Size        int64
	Created     int64
}

func (d *Docker) Images(ctx context.Context, reference string) ([]ImageSummary, error) {
	var out []ImageSummary
	q := url.Values{}
	if reference != "" {
		f, _ := json.Marshal(map[string][]string{"reference": {reference}})
		q.Set("filters", string(f))
	}
	return out, d.call(ctx, "GET", "/images/json", q, nil, &out)
}

// Containers

type ContainerConfig struct {
	Name     string
	Image    string
	Cmd      []string
	Env      []string
	Labels   map[string]string
	Memory   int64
	Restart  string // no | always | unless-stopped | on-failure
	Network  string
	Aliases  []string
	Mounts   []Mount
	Ports    []PortBinding
	Log      *LogConfig
	ReadOnly bool
	Tmpfs    map[string]string
	// Pids and NanoCPUs cap processes and CPU time, 0 = none. Hardened drops NET_RAW (ARP and
	// raw-socket tricks against neighbours in the network) and sets no-new-privileges.
	Pids     int64
	NanoCPUs int64
	Hardened bool
}

type Mount struct {
	Type     string `json:"Type"` // volume | bind | tmpfs
	Source   string `json:"Source,omitempty"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

type PortBinding struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Proto         string // tcp (default) | udp
}

type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

// ContainerCreate creates (does not start) a container and returns its ID.
func (d *Docker) ContainerCreate(ctx context.Context, c ContainerConfig) (string, error) {
	host := map[string]any{
		"Memory":         c.Memory,
		"RestartPolicy":  map[string]any{"Name": c.Restart},
		"Init":           true,
		"ReadonlyRootfs": c.ReadOnly,
	}
	if c.Memory > 0 {
		host["MemorySwap"] = c.Memory // no swap on top of the limit
	}
	if len(c.Mounts) > 0 {
		host["Mounts"] = c.Mounts
	}
	if c.Log != nil {
		host["LogConfig"] = c.Log
	}
	if len(c.Tmpfs) > 0 {
		host["Tmpfs"] = c.Tmpfs
	}
	if c.Pids > 0 {
		host["PidsLimit"] = c.Pids
	}
	if c.NanoCPUs > 0 {
		host["NanoCpus"] = c.NanoCPUs
	}
	if c.Hardened {
		host["CapDrop"] = []string{"NET_RAW"}
		host["SecurityOpt"] = []string{"no-new-privileges"}
	}
	exposed := map[string]struct{}{}
	if len(c.Ports) > 0 {
		pb := map[string][]map[string]string{}
		for _, p := range c.Ports {
			proto := p.Proto
			if proto == "" {
				proto = "tcp"
			}
			k := fmt.Sprintf("%d/%s", p.ContainerPort, proto)
			exposed[k] = struct{}{}
			pb[k] = append(pb[k], map[string]string{"HostIp": p.HostIP, "HostPort": fmt.Sprint(p.HostPort)})
		}
		host["PortBindings"] = pb
	}
	body := map[string]any{
		"Image":      c.Image,
		"Env":        c.Env,
		"Labels":     c.Labels,
		"HostConfig": host,
	}
	if len(exposed) > 0 {
		body["ExposedPorts"] = exposed
	}
	if len(c.Cmd) > 0 {
		body["Cmd"] = c.Cmd
	}
	if c.Network != "" {
		host["NetworkMode"] = c.Network
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{c.Network: map[string]any{"Aliases": c.Aliases}},
		}
	}
	var out struct{ Id string }
	err := d.call(ctx, "POST", "/containers/create", url.Values{"name": {c.Name}}, body, &out)
	return out.Id, err
}

func (d *Docker) ContainerStart(ctx context.Context, id string) error {
	return d.call(ctx, "POST", "/containers/"+id+"/start", nil, nil, nil)
}

// ContainerStop sends SIGTERM and SIGKILL after grace.
func (d *Docker) ContainerStop(ctx context.Context, id string, grace time.Duration) error {
	err := d.call(ctx, "POST", "/containers/"+id+"/stop", url.Values{"t": {fmt.Sprint(int(grace.Seconds()))}}, nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 304 { // already stopped
		return nil
	}
	return err
}

func (d *Docker) ContainerRemove(ctx context.Context, id string) error {
	err := d.call(ctx, "DELETE", "/containers/"+id, url.Values{"force": {"true"}, "v": {"false"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

type ContainerInspect struct {
	ID    string `json:"Id"`
	Name  string
	Image string
	State struct {
		Status     string // created | running | restarting | exited | dead
		Running    bool
		Restarting bool
		OOMKilled  bool
		ExitCode   int
		StartedAt  time.Time
		FinishedAt time.Time
		Health     *struct{ Status string }
	}
	RestartCount int
	Config       struct {
		Image  string
		Labels map[string]string
		Env    []string
	}
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string
			Aliases   []string
		}
	}
}

func (d *Docker) ContainerInspect(ctx context.Context, id string) (ContainerInspect, error) {
	var c ContainerInspect
	return c, d.call(ctx, "GET", "/containers/"+id+"/json", nil, nil, &c)
}

type ContainerSummary struct {
	ID      string `json:"Id"`
	Names   []string
	Image   string
	State   string
	Status  string
	Labels  map[string]string
	Created int64
}

func (s ContainerSummary) Name() string {
	if len(s.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(s.Names[0], "/")
}

// Containers lists containers (including stopped) matching all labels ("k" or "k=v").
func (d *Docker) Containers(ctx context.Context, labels ...string) ([]ContainerSummary, error) {
	f, _ := json.Marshal(map[string][]string{"label": labels})
	var out []ContainerSummary
	return out, d.call(ctx, "GET", "/containers/json", url.Values{"all": {"true"}, "filters": {string(f)}}, nil, &out)
}

// ContainerLogs returns the raw multiplexed stream; use Demux to split it.
func (d *Docker) ContainerLogs(ctx context.Context, id string, follow bool, tail string, since time.Time) (io.ReadCloser, error) {
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}, "timestamps": {"true"}}
	if follow {
		q.Set("follow", "true")
	}
	if tail != "" {
		q.Set("tail", tail)
	}
	if !since.IsZero() {
		q.Set("since", fmt.Sprint(since.Unix()))
	}
	resp, err := d.do(ctx, "GET", "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Demux splits docker's stdout/stderr frame stream into lines and calls fn for each.
func Demux(r io.Reader, fn func(stderr bool, line string)) error {
	br := bufio.NewReader(r)
	var hdr [8]byte
	var partial [3]string
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			for i, p := range partial {
				if p != "" {
					fn(i == 2, p)
				}
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		stream := hdr[0]
		if stream > 2 {
			stream = 1
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		data := partial[stream] + string(buf)
		for {
			i := strings.IndexByte(data, '\n')
			if i < 0 {
				break
			}
			fn(stream == 2, data[:i])
			data = data[i+1:]
		}
		partial[stream] = data
	}
}

// Exec runs cmd in a container and returns combined output and exit code.
func (d *Docker) Exec(ctx context.Context, id string, cmd []string) (string, int, error) {
	var created struct{ Id string }
	err := d.call(ctx, "POST", "/containers/"+id+"/exec", nil, map[string]any{
		"Cmd": cmd, "AttachStdout": true, "AttachStderr": true,
	}, &created)
	if err != nil {
		return "", -1, err
	}
	resp, err := d.do(ctx, "POST", "/exec/"+created.Id+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return "", -1, err
	}
	var out strings.Builder
	err = Demux(resp.Body, func(_ bool, line string) { out.WriteString(line + "\n") })
	resp.Body.Close()
	if err != nil {
		return out.String(), -1, err
	}
	var st struct{ ExitCode int }
	if err := d.call(ctx, "GET", "/exec/"+created.Id+"/json", nil, nil, &st); err != nil {
		return out.String(), -1, err
	}
	return out.String(), st.ExitCode, nil
}

// Networks

type NetworkInspect struct {
	ID         string `json:"Id"`
	Name       string
	Labels     map[string]string
	Containers map[string]struct{ Name string }
}

func (d *Docker) NetworkInspect(ctx context.Context, name string) (NetworkInspect, error) {
	var n NetworkInspect
	return n, d.call(ctx, "GET", "/networks/"+name, nil, nil, &n)
}

// NetworkEnsure creates a bridge network if it does not exist.
func (d *Docker) NetworkEnsure(ctx context.Context, name string, labels map[string]string) error {
	if _, err := d.NetworkInspect(ctx, name); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	err := d.call(ctx, "POST", "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "Labels": labels, "CheckDuplicate": true,
	}, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 409 {
		return nil
	}
	return err
}

func (d *Docker) NetworkConnect(ctx context.Context, network, container string, aliases []string) error {
	err := d.call(ctx, "POST", "/networks/"+network+"/connect", nil, map[string]any{
		"Container": container, "EndpointConfig": map[string]any{"Aliases": aliases},
	}, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 403 && strings.Contains(ae.Message, "already exists") {
		return nil
	}
	return err
}

func (d *Docker) NetworkDisconnect(ctx context.Context, network, container string) error {
	err := d.call(ctx, "POST", "/networks/"+network+"/disconnect", nil, map[string]any{"Container": container, "Force": true}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (d *Docker) NetworkRemove(ctx context.Context, name string) error {
	err := d.call(ctx, "DELETE", "/networks/"+name, nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Volumes

type Volume struct {
	Name      string
	Labels    map[string]string
	CreatedAt string
}

func (d *Docker) VolumeEnsure(ctx context.Context, name string, labels map[string]string) error {
	return d.call(ctx, "POST", "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

func (d *Docker) Volumes(ctx context.Context, labels ...string) ([]Volume, error) {
	f, _ := json.Marshal(map[string][]string{"label": labels})
	var out struct{ Volumes []Volume }
	return out.Volumes, d.call(ctx, "GET", "/volumes", url.Values{"filters": {string(f)}}, nil, &out)
}

func (d *Docker) VolumeRemove(ctx context.Context, name string) error {
	return d.call(ctx, "DELETE", "/volumes/"+name, nil, nil, nil)
}

// Stats

type Stats struct {
	MemoryUsage int64
	MemoryLimit int64
	CPUPercent  float64
}

func (d *Docker) Stats(ctx context.Context, id string) (Stats, error) {
	var s struct {
		CPUStats struct {
			CPUUsage    struct{ TotalUsage uint64 } `json:"cpu_usage"`
			SystemUsage uint64                      `json:"system_cpu_usage"`
			OnlineCPUs  int                         `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage    struct{ TotalUsage uint64 } `json:"cpu_usage"`
			SystemUsage uint64                      `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage int64            `json:"usage"`
			Limit int64            `json:"limit"`
			Stats map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := d.call(ctx, "GET", "/containers/"+id+"/stats", url.Values{"stream": {"false"}}, nil, &s); err != nil {
		return Stats{}, err
	}
	out := Stats{MemoryUsage: s.MemoryStats.Usage - s.MemoryStats.Stats["inactive_file"], MemoryLimit: s.MemoryStats.Limit}
	cpu := float64(s.CPUStats.CPUUsage.TotalUsage - s.PreCPUStats.CPUUsage.TotalUsage)
	sys := float64(s.CPUStats.SystemUsage - s.PreCPUStats.SystemUsage)
	if sys > 0 && cpu > 0 {
		out.CPUPercent = cpu / sys * float64(max(s.CPUStats.OnlineCPUs, 1)) * 100
	}
	return out, nil
}

// Event is a docker engine event.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	TimeNano int64 `json:"timeNano"`
}

// Events streams container events matching labels until ctx ends or the stream breaks.
func (d *Docker) Events(ctx context.Context, actions []string, labels ...string) (<-chan Event, <-chan error) {
	out := make(chan Event, 64)
	errc := make(chan error, 1)
	f, _ := json.Marshal(map[string][]string{"type": {"container"}, "event": actions, "label": labels})
	go func() {
		defer close(out)
		resp, err := d.do(ctx, "GET", "/events", url.Values{"filters": {string(f)}}, nil)
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var ev Event
			if err := dec.Decode(&ev); err != nil {
				errc <- err
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
	}()
	return out, errc
}
