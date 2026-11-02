// Package deploy runs releases: start the new containers next to the old ones, wait for
// health, switch nginx upstreams, drain and remove the old containers.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/veksa/clowdr/internal/auth"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/registry"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

// Docker is the subset of the docker client the engine uses.
type Docker interface {
	Info(ctx context.Context) (runtime.Info, error)
	ImageInspect(ctx context.Context, ref string) (runtime.ImageInspect, error)
	ImagePull(ctx context.Context, ref string) error
	ImageRemove(ctx context.Context, ref string) error
	ImageTag(ctx context.Context, ref, target string) error
	ContainerCreate(ctx context.Context, c runtime.ContainerConfig) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, grace time.Duration) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (runtime.ContainerInspect, error)
	ContainerLogs(ctx context.Context, id string, follow bool, tail string, since time.Time) (io.ReadCloser, error)
	Containers(ctx context.Context, labels ...string) ([]runtime.ContainerSummary, error)
	NetworkEnsure(ctx context.Context, name string, labels map[string]string) error
	NetworkConnect(ctx context.Context, network, container string, aliases []string) error
	NetworkDisconnect(ctx context.Context, network, container string) error
	NetworkRemove(ctx context.Context, name string) error
	VolumeEnsure(ctx context.Context, name string, labels map[string]string) error
	Volumes(ctx context.Context, labels ...string) ([]runtime.Volume, error)
	VolumeRemove(ctx context.Context, name string) error
	Exec(ctx context.Context, id string, cmd []string) (string, int, error)
}

// Prober checks a service's health endpoint.
type Prober interface {
	Probe(ctx context.Context, url string) error
}

type httpProber struct{ c *http.Client }

func (p httpProber) Probe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.c.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

type Engine struct {
	Docker   Docker
	Store    *store.Store
	Platform func() *config.Platform
	Ingress  *ingress.Controller
	Prober   Prober
	Log      *slog.Logger
	// CertExists reports whether a certificate for domain is installed.
	CertExists func(domain string) bool
	// OnEvent records deploy results (events journal, alerts). Optional.
	OnEvent func(ctx context.Context, project, service, kind string, failed bool, msg string)
	// RoutesChanged is called after a project's nginx config changed (ACME picks up new domains).
	RoutesChanged func()
	// LogConfig sets the docker log driver of service containers (fluentd into clowdrd);
	// nil keeps json-file.
	LogConfig func(project, service string, release uint64, replica int) *runtime.LogConfig
	// LogTail reads the last lines of a container from the log store; nil uses docker logs.
	LogTail func(project, service string, release uint64, replica, n int) []string
	// Syslog makes nginx send access and error logs to clowdrd.
	Syslog bool
	// MoveLogs moves a renamed project's logs, traces and metrics to its new name. Optional.
	MoveLogs func(old, name string)

	locks   sync.Map // project/service -> *sync.Mutex
	gates   sync.Map // project -> *sync.RWMutex: operations hold it shared, a rename exclusively
	renames sync.Map // new project name -> old, while a rename is under way
	renameM sync.Mutex
	pushed  sync.Map // image -> time.Time of the last push
	renderM sync.Mutex
	netM    sync.Mutex // expose_to connects vs pruneForeign
	// quotaM guards quota checks and reserved: memory of deploys in flight, project/service -> bytes,
	// so two deploys of a project cannot both fit a quota that holds only one.
	quotaM   sync.Mutex
	reserved map[string]int64
	drains   sync.WaitGroup
}

func New(d Docker, st *store.Store, platform func() *config.Platform, ing *ingress.Controller, log *slog.Logger) *Engine {
	return &Engine{
		Docker: d, Store: st, Platform: platform, Ingress: ing, Log: log,
		Prober: httpProber{c: &http.Client{Timeout: 5 * time.Second}},
		CertExists: func(domain string) bool {
			_, err := os.Stat(filepath.Join(ingress.CertDir, domain, "fullchain.pem"))
			return err == nil
		},
	}
}

func (e *Engine) lock(project, service string) func() {
	g := e.gate(project)
	g.RLock()
	m, _ := e.locks.LoadOrStore(project+"/"+service, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return func() {
		mu.Unlock()
		g.RUnlock()
	}
}

func (e *Engine) gate(project string) *sync.RWMutex {
	g, _ := e.gates.LoadOrStore(project, &sync.RWMutex{})
	return g.(*sync.RWMutex)
}

// Progress receives human-readable deploy steps, streamed to the CLI.
type Progress func(format string, args ...any)

// Request is one deploy.
type Request struct {
	Project string
	Spec    spec.Service
	Image   string // image reference to run
	Digest  string
	Version string
	Author  string
	// Rollback marks the release in history.
	Rollback bool
}

var (
	ErrHealth = errors.New("health check failed")
	ErrQuota  = errors.New("project memory quota exceeded")
)

func NetworkName(project string) string { return "clowdr-" + project }

func VolumeName(project, name string) string { return project + "_" + name }

func containerName(project, service string, release uint64, replica int) string {
	return fmt.Sprintf("%s-%s-r%d-%d", project, service, release, replica)
}

var (
	imageID = regexp.MustCompile(`^(sha256:)?[0-9a-f]+$`)
	// imageRef is docker's reference grammar: [host[:port]/]path[:tag][@sha256:digest].
	imageRef = regexp.MustCompile(`^(?:([a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*(?::[0-9]+)?)/)?` +
		`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
		`(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)
)

// CheckImage rejects image references that would run another project's image: pushed images
// live in docker as clowdr.local/<project>/..., and docker also resolves a bare image ID, or
// any unique prefix of one, to whatever local image it names.
func CheckImage(project, ref string) error {
	r := strings.ToLower(strings.TrimSpace(ref))
	if imageID.MatchString(r) {
		return fmt.Errorf("image %s: refer to images by name, not by ID", ref)
	}
	if strings.HasPrefix(r, registry.ImagePrefix) && !strings.HasPrefix(r, registry.ImagePrefix+project+"/") {
		return fmt.Errorf("image %s belongs to another project", ref)
	}
	// The ref goes into docker API paths and makes dockerd connect to its registry: it has to
	// be a plain reference, from a registry outside the host and its private networks
	// (docker talks plain http to 127.0.0.0/8).
	if !imageRef.MatchString(ref) {
		return fmt.Errorf("image %q is not a valid image reference", ref)
	}
	if first, _, ok := strings.Cut(r, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host := first
		if h, _, err := net.SplitHostPort(first); err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
			ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
			return fmt.Errorf("image %s: registry %s is on the host or a private network", ref, first)
		}
	}
	return nil
}

// CheckSpec validates a spec against the project and the rest of the host.
func (e *Engine) CheckSpec(project string, s spec.Service) error {
	if err := s.Validate(); err != nil {
		return err
	}
	p, err := e.Store.Project(project)
	if err != nil {
		return fmt.Errorf("project %s does not exist on this host", project)
	}
	for _, d := range s.Domains {
		if !auth.DomainAllowed(p.Domains, d) {
			return fmt.Errorf("domain %s is not allowed for project %s (allowed: %s)", d, project, strings.Join(p.Domains, ", "))
		}
	}
	// memory 0 is no limit: with a quota it would let the project take the whole host.
	if p.Memory > 0 && s.Memory <= 0 && s.Replicas > 0 {
		return fmt.Errorf("service %s: memory is required, project %s has a %s quota", s.Name, project, config.FormatBytes(p.Memory))
	}
	if s.NginxExtra != "" || hasRouteExtra(s) {
		if !p.NginxExtra {
			return fmt.Errorf("nginx_extra is not enabled for project %s (host admin: project create --nginx-extra)", project)
		}
	}
	plat := e.Platform()
	for _, u := range s.Uses {
		if !contains(p.Addons, u) && !plat.AddonAllowed(u, project) {
			return fmt.Errorf("addon %s is not allowed for project %s", u, project)
		}
		if _, ok := plat.Addons[u]; !ok {
			return fmt.Errorf("addon %s is not configured on this host", u)
		}
	}
	// A domain may only be served by one service on the host.
	all, err := e.Store.Services("")
	if err != nil {
		return err
	}
	// expose_to puts the container into the other project's network, next to its services
	// and addons: only with that project's consent (accept_from in its deploy.yml).
	for _, other := range s.ExposeTo {
		if other == project {
			return fmt.Errorf("expose_to %s: the service is already in its own project's network", other)
		}
		if !e.accepts(all, other, project) {
			return fmt.Errorf("expose_to %s: project %s does not accept %s (accept_from in its deploy.yml)", other, other, project)
		}
	}
	for _, o := range all {
		if o.Project == project && o.Name == s.Name {
			continue
		}
		for _, d := range o.Spec.Domains {
			if contains(s.Domains, d) {
				return fmt.Errorf("domain %s is already served by %s/%s", d, o.Project, o.Name)
			}
		}
	}
	if d := plat.DeployHost(); d != "" && contains(s.Domains, d) {
		return fmt.Errorf("domain %s is reserved for clowdr", d)
	}
	return nil
}

// accepts reports whether a deployed service of project lists from in accept_from.
func (e *Engine) accepts(all []store.Service, project, from string) bool {
	// Before a rename's switch the specs still name the project by its old name.
	old, renaming := e.renames.Load(from)
	for _, o := range all {
		if o.Project != project {
			continue
		}
		if contains(o.Spec.AcceptFrom, from) || (renaming && contains(o.Spec.AcceptFrom, old.(string))) {
			return true
		}
	}
	return false
}

// exposeTo connects a container to another project's network if that project still accepts it.
// The check and the connect are one step under netM, as is pruneForeign, so a connect racing a
// revoked accept_from is either refused here or undone by the prune that follows.
func (e *Engine) exposeTo(ctx context.Context, project, other, id string, aliases []string) error {
	e.netM.Lock()
	defer e.netM.Unlock()
	all, err := e.Store.Services("")
	if err != nil {
		return err
	}
	if !e.accepts(all, other, project) {
		return fmt.Errorf("expose_to %s: project %s no longer accepts %s", other, other, project)
	}
	return e.Docker.NetworkConnect(ctx, NetworkName(other), id, aliases)
}

// pruneForeign disconnects other projects' containers from project's network once project no
// longer accepts them.
func (e *Engine) pruneForeign(ctx context.Context, project string) {
	e.netM.Lock()
	defer e.netM.Unlock()
	all, err := e.Store.Services("")
	if err != nil {
		return
	}
	cs, err := e.Docker.Containers(ctx, runtime.LabelManaged, runtime.LabelRole+"=service")
	if err != nil {
		return
	}
	net := NetworkName(project)
	for _, c := range cs {
		from := c.Labels[runtime.LabelProject]
		if from == project || e.accepts(all, project, from) {
			continue
		}
		ci, err := e.Docker.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}
		if _, ok := ci.NetworkSettings.Networks[net]; !ok {
			continue
		}
		if err := e.Docker.NetworkDisconnect(ctx, net, c.ID); err != nil {
			e.Log.Warn("disconnect foreign container", "network", net, "container", c.Name(), "err", err)
		} else {
			e.Log.Info("disconnected container no longer accepted", "network", net, "container", c.Name())
		}
	}
}

func hasRouteExtra(s spec.Service) bool {
	for _, r := range s.Routes {
		if r.NginxExtra != "" {
			return true
		}
	}
	return false
}

// Deploy runs a release to completion (or failure). The old containers are drained in the
// background after the switch.
func (e *Engine) Deploy(ctx context.Context, req Request, progress Progress) (store.Release, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	s := req.Spec
	s.Defaults()
	// The lock comes first: a deploy that waited for a rename has to see the names it left behind.
	unlock := e.lock(req.Project, s.Name)
	defer unlock()
	if err := e.CheckSpec(req.Project, s); err != nil {
		return store.Release{}, err
	}
	if err := CheckImage(req.Project, req.Image); err != nil {
		return store.Release{}, err
	}

	rel := store.Release{
		Project: req.Project, Service: s.Name, Image: req.Image, Digest: req.Digest,
		Version: req.Version, Author: req.Author, Status: store.StatusDeploying,
		Spec: s, Rollback: req.Rollback,
	}
	if err := e.Store.CreateRelease(&rel); err != nil {
		return rel, err
	}
	start := time.Now()
	fail := func(err error) (store.Release, error) {
		rel.Status = store.StatusFailed
		rel.Error = err.Error()
		rel.Finished = time.Now().UTC()
		e.Store.PutRelease(rel)
		e.event(ctx, req.Project, s.Name, "deploy", true, fmt.Sprintf("r%d %s failed: %v", rel.ID, short(req.Version), err))
		return rel, err
	}
	progress("release r%d: %s", rel.ID, req.Image)

	svc, err := e.Store.Service(req.Project, s.Name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fail(err)
	}
	var old *store.Release
	if svc.Current != 0 {
		if r, err := e.Store.Release(req.Project, s.Name, svc.Current); err == nil {
			old = &r
		}
	}

	if err := e.ensureImage(ctx, req.Project, req.Image, progress); err != nil {
		return fail(err)
	}
	if err := e.prepare(ctx, req.Project, s, progress); err != nil {
		return fail(err)
	}

	stopFirst, err := e.planMemory(ctx, req.Project, s, old)
	if err != nil {
		return fail(err)
	}
	defer e.unreserve(req.Project, s.Name)
	if stopFirst && old != nil {
		progress("stop-first: stopping r%d before starting r%d", old.ID, rel.ID)
		for _, c := range old.Containers {
			e.Docker.ContainerStop(ctx, c, old.Spec.StopGrace)
		}
	}

	env := e.containerEnv(req.Project, s, svc.Env)
	var started []string
	cleanup := func() {
		for _, c := range started {
			e.Docker.ContainerRemove(context.WithoutCancel(ctx), c)
		}
		if stopFirst && old != nil {
			progress("starting previous release r%d again", old.ID)
			for _, c := range old.Containers {
				e.Docker.ContainerStart(context.WithoutCancel(ctx), c)
			}
		}
	}
	for i := 1; i <= s.Replicas; i++ {
		name := containerName(req.Project, s.Name, rel.ID, i)
		_, err := e.startContainer(ctx, req.Project, s, rel.ID, i, name, req.Image, env)
		if err != nil {
			cleanup()
			return fail(fmt.Errorf("start %s: %w", name, err))
		}
		started = append(started, name)
		progress("started %s", name)
	}
	rel.Containers = started
	e.Store.PutRelease(rel)

	for i, c := range started {
		if err := e.waitHealthy(ctx, req.Project, s, c, progress); err != nil {
			tail := e.tail(ctx, req.Project, s.Name, rel.ID, i+1, c, 20)
			if tail != "" {
				progress("last log lines of %s:\n%s", c, tail)
			}
			cleanup()
			return fail(err)
		}
	}

	// Switch: state first (the renderer reads it), then nginx; on nginx failure revert both.
	prevSvc := svc
	_, err = e.Store.UpdateService(req.Project, s.Name, func(v *store.Service) error {
		v.Spec = s
		if v.Current != rel.ID {
			v.Previous = v.Current
		}
		v.Current = rel.ID
		v.Stopped = false
		return nil
	})
	if err != nil {
		cleanup()
		return fail(err)
	}
	if err := e.RenderProject(ctx, req.Project); err != nil {
		if prevSvc.Name == "" {
			e.Store.DeleteService(req.Project, s.Name)
			e.Store.PutRelease(rel) // DeleteService dropped the release history too
		} else {
			e.Store.PutService(prevSvc)
		}
		e.RenderProject(context.WithoutCancel(ctx), req.Project)
		cleanup()
		return fail(err)
	}
	e.pruneForeign(ctx, req.Project)
	if s.Port != 0 {
		progress("traffic switched to r%d", rel.ID)
	} else {
		progress("r%d is live", rel.ID)
	}

	rel.Status = store.StatusActive
	rel.Finished = time.Now().UTC()
	e.Store.PutRelease(rel)
	if old != nil {
		old.Status = store.StatusSuperseded
		e.Store.PutRelease(*old)
		e.drain(*old, stopFirst, progress)
	}
	e.gc(context.WithoutCancel(ctx), req.Project, s.Name)
	verb := "deployed"
	if req.Rollback {
		verb = "rolled back"
	}
	e.event(ctx, req.Project, s.Name, "deploy", false, fmt.Sprintf("r%d %s %s by %s in %s", rel.ID, short(req.Version), verb, req.Author, time.Since(start).Round(time.Second)))
	return rel, nil
}

func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

func (e *Engine) event(ctx context.Context, project, service, kind string, failed bool, msg string) {
	if e.OnEvent != nil {
		e.OnEvent(context.WithoutCancel(ctx), project, service, kind, failed, msg)
	}
}

// ensureImage makes ref available. Pushed images must already be there. Any other reference is
// pulled every time, even when docker has an image of that name: a local copy may have been
// built on the host by someone else (a compose project, another tool) and carry their code and
// secrets. A local copy is used only if the pull fails and this project ran that image before.
func (e *Engine) ensureImage(ctx context.Context, project, ref string, progress Progress) error {
	if strings.HasPrefix(ref, registry.ImagePrefix) {
		if _, err := e.Docker.ImageInspect(ctx, ref); err != nil {
			return fmt.Errorf("image %s is not on the server (push it first)", ref)
		}
		return nil
	}
	progress("pulling %s", ref)
	err := e.Docker.ImagePull(ctx, ref)
	if err == nil {
		return nil
	}
	if _, ierr := e.Docker.ImageInspect(ctx, ref); ierr == nil && e.ranImage(project, ref) {
		progress("pull failed (%v), using the local copy this project ran before", err)
		return nil
	}
	return fmt.Errorf("pull %s: %w", ref, err)
}

func (e *Engine) ranImage(project, ref string) bool {
	svcs, _ := e.Store.Services(project)
	for _, s := range svcs {
		rels, _ := e.Store.Releases(project, s.Name)
		for _, r := range rels {
			if r.Image == ref {
				return true
			}
		}
	}
	return false
}

// prepare creates the project network, volumes and addons the service needs.
func (e *Engine) prepare(ctx context.Context, project string, s spec.Service, progress Progress) error {
	labels := map[string]string{runtime.LabelManaged: "1", runtime.LabelProject: project}
	if err := e.Docker.NetworkEnsure(ctx, NetworkName(project), labels); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if err := e.connectNginx(ctx, project); err != nil {
		return err
	}
	for _, v := range s.Volumes {
		if err := e.Docker.VolumeEnsure(ctx, VolumeName(project, v.Name), labels); err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
	}
	for _, a := range s.Uses {
		if err := e.EnsureAddon(ctx, project, a, progress); err != nil {
			return fmt.Errorf("addon %s: %w", a, err)
		}
	}
	return nil
}

// planMemory decides between start-first and stop-first. Start-first needs room for one more
// set of replicas both on the host and in the project quota.
func (e *Engine) planMemory(ctx context.Context, project string, s spec.Service, old *store.Release) (stopFirst bool, err error) {
	need := int64(s.Replicas) * s.Memory
	// The check and the reservation are one step; Deploy drops the reservation once the
	// service record holds the new spec.
	e.quotaM.Lock()
	used, err := e.projectMemory(project, s.Name)
	if err != nil {
		e.quotaM.Unlock()
		return false, err
	}
	p, perr := e.Store.Project(project)
	if perr == nil && p.Memory > 0 && used+need > p.Memory {
		e.quotaM.Unlock()
		return false, fmt.Errorf("%w: %s used + %s new > %s", ErrQuota,
			config.FormatBytes(used), config.FormatBytes(need), config.FormatBytes(p.Memory))
	}
	e.reserve(project, s.Name, need)
	e.quotaM.Unlock()

	if s.Switch == spec.SwitchStopFirst {
		return true, nil
	}
	if old == nil || len(old.Containers) == 0 {
		return false, nil
	}
	oldMem := int64(len(old.Containers)) * old.Spec.Memory
	if perr == nil && p.Memory > 0 && used+need+oldMem > p.Memory {
		return true, nil
	}
	free, err := e.freeMemory(ctx)
	if err != nil {
		return false, err
	}
	if free >= 0 && need > free {
		return true, nil
	}
	return false, nil
}

// reserve records a deploy in flight; callers hold quotaM.
func (e *Engine) reserve(project, service string, n int64) {
	if e.reserved == nil {
		e.reserved = map[string]int64{}
	}
	e.reserved[project+"/"+service] = n
}

func (e *Engine) unreserve(project, service string) {
	e.quotaM.Lock()
	delete(e.reserved, project+"/"+service)
	e.quotaM.Unlock()
}

// projectMemory sums memory limits of the project's running services except one, counting a
// service with a deploy in flight at the larger of its current and reserved memory. Callers
// hold quotaM.
func (e *Engine) projectMemory(project, except string) (int64, error) {
	svcs, err := e.Store.Services(project)
	if err != nil {
		return 0, err
	}
	mem := map[string]int64{}
	for _, v := range svcs {
		if !v.Stopped {
			mem[v.Name] = int64(v.Spec.Replicas) * v.Spec.Memory
		}
	}
	for k, n := range e.reserved {
		if name, ok := strings.CutPrefix(k, project+"/"); ok && n > mem[name] {
			mem[name] = n
		}
	}
	var sum int64
	for name, n := range mem {
		if name != except {
			sum += n
		}
	}
	return sum, nil
}

// freeMemory estimates memory not promised to any clowdr container: total minus the
// platform reserve minus every running container's limit. -1 when unknown.
func (e *Engine) freeMemory(ctx context.Context) (int64, error) {
	info, err := e.Docker.Info(ctx)
	if err != nil || info.MemTotal == 0 {
		return -1, nil
	}
	cs, err := e.Docker.Containers(ctx, runtime.LabelManaged)
	if err != nil {
		return -1, err
	}
	var promised int64
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		promised += memoryLabel(c.Labels)
	}
	reserve := int64(e.Platform().Limits.ReserveMemory)
	return info.MemTotal - reserve - promised, nil
}

const labelMemory = "clowdr.memory"

func memoryLabel(l map[string]string) int64 {
	v, _ := config.ParseBytes(l[labelMemory])
	return v
}

func (e *Engine) containerEnv(project string, s spec.Service, values map[string]string) []string {
	var env []string
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+values[k])
	}
	plat := e.Platform()
	for _, u := range s.Uses {
		if u == "redis" {
			port := plat.Addons[u].Port
			env = append(env, fmt.Sprintf("REDIS_URL=redis://redis:%d", port))
		}
	}
	for _, ext := range plat.External {
		if sh, ok := ext.Share[project]; ok {
			for k, v := range sh.Env {
				if _, set := values[k]; !set {
					env = append(env, k+"="+v)
				}
			}
		}
	}
	env = append(env, "CLOWDR_PROJECT="+project, "CLOWDR_SERVICE="+s.Name)
	return env
}

func (e *Engine) startContainer(ctx context.Context, project string, s spec.Service, release uint64, replica int, name, image string, env []string) (string, error) {
	labels := map[string]string{
		runtime.LabelManaged: "1",
		runtime.LabelProject: project,
		runtime.LabelService: s.Name,
		runtime.LabelRelease: fmt.Sprint(release),
		runtime.LabelReplica: fmt.Sprint(replica),
		runtime.LabelRole:    "service",
		labelMemory:          fmt.Sprint(s.Memory),
	}
	var mounts []runtime.Mount
	for _, v := range s.Volumes {
		mounts = append(mounts, runtime.Mount{Type: "volume", Source: VolumeName(project, v.Name), Target: v.Target, ReadOnly: v.RO})
	}
	id, err := e.Docker.ContainerCreate(ctx, runtime.ContainerConfig{
		Name:     name,
		Image:    image,
		Env:      env,
		Labels:   labels,
		Memory:   s.Memory,
		Restart:  "unless-stopped",
		Network:  NetworkName(project),
		Aliases:  []string{s.Name},
		Mounts:   mounts,
		Log:      e.logConfig(project, s.Name, release, replica),
		Pids:     e.Platform().Limits.Pids,
		NanoCPUs: int64(e.Platform().Limits.CPUs * 1e9),
		Hardened: true,
	})
	if err != nil {
		return "", err
	}
	for _, other := range s.ExposeTo {
		if _, err := e.Store.Project(other); err != nil {
			continue
		}
		if err := e.Docker.NetworkEnsure(ctx, NetworkName(other), map[string]string{runtime.LabelManaged: "1", runtime.LabelProject: other}); err != nil {
			return id, err
		}
		if err := e.exposeTo(ctx, project, other, id, []string{project + "-" + s.Name}); err != nil {
			return id, err
		}
	}
	if err := e.Docker.ContainerStart(ctx, id); err != nil {
		e.Docker.ContainerRemove(ctx, id)
		return "", err
	}
	return id, nil
}

// waitHealthy waits for `successes` consecutive good probes. A container that exits or
// restarts fails immediately.
func (e *Engine) waitHealthy(ctx context.Context, project string, s spec.Service, container string, progress Progress) error {
	deadline := time.Now().Add(s.Health.Timeout)
	ok := 0
	var lastErr error
	for {
		ci, err := e.Docker.ContainerInspect(ctx, container)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrHealth, container, err)
		}
		if ci.State.OOMKilled {
			return fmt.Errorf("%w: %s was killed: out of memory (limit %s)", ErrHealth, container, config.FormatBytes(s.Memory))
		}
		if !ci.State.Running || ci.State.Restarting || ci.RestartCount > 0 {
			return fmt.Errorf("%w: %s exited with code %d", ErrHealth, container, ci.State.ExitCode)
		}
		if s.Health.Path == "" {
			// No HTTP check: running for two intervals is enough.
			ok++
		} else {
			ip := ci.NetworkSettings.Networks[NetworkName(project)].IPAddress
			url := fmt.Sprintf("http://%s:%d%s", ip, s.Port, s.Health.Path)
			pctx, cancel := context.WithTimeout(ctx, max(s.Health.Interval, 2*time.Second))
			err := e.Prober.Probe(pctx, url)
			cancel()
			if err == nil {
				ok++
			} else {
				ok, lastErr = 0, err
			}
		}
		if ok >= s.Health.Successes {
			progress("%s is healthy", container)
			return nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("%w: %s did not answer %s within %s: %v", ErrHealth, container, s.Health.Path, s.Health.Timeout, lastErr)
			}
			return fmt.Errorf("%w: %s not healthy within %s", ErrHealth, container, s.Health.Timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.Health.Interval):
		}
	}
}

func (e *Engine) tail(ctx context.Context, project, service string, release uint64, replica int, container string, n int) string {
	if e.LogTail != nil {
		// The log driver delivers asynchronously; give the last lines a moment to arrive.
		time.Sleep(500 * time.Millisecond)
		var b strings.Builder
		for _, l := range e.LogTail(project, service, release, replica, n) {
			b.WriteString("  " + l + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	rc, err := e.Docker.ContainerLogs(context.WithoutCancel(ctx), container, false, fmt.Sprint(n), time.Time{})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var b strings.Builder
	runtime.Demux(rc, func(_ bool, line string) { b.WriteString("  " + line + "\n") })
	return strings.TrimRight(b.String(), "\n")
}

// drain stops and removes the old release's containers after its drain period.
func (e *Engine) drain(old store.Release, alreadyStopped bool, progress Progress) {
	wait := old.Spec.Drain
	if alreadyStopped {
		wait = 0
	}
	progress("draining r%d for %s, then removing it", old.ID, wait)
	e.drains.Add(1)
	go func() {
		defer e.drains.Done()
		time.Sleep(wait)
		ctx := context.Background()
		for _, c := range old.Containers {
			if err := e.Docker.ContainerStop(ctx, c, old.Spec.StopGrace); err != nil {
				e.Log.Warn("stop old container", "container", c, "err", err)
			}
			if err := e.Docker.ContainerRemove(ctx, c); err != nil {
				e.Log.Warn("remove old container", "container", c, "err", err)
			}
		}
	}()
}

// WaitDrains blocks until background drains finish (shutdown, tests).
func (e *Engine) WaitDrains() { e.drains.Wait() }

// gc removes images of releases older than current and previous.
func (e *Engine) gc(ctx context.Context, project, service string) {
	svc, err := e.Store.Service(project, service)
	if err != nil {
		return
	}
	rels, err := e.Store.Releases(project, service)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, r := range rels {
		if r.ID == svc.Current || r.ID == svc.Previous {
			keep[r.Image] = true
		}
	}
	for _, r := range rels {
		if keep[r.Image] || r.ImageRemoved || r.Status == store.StatusRemoved || r.Status == store.StatusDeploying {
			continue
		}
		if strings.HasPrefix(r.Image, "clowdr.local/") {
			if err := e.Docker.ImageRemove(ctx, r.Image); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				e.Log.Warn("gc image", "image", r.Image, "err", err)
				continue
			}
		}
		r.ImageRemoved = true
		if r.Status == store.StatusSuperseded {
			r.Status = store.StatusRemoved
		}
		e.Store.PutRelease(r)
	}
	// Keep history bounded.
	const maxHistory = 50
	if len(rels) > maxHistory {
		for _, r := range rels[maxHistory:] {
			if r.ID != svc.Current && r.ID != svc.Previous {
				e.Store.DeleteRelease(r.Project, r.Service, r.ID)
			}
		}
	}
}

// Rollback redeploys the previous release (or the given one) with its image and spec.
func (e *Engine) Rollback(ctx context.Context, project, service string, to uint64, author string, progress Progress) (store.Release, error) {
	svc, err := e.Store.Service(project, service)
	if err != nil {
		return store.Release{}, fmt.Errorf("service %s/%s: %w", project, service, err)
	}
	if to == 0 {
		to = svc.Previous
	}
	if to == 0 {
		return store.Release{}, errors.New("no previous release to roll back to")
	}
	target, err := e.Store.Release(project, service, to)
	if err != nil {
		return store.Release{}, fmt.Errorf("release r%d: %w", to, err)
	}
	if target.Status == store.StatusRemoved || target.ImageRemoved {
		return store.Release{}, fmt.Errorf("release r%d image was garbage-collected", to)
	}
	if target.Status == store.StatusFailed {
		return store.Release{}, fmt.Errorf("release r%d failed, refusing to roll back to it", to)
	}
	progress("rolling back %s/%s to r%d (%s)", project, service, to, short(target.Version))
	return e.Deploy(ctx, Request{
		Project: project, Spec: target.Spec, Image: target.Image, Digest: target.Digest,
		Version: target.Version, Author: author, Rollback: true,
	}, progress)
}

// Restart redeploys the current release (same image, fresh containers, health-gated).
func (e *Engine) Restart(ctx context.Context, project, service, author string, progress Progress) (store.Release, error) {
	svc, err := e.Store.Service(project, service)
	if err != nil {
		return store.Release{}, fmt.Errorf("service %s/%s: %w", project, service, err)
	}
	cur, err := e.Store.Release(project, service, svc.Current)
	if err != nil {
		return store.Release{}, errors.New("service has no active release")
	}
	return e.Deploy(ctx, Request{
		Project: project, Spec: svc.Spec, Image: cur.Image, Digest: cur.Digest,
		Version: cur.Version, Author: author,
	}, progress)
}

// Stop stops the service's containers and takes it out of nginx.
func (e *Engine) Stop(ctx context.Context, project, service string) error {
	unlock := e.lock(project, service)
	defer unlock()
	svc, err := e.Store.Service(project, service)
	if err != nil {
		return err
	}
	if _, err := e.Store.UpdateService(project, service, func(v *store.Service) error { v.Stopped = true; return nil }); err != nil {
		return err
	}
	if err := e.RenderProject(ctx, project); err != nil {
		return err
	}
	if cur, err := e.Store.Release(project, service, svc.Current); err == nil {
		for _, c := range cur.Containers {
			e.Docker.ContainerStop(ctx, c, cur.Spec.StopGrace)
		}
	}
	return nil
}

// Start starts a stopped service's containers again and waits for health.
func (e *Engine) Start(ctx context.Context, project, service string, progress Progress) error {
	unlock := e.lock(project, service)
	defer unlock()
	svc, err := e.Store.Service(project, service)
	if err != nil {
		return err
	}
	cur, err := e.Store.Release(project, service, svc.Current)
	if err != nil {
		return errors.New("service has no active release")
	}
	// While stopped the service did not count against the quota; something else may use it now.
	if svc.Stopped {
		need := int64(len(cur.Containers)) * cur.Spec.Memory
		e.quotaM.Lock()
		used, err := e.projectMemory(project, service)
		if err == nil {
			if p, perr := e.Store.Project(project); perr == nil && p.Memory > 0 && used+need > p.Memory {
				err = fmt.Errorf("%w: %s used + %s to start > %s", ErrQuota,
					config.FormatBytes(used), config.FormatBytes(need), config.FormatBytes(p.Memory))
			}
		}
		if err != nil {
			e.quotaM.Unlock()
			return err
		}
		e.reserve(project, service, need)
		e.quotaM.Unlock()
		defer e.unreserve(project, service)
	}
	for _, c := range cur.Containers {
		if err := e.Docker.ContainerStart(ctx, c); err != nil {
			return err
		}
	}
	for _, c := range cur.Containers {
		if err := e.waitHealthy(ctx, project, cur.Spec, c, progress); err != nil {
			return err
		}
	}
	if _, err := e.Store.UpdateService(project, service, func(v *store.Service) error { v.Stopped = false; return nil }); err != nil {
		return err
	}
	return e.RenderProject(ctx, project)
}

// Remove deletes a service: routes, containers, images. Volumes stay.
func (e *Engine) Remove(ctx context.Context, project, service string) error {
	unlock := e.lock(project, service)
	defer unlock()
	rels, err := e.Store.Releases(project, service)
	if err != nil {
		return err
	}
	if err := e.Store.DeleteService(project, service); err != nil {
		return err
	}
	if err := e.RenderProject(ctx, project); err != nil {
		return err
	}
	cs, _ := e.Docker.Containers(ctx, runtime.LabelProject+"="+project, runtime.LabelService+"="+service)
	for _, c := range cs {
		e.Docker.ContainerRemove(ctx, c.ID)
	}
	e.pruneForeign(ctx, project)
	for _, r := range rels {
		if strings.HasPrefix(r.Image, "clowdr.local/") {
			e.Docker.ImageRemove(ctx, r.Image)
		}
	}
	return nil
}

// RenderProject regenerates conf.d/<project>.conf from state and applies it.
func (e *Engine) RenderProject(ctx context.Context, project string) error {
	e.renderM.Lock()
	defer e.renderM.Unlock()
	content, err := e.renderProject(project)
	if err != nil {
		return err
	}
	if err := e.Ingress.Apply(ctx, ingress.File{Path: ingress.ProjectFile(project), Content: content}); err != nil {
		return err
	}
	if e.RoutesChanged != nil {
		e.RoutesChanged()
	}
	return nil
}

func (e *Engine) renderProject(project string) (string, error) {
	svcs, err := e.Store.Services(project)
	if err != nil {
		return "", err
	}
	if len(svcs) == 0 {
		return "", nil
	}
	p, err := e.Store.Project(project)
	if err != nil {
		return "", err
	}
	var views []ingress.ServiceView
	for _, v := range svcs {
		// The spec was checked at deploy time; an admin may have taken a domain or nginx_extra
		// away from the project since. Render only what the project is allowed now.
		s := v.Spec
		s.Domains = slices.DeleteFunc(slices.Clone(s.Domains), func(d string) bool { return !auth.DomainAllowed(p.Domains, d) })
		if !p.NginxExtra {
			s.NginxExtra = ""
			s.Routes = slices.Clone(s.Routes)
			for i := range s.Routes {
				s.Routes[i].NginxExtra = ""
			}
		}
		view := ingress.ServiceView{Spec: s}
		if !v.Stopped && v.Current != 0 && v.Spec.Port != 0 {
			if r, err := e.Store.Release(project, v.Name, v.Current); err == nil {
				for _, c := range r.Containers {
					view.Targets = append(view.Targets, fmt.Sprintf("%s:%d", c, v.Spec.Port))
				}
			}
		}
		views = append(views, view)
	}
	return ingress.RenderProject(project, views, e.IngressOptions())
}

// IngressOptions are the host-wide rendering options.
func (e *Engine) IngressOptions() ingress.Options {
	p := e.Platform()
	return ingress.Options{
		Syslog:                e.Syslog,
		TrustedProxies:        p.TrustedProxies,
		WorkerShutdownTimeout: p.Nginx.WorkerShutdownTimeout.D(),
		ClientMaxBody:         int64(p.Nginx.ClientMaxBodySize),
		DeployHost:            p.DeployHost(),
		HasCert:               e.CertExists,
	}
}

// RenderAll rewrites every project's config and removes files of deleted projects.
func (e *Engine) RenderAll(ctx context.Context) error {
	e.renderM.Lock()
	defer e.renderM.Unlock()
	projects, err := e.Store.Projects()
	if err != nil {
		return err
	}
	files := []ingress.File{
		{Path: "nginx.conf", Content: ingress.RenderMain(e.IngressOptions())},
		{Path: "proxy_defaults.conf", Content: ingress.ProxyDefaultsConf},
	}
	known := map[string]bool{}
	for _, p := range projects {
		known[ingress.ProjectFile(p.Name)] = true
		content, err := e.renderProject(p.Name)
		if err != nil {
			// One broken project keeps its last config instead of taking every other one down.
			e.Log.Error("render project", "project", p.Name, "err", err)
			continue
		}
		files = append(files, ingress.File{Path: ingress.ProjectFile(p.Name), Content: content})
	}
	if es, err := os.ReadDir(filepath.Join(e.Ingress.Dir, "conf.d")); err == nil {
		for _, de := range es {
			f := "conf.d/" + de.Name()
			if strings.HasSuffix(f, ".conf") && !known[f] {
				files = append(files, ingress.File{Path: f})
			}
		}
	}
	return e.Ingress.Apply(ctx, files...)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CertDomains lists domains clowdr must hold certificates for: deploy.<domain> and every
// domain of a service with `tls: direct`.
func (e *Engine) CertDomains() []string {
	var out []string
	if d := e.Platform().DeployHost(); d != "" {
		out = append(out, d)
	}
	svcs, _ := e.Store.Services("")
	for _, v := range svcs {
		if v.Spec.TLS != spec.TLSDirect {
			continue
		}
		p, err := e.Store.Project(v.Project)
		if err != nil {
			continue
		}
		for _, d := range v.Spec.Domains {
			if auth.DomainAllowed(p.Domains, d) {
				out = append(out, d)
			}
		}
	}
	return out
}

func (e *Engine) logConfig(project, service string, release uint64, replica int) *runtime.LogConfig {
	if e.LogConfig != nil {
		return e.LogConfig(project, service, release, replica)
	}
	return &runtime.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}}
}
