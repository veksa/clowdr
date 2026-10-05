package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/runtime"
)

const NginxContainer = "clowdr-nginx"

// NginxExec runs nginx commands inside the nginx container.
type NginxExec struct{ Docker Docker }

func (n NginxExec) Test(ctx context.Context) (string, error) {
	out, code, err := n.Docker.Exec(ctx, NginxContainer, []string{"nginx", "-t", "-q", "-c", ingress.MainConf})
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("nginx -t exited with %d", code)
	}
	return out, nil
}

func (n NginxExec) Reload(ctx context.Context) error {
	out, code, err := n.Docker.Exec(ctx, NginxContainer, []string{"nginx", "-s", "reload", "-c", ingress.MainConf})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("nginx -s reload exited with %d: %s", code, strings.TrimSpace(out))
	}
	return nil
}

// EnsureNginx makes sure the nginx container runs the configured image with clowdr's config,
// and is connected to every project network.
func (e *Engine) EnsureNginx(ctx context.Context) error {
	image := e.Platform().Nginx.Image
	ci, err := e.Docker.ContainerInspect(ctx, NginxContainer)
	switch {
	case err == nil && ci.Config.Image != image:
		e.Log.Info("nginx image changed, recreating", "from", ci.Config.Image, "to", image)
		if err := e.Docker.ContainerRemove(ctx, NginxContainer); err != nil {
			return err
		}
		err = runtime.ErrNotFound
	case err == nil && !ci.State.Running:
		if err := e.Docker.ContainerStart(ctx, NginxContainer); err != nil {
			return fmt.Errorf("start nginx: %w", err)
		}
	}
	if errors.Is(err, runtime.ErrNotFound) {
		if _, ierr := e.Docker.ImageInspect(ctx, image); ierr != nil {
			e.Log.Info("pulling nginx", "image", image)
			if err := e.Docker.ImagePull(ctx, image); err != nil {
				return err
			}
		}
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
			return fmt.Errorf("create nginx: %w", err)
		}
		if err := e.Docker.ContainerStart(ctx, id); err != nil {
			return fmt.Errorf("start nginx: %w", err)
		}
	} else if err != nil {
		return err
	}
	projects, err := e.Store.Projects()
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := e.connectNginx(ctx, p.Name); err != nil {
			return err
		}
	}
	return nil
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
		Name:    name,
		Image:   a.Image,
		Cmd:     cmd,
		Labels:  labels,
		Memory:  int64(a.Memory),
		Restart: "unless-stopped",
		Network: NetworkName(project),
		Aliases: []string{addon},
		Mounts:  mounts,
		Log:     &runtime.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "5m", "max-file": "2"}},
	})
	if err != nil {
		return err
	}
	progress("started addon %s", name)
	return e.Docker.ContainerStart(ctx, id)
}
