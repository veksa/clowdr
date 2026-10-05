package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/store"
)

// Scale changes the number of replicas: a health-gated redeploy of the current release
// with the new count. 0 stops the service.
func (e *Engine) Scale(ctx context.Context, project, service string, n int, author string, progress Progress) (store.Release, error) {
	if n < 0 || n > 32 {
		return store.Release{}, errors.New("replicas must be between 0 and 32")
	}
	svc, err := e.Store.Service(project, service)
	if err != nil {
		return store.Release{}, fmt.Errorf("service %s/%s: %w", project, service, err)
	}
	if n == 0 {
		progress("stopping %s (replicas=0)", service)
		return store.Release{}, e.Stop(ctx, project, service)
	}
	cur, err := e.Store.Release(project, service, svc.Current)
	if err != nil {
		return store.Release{}, errors.New("service has no active release")
	}
	// Refuse up front when the new set cannot fit next to everything else, instead of
	// killing the running copies and failing.
	if free, err := e.freeMemory(ctx); err == nil && free >= 0 && svc.Spec.Memory > 0 {
		running := int64(0)
		if !svc.Stopped {
			running = int64(len(cur.Containers)) * cur.Spec.Memory
		}
		if need := int64(n) * svc.Spec.Memory; need > free+running {
			return store.Release{}, fmt.Errorf("%d × %s does not fit: %s free on the server", n, config.FormatBytes(svc.Spec.Memory), config.FormatBytes(max(free+running, 0)))
		}
	}
	s := svc.Spec
	s.Replicas = n
	progress("scaling %s to %d", service, n)
	return e.Deploy(ctx, Request{
		Project: project, Spec: s, Image: cur.Image, Digest: cur.Digest, Version: cur.Version, Author: author,
	}, progress)
}

// Exec runs a command in a replica (1-based) of the current release.
func (e *Engine) Exec(ctx context.Context, project, service string, replica int, cmd []string) (string, int, error) {
	if len(cmd) == 0 {
		return "", -1, errors.New("no command")
	}
	svc, err := e.Store.Service(project, service)
	if err != nil {
		return "", -1, fmt.Errorf("service %s/%s: %w", project, service, err)
	}
	cur, err := e.Store.Release(project, service, svc.Current)
	if err != nil || len(cur.Containers) == 0 {
		return "", -1, errors.New("service has no running release")
	}
	if replica <= 0 {
		replica = 1
	}
	if replica > len(cur.Containers) {
		return "", -1, fmt.Errorf("service has %d replicas", len(cur.Containers))
	}
	return e.Docker.Exec(ctx, cur.Containers[replica-1], cmd)
}

// AddonExec runs a command in the project's addon container (e.g. redis-cli).
func (e *Engine) AddonExec(ctx context.Context, project, addon string, cmd []string) (string, int, error) {
	if len(cmd) == 0 {
		return "", -1, errors.New("no command")
	}
	name := AddonContainer(project, addon)
	if _, err := e.Docker.ContainerInspect(ctx, name); err != nil {
		return "", -1, fmt.Errorf("addon %s is not running for %s", addon, project)
	}
	return e.Docker.Exec(ctx, name, cmd)
}

// Volume is a project volume as shown by `clowctl volume ls`.
type Volume struct {
	Name    string   `json:"name"` // as written in deploy.yml
	Docker  string   `json:"docker"`
	Created string   `json:"created,omitempty"`
	UsedBy  []string `json:"used_by,omitempty"`
}

func (e *Engine) Volumes(ctx context.Context, project string) ([]Volume, error) {
	vs, err := e.Docker.Volumes(ctx, runtime.LabelProject+"="+project)
	if err != nil {
		return nil, err
	}
	svcs, _ := e.Store.Services(project)
	var out []Volume
	for _, v := range vs {
		name := strings.TrimPrefix(v.Name, project+"_")
		vol := Volume{Name: name, Docker: v.Name, Created: v.CreatedAt}
		for _, s := range svcs {
			for _, sv := range s.Spec.Volumes {
				if sv.Name == name {
					vol.UsedBy = append(vol.UsedBy, s.Name)
				}
			}
			for _, a := range s.Spec.Uses {
				if name == a+"-data" {
					vol.UsedBy = append(vol.UsedBy, "addon "+a)
				}
			}
		}
		out = append(out, vol)
	}
	return out, nil
}

// RemoveVolume deletes a project volume that no deployed service declares.
func (e *Engine) RemoveVolume(ctx context.Context, project, name string) error {
	vols, err := e.Volumes(ctx, project)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name != name {
			continue
		}
		if len(v.UsedBy) > 0 {
			return fmt.Errorf("volume %s is used by %s; remove it from deploy.yml and redeploy first", name, strings.Join(v.UsedBy, ", "))
		}
		return e.Docker.VolumeRemove(ctx, v.Docker)
	}
	return fmt.Errorf("volume %s: %w", name, store.ErrNotFound)
}

// Addon is a running addon instance of a project.
type Addon struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Memory int64  `json:"memory,omitempty"`
}

func (e *Engine) Addons(ctx context.Context, project string) ([]Addon, error) {
	cs, err := e.Docker.Containers(ctx, runtime.LabelProject+"="+project, runtime.LabelRole+"=addon")
	if err != nil {
		return nil, err
	}
	var out []Addon
	for _, c := range cs {
		out = append(out, Addon{Name: c.Labels[runtime.LabelAddon], Image: c.Image, State: c.State, Memory: memoryLabel(c.Labels)})
	}
	return out, nil
}

// RemoveAddon removes the addon container; its data volume only with volumes=true.
func (e *Engine) RemoveAddon(ctx context.Context, project, addon string, volumes bool) error {
	svcs, _ := e.Store.Services(project)
	for _, s := range svcs {
		if contains(s.Spec.Uses, addon) {
			return fmt.Errorf("service %s uses %s; remove it from `uses` and redeploy first", s.Name, addon)
		}
	}
	if err := e.Docker.ContainerRemove(ctx, AddonContainer(project, addon)); err != nil {
		return err
	}
	if volumes {
		if err := e.Docker.VolumeRemove(ctx, VolumeName(project, addon+"-data")); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
	}
	return nil
}

// GCResult is what `clowctl gc` removed.
type GCResult struct {
	Containers []string `json:"containers"`
	Images     []string `json:"images"`
}

// GC removes service containers no release needs any more, old images of every service
// and pushed images that were never deployed.
func (e *Engine) GC(ctx context.Context) (GCResult, error) {
	var res GCResult
	svcs, err := e.Store.Services("")
	if err != nil {
		return res, err
	}
	keepC := map[string]bool{}
	keepI := map[string]bool{}
	// A deploy in flight records its containers only once they all started: keep every
	// container labelled with a deploying release.
	deploying := map[string]bool{}
	for _, v := range svcs {
		e.gc(ctx, v.Project, v.Name)
		rels, _ := e.Store.Releases(v.Project, v.Name)
		for _, r := range rels {
			if r.ID == v.Current || r.ID == v.Previous {
				keepI[r.Image] = true
			}
			if r.Status == store.StatusDeploying {
				deploying[fmt.Sprintf("%s/%s/%d", r.Project, r.Service, r.ID)] = true
			}
			if r.ID == v.Current || r.ID == v.Previous || r.Status == store.StatusDeploying {
				for _, c := range r.Containers {
					keepC[c] = true
				}
			}
		}
	}
	cs, err := e.Docker.Containers(ctx, runtime.LabelManaged, runtime.LabelRole+"=service")
	if err != nil {
		return res, err
	}
	for _, c := range cs {
		l := c.Labels
		if !keepC[c.Name()] && !deploying[l[runtime.LabelProject]+"/"+l[runtime.LabelService]+"/"+l[runtime.LabelRelease]] {
			if err := e.Docker.ContainerRemove(ctx, c.ID); err == nil {
				res.Containers = append(res.Containers, c.Name())
			}
		}
	}
	if lister, ok := e.Docker.(interface {
		Images(ctx context.Context, reference string) ([]runtime.ImageSummary, error)
	}); ok {
		imgs, err := lister.Images(ctx, "clowdr.local/*/*")
		if err == nil {
			for _, img := range imgs {
				for _, tag := range img.RepoTags {
					if strings.HasPrefix(tag, "clowdr.local/") && !keepI[tag] && e.unused(tag) {
						if err := e.Docker.ImageRemove(ctx, tag); err == nil {
							res.Images = append(res.Images, tag)
						}
					}
				}
			}
		}
	}
	return res, nil
}

// Pushed records that the registry received an image; GC leaves it alone for an hour, as a
// deploy usually follows the push.
func (e *Engine) Pushed(image string) {
	e.pushed.Store(image, time.Now())
}

func (e *Engine) unused(image string) bool {
	if t, ok := e.pushed.Load(image); ok && time.Since(t.(time.Time)) < time.Hour {
		return false
	}
	return true
}
