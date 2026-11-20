package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/runtime"
)

const NginxContainer = "clowdr-nginx"

// NginxExec runs nginx commands inside the nginx container.
type NginxExec struct{ Docker Docker }

func (n NginxExec) Test(ctx context.Context) (string, error) {
	out, code, err := n.Docker.Exec(ctx, NginxContainer, []string{"nginx", "-t", "-q", "-c", ingress.MainConf})
	if err != nil {
		return out, fmt.Errorf("%w: %w", ingress.ErrNginxUnreachable, err)
	}
	if code != 0 {
		return out, fmt.Errorf("nginx -t exited with %d", code)
	}
	return out, nil
}

// Reload signals nginx and waits until the new config takes the traffic. `nginx -s reload`
// returns as soon as the signal is sent, while the old workers keep accepting connections
// until the master has started new ones and told the old ones to quit; a deploy is only
// "live" once that happened.
func (n NginxExec) Reload(ctx context.Context) error {
	old, _ := n.workers(ctx)
	out, code, err := n.Docker.Exec(ctx, NginxContainer, []string{"nginx", "-s", "reload", "-c", ingress.MainConf})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("nginx -s reload exited with %d: %s", code, strings.TrimSpace(out))
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(old) > 0 && time.Now().Before(deadline) {
		now, err := n.workers(ctx)
		if err != nil {
			return nil // cannot tell; the reload itself went through
		}
		accepting := false
		for pid := range old {
			if title, ok := now[pid]; ok && !strings.Contains(title, "shutting down") {
				accepting = true
				break
			}
		}
		if !accepting {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

// workers lists nginx worker processes (pid -> title). Old workers renamed to "worker process
// is shutting down" no longer accept connections. Needs ps in the image (nginx images have it).
func (n NginxExec) workers(ctx context.Context) (map[string]string, error) {
	out, code, err := n.Docker.Exec(ctx, NginxContainer, []string{"ps", "-o", "pid,args"})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("ps exited with %d", code)
	}
	ws := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		pid, title, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.Contains(title, "nginx: worker process") {
			ws[pid] = strings.TrimSpace(title)
		}
	}
	return ws, nil
}

// StartNginx brings nginx up with the current config when clowdrd starts.
//
// A container that is missing, stopped, restarting or about to be recreated for a new image
// reads its config when it (re)starts, so the main files are written first. A restarting one
// counts as not running: exec into it fails until it is up, and Docker's restart policy starts
// it on whatever is on disk, so writing first is the only way to give it the new config.
//
// A container that already runs keeps the config it loaded until it is reloaded, and what it
// loaded may differ from the disk even when no file changes now: an upgrade that rewrote
// nginx.conf, a reload that failed, a clowdrd killed between writing and reloading. So it
// gets exactly one checked reload per start, `nginx -t` then a graceful reload, whether or not
// a file changed. If `nginx -t` fails, the files this start changed are restored (the whole
// batch, nginx.conf included, even when one project file is the bad one), nginx keeps serving
// what it had, and the daemon still starts: stopping it would not fix nginx either.
func (e *Engine) StartNginx(ctx context.Context) error {
	running, err := e.nginxCurrent(ctx)
	if err != nil {
		return fmt.Errorf("inspect nginx: %w", err)
	}
	if !running {
		if err := e.Ingress.Write(e.mainConf()...); err != nil {
			return err
		}
	}
	if err := e.EnsureNginx(ctx); errors.Is(err, ErrNginxDegraded) {
		// nginx serves, so the daemon starts; a container that ran before takes the config
		// with a checked reload like any running one.
		e.Log.Error("nginx", "err", err)
		if !running {
			running = e.nginxUp(ctx)
		}
	} else if err != nil {
		return err
	}
	err = e.renderAll(ctx, running)
	if running && errors.Is(err, ingress.ErrInvalidConfig) {
		e.Log.Error("nginx -t failed: nginx keeps running on the config it had loaded, and every file this start changed (nginx.conf included) was restored", "err", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("nginx config: %w", err)
	}
	return nil
}

// nginxCurrent tells whether the nginx container runs the configured image and is up, i.e.
// whether EnsureNginx will leave it alone and it takes a reload. A missing container is not
// an error; any other inspect failure is, since guessing would write files nginx never loads.
func (e *Engine) nginxCurrent(ctx context.Context) (bool, error) {
	ci, err := e.Docker.ContainerInspect(ctx, NginxContainer)
	if errors.Is(err, runtime.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ci.State.Running && !ci.State.Restarting && ci.Config.Image == e.Platform().Nginx.Image, nil
}

// nginxUp tells whether the nginx container runs, whatever its image.
func (e *Engine) nginxUp(ctx context.Context) bool {
	ci, err := e.Docker.ContainerInspect(ctx, NginxContainer)
	return err == nil && ci.State.Running && !ci.State.Restarting
}

// ErrNginxDegraded marks an EnsureNginx error that left nginx serving, only not as configured
// (an older image, a project network it could not join). Callers report it and go on:
// stopping clowdrd would not bring nginx closer.
var ErrNginxDegraded = errors.New("nginx is serving, but not as configured")

// EnsureNginx makes sure the nginx container runs the configured image with clowdr's config,
// and is connected to every project network.
//
// A new image is pulled before the old container is removed: a tag that does not exist, or a
// registry that does not answer, leaves the old nginx serving (ErrNginxDegraded) rather than
// no ingress at all.
//
// nginx resolves the upstream container names when it starts, through Docker's DNS, which
// answers only for the networks the container is in at that moment. A name that does not
// resolve then leaves its upstream without servers until the next reload, so a container
// that is about to start (new, recreated or stopped) is connected to every project network
// first and started after. Docker connects a created or stopped container to a network as
// well as a running one; the endpoints are kept until the container is removed.
func (e *Engine) EnsureNginx(ctx context.Context) error {
	image := e.Platform().Nginx.Image
	ci, err := e.Docker.ContainerInspect(ctx, NginxContainer)
	exists := err == nil
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	var errs []error
	start := ""
	if exists && !ci.State.Running {
		start = NginxContainer
	}
	if !exists || ci.Config.Image != image {
		if err := e.pullNginx(ctx, image); err != nil {
			if !exists {
				return err
			}
			e.Log.Error("nginx keeps its old image: the new one cannot be pulled", "image", image, "running", ci.Config.Image, "err", err)
			errs = append(errs, fmt.Errorf("%w: it stays on %s: %w", ErrNginxDegraded, ci.Config.Image, err))
		} else {
			if exists {
				e.Log.Info("nginx image changed, recreating", "from", ci.Config.Image, "to", image)
				if err := e.Docker.ContainerRemove(ctx, NginxContainer); err != nil {
					return err
				}
			}
			id, err := e.createNginx(ctx, image)
			if err != nil {
				return err
			}
			start = id
		}
	}
	// A network that cannot be joined breaks the sites of its project only: nginx starts
	// anyway, rather than leaving every project without ingress.
	projects, err := e.Store.Projects()
	if err != nil {
		e.Log.Error("nginx: cannot list the projects to join their networks", "err", err)
		errs = append(errs, fmt.Errorf("%w: list projects: %w", ErrNginxDegraded, err))
	}
	for _, p := range projects {
		if err := e.connectNginx(ctx, p.Name); err != nil {
			e.Log.Error("nginx is not in a project network: the project's sites fail until it is", "project", p.Name, "err", err)
			errs = append(errs, fmt.Errorf("%w: %w", ErrNginxDegraded, err))
		}
	}
	if start != "" {
		// The errors above are logged; they do not make a nginx that is down degraded.
		if err := e.Docker.ContainerStart(ctx, start); err != nil {
			return fmt.Errorf("start nginx: %w", err)
		}
	}
	return errors.Join(errs...)
}

// pullNginx pulls the nginx image unless it is on the host already.
func (e *Engine) pullNginx(ctx context.Context, image string) error {
	if _, err := e.Docker.ImageInspect(ctx, image); err == nil {
		return nil
	}
	e.Log.Info("pulling nginx", "image", image)
	if err := e.Docker.ImagePull(ctx, image); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

func (e *Engine) createNginx(ctx context.Context, image string) (string, error) {
	id, err := e.Docker.ContainerCreate(ctx, runtime.ContainerConfig{
		Name:    NginxContainer,
		Image:   image,
		Cmd:     []string{"nginx", "-c", ingress.MainConf, "-g", "daemon off;"},
		Labels:  map[string]string{runtime.LabelManaged: "1", runtime.LabelRole: "nginx"},
		Restart: "always",
		Mounts: []runtime.Mount{
			{Type: "bind", Source: ingress.ConfDir, Target: ingress.ConfDir, ReadOnly: true},
			{Type: "bind", Source: ingress.CertDir, Target: ingress.CertDir, ReadOnly: true},
			{Type: "bind", Source: ingress.RunDir, Target: ingress.RunDir},
		},
		Ports: []runtime.PortBinding{{HostPort: 80, ContainerPort: 80}, {HostPort: 443, ContainerPort: 443}},
		Log:   &runtime.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "2"}},
	})
	if err != nil {
		return "", fmt.Errorf("create nginx: %w", err)
	}
	return id, nil
}

func (e *Engine) connectNginx(ctx context.Context, project string) error {
	labels := map[string]string{runtime.LabelManaged: "1", runtime.LabelProject: project}
	if err := e.Docker.NetworkEnsure(ctx, NetworkName(project), labels); err != nil {
		return err
	}
	if err := e.Docker.NetworkConnect(ctx, NetworkName(project), NginxContainer, nil); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("connect nginx to %s: %w", NetworkName(project), err)
	}
	return nil
}

func AddonContainer(project, addon string) string { return project + "-" + addon }

// EnsureAddon starts the project's own instance of an addon (e.g. redis), reachable in the
// project network by the addon name.
func (e *Engine) EnsureAddon(ctx context.Context, project, addon string, progress Progress) error {
	a, ok := e.Platform().Addons[addon]
	if !ok {
		return fmt.Errorf("addon %s is not configured on this host", addon)
	}
	name := AddonContainer(project, addon)
	if ci, err := e.Docker.ContainerInspect(ctx, name); err == nil {
		if !ci.State.Running {
			return e.Docker.ContainerStart(ctx, name)
		}
		return nil
	} else if !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if _, err := e.Docker.ImageInspect(ctx, a.Image); err != nil {
		progress("pulling %s", a.Image)
		if err := e.Docker.ImagePull(ctx, a.Image); err != nil {
			return err
		}
	}
	labels := map[string]string{
		runtime.LabelManaged: "1", runtime.LabelProject: project, runtime.LabelRole: "addon",
		runtime.LabelAddon: addon, labelMemory: fmt.Sprint(int64(a.Memory)),
	}
	var mounts []runtime.Mount
	if a.Volume != "" {
		vol := VolumeName(project, addon+"-data")
		if err := e.Docker.VolumeEnsure(ctx, vol, labels); err != nil {
			return err
		}
		mounts = append(mounts, runtime.Mount{Type: "volume", Source: vol, Target: a.Volume})
	}
	var cmd []string
	if addon == "redis" && a.Memory > 0 {
		cmd = []string{"redis-server", "--maxmemory", fmt.Sprint(int64(a.Memory) * 8 / 10), "--maxmemory-policy", "allkeys-lru"}
	}
	id, err := e.Docker.ContainerCreate(ctx, runtime.ContainerConfig{
		Name:     name,
		Image:    a.Image,
		Cmd:      cmd,
		Labels:   labels,
		Memory:   int64(a.Memory),
		Restart:  "unless-stopped",
		Network:  NetworkName(project),
		Aliases:  []string{addon},
		Mounts:   mounts,
		Log:      &runtime.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "5m", "max-file": "2"}},
		Pids:     e.Platform().Limits.Pids,
		Hardened: true,
	})
	if err != nil {
		return err
	}
	progress("started addon %s", name)
	return e.Docker.ContainerStart(ctx, id)
}
