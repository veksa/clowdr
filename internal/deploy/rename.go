package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/registry"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/spec"
	"github.com/veksa/clowdr/internal/store"
)

// RenameProject gives a project a new name without losing its history, tokens or images.
//
// Docker cannot rename what carries the old name for good (labels, the log tag, CLOWDR_PROJECT,
// the network), so every service gets new containers of its current release next to the old
// ones. Only when all of them are healthy does the state move, in one transaction, and nginx
// switch, in one reload. A failure before that leaves the old project serving as it was.
//
// A project with volumes or addons is refused: docker cannot rename a volume, and copying data
// is not something to do behind a rename.
func (e *Engine) RenameProject(ctx context.Context, old, name, author string, progress Progress) error {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	start := time.Now()
	e.renameM.Lock()
	defer e.renameM.Unlock()

	// Hold off every operation on this project and on the projects exposed to it: those have to
	// see the new name in their specs, and a deploy of one now would connect to the old network.
	all, err := e.Store.Services("")
	if err != nil {
		return err
	}
	gates := []string{old}
	for _, v := range all {
		if contains(v.Spec.ExposeTo, old) && !contains(gates, v.Project) {
			gates = append(gates, v.Project)
		}
	}
	for _, p := range gates {
		g := e.gate(p)
		g.Lock()
		defer g.Unlock()
	}

	svcs, current, err := e.checkRename(ctx, old, name)
	if err != nil {
		return err
	}
	e.renames.Store(name, old)
	defer e.renames.Delete(name)

	var (
		connected  [][2]string // network, container
		tagged     []string
		started    []string
		stopped    []store.Release
		stopFirsts = map[string]bool{}
	)
	cleanup := func() {
		ctx := context.WithoutCancel(ctx)
		for _, c := range started {
			e.Docker.ContainerRemove(ctx, c)
		}
		for _, r := range stopped {
			progress("starting %s again", strings.Join(r.Containers, ", "))
			for _, c := range r.Containers {
				e.Docker.ContainerStart(ctx, c)
			}
		}
		for _, c := range connected {
			e.Docker.NetworkDisconnect(ctx, c[0], c[1])
		}
		e.Docker.NetworkDisconnect(ctx, NetworkName(name), NginxContainer)
		if err := e.Docker.NetworkRemove(ctx, NetworkName(name)); err != nil {
			e.Log.Warn("remove network", "network", NetworkName(name), "err", err)
		}
		for _, ref := range tagged {
			e.Docker.ImageRemove(ctx, ref)
		}
	}

	if err := e.connectNginx(ctx, name); err != nil {
		cleanup()
		return err
	}
	// Containers exposed to this project (a shared redis) join the new network without a restart:
	// the new containers need them from their first second.
	for _, v := range all {
		if !contains(v.Spec.ExposeTo, old) || v.Current == 0 {
			continue
		}
		r, err := e.Store.Release(v.Project, v.Name, v.Current)
		if err != nil {
			cleanup()
			return err
		}
		for _, c := range r.Containers {
			if err := e.Docker.NetworkConnect(ctx, NetworkName(name), c, []string{v.Project + "-" + v.Name}); err != nil {
				cleanup()
				return fmt.Errorf("connect %s to %s: %w", c, NetworkName(name), err)
			}
			connected = append(connected, [2]string{NetworkName(name), c})
			progress("%s joined %s", c, NetworkName(name))
		}
	}

	refs, removed, saved, err := e.retagImages(ctx, old, name, svcs, &tagged)
	if err != nil {
		cleanup()
		return err
	}

	// Start-first where the quota and the host have room for a second copy, as in a deploy.
	var used, quota int64
	if p, err := e.Store.Project(old); err == nil {
		quota = p.Memory
	}
	for _, v := range svcs {
		used += int64(v.Spec.Replicas) * v.Spec.Memory
	}
	free, err := e.freeMemory(ctx)
	if err != nil {
		cleanup()
		return err
	}
	containers := map[uint64][]string{}
	for _, v := range svcs {
		r, ok := current[v.Name]
		if !ok {
			continue
		}
		s := r.Spec
		need := int64(len(r.Containers)) * s.Memory
		if s.Switch == spec.SwitchStopFirst || (quota > 0 && used+need > quota) || (free >= 0 && need > free) {
			stopFirsts[v.Name] = true
			progress("stop-first: stopping %s before starting it under the new name", v.Name)
			stopped = append(stopped, r)
			for _, c := range r.Containers {
				e.Docker.ContainerStop(ctx, c, s.StopGrace)
			}
		} else {
			used += need
			if free >= 0 {
				free -= need
			}
		}
		env := e.containerEnv(name, s, v.Env)
		for i := range r.Containers {
			c := containerName(name, v.Name, r.ID, i+1)
			if _, err := e.startContainer(ctx, name, s, r.ID, i+1, c, refs[r.Image], env); err != nil {
				cleanup()
				return fmt.Errorf("start %s: %w", c, err)
			}
			started = append(started, c)
			containers[r.ID] = append(containers[r.ID], c)
			progress("started %s", c)
		}
		for i, c := range containers[r.ID] {
			if err := e.waitHealthy(ctx, name, s, c, progress); err != nil {
				if tail := e.tail(ctx, name, v.Name, r.ID, i+1, c, 20); tail != "" {
					progress("last log lines of %s:\n%s", c, tail)
				}
				cleanup()
				return err
			}
		}
	}

	// The switch: state first (the renderer reads it), then nginx; on nginx failure back to both.
	err = e.Store.RenameProject(old, name, func(r *store.Release) {
		if ref, ok := refs[r.Image]; ok {
			r.ImageRemoved = r.ImageRemoved || removed[r.Image]
			r.Image = ref
		}
		r.Containers = containers[r.ID]
	})
	if err != nil {
		cleanup()
		return err
	}
	if err := e.switchProjectFile(ctx, old, name); err != nil {
		ctx := context.WithoutCancel(ctx)
		if rerr := e.Store.RenameProject(name, old, func(r *store.Release) {
			if s, ok := saved[r.ID]; ok {
				r.Image, r.Containers, r.ImageRemoved = s.Image, s.Containers, s.ImageRemoved
			}
		}); rerr != nil {
			return errors.Join(err, rerr)
		}
		if rerr := e.Store.FinishRename(old); rerr != nil {
			e.Log.Warn("rename: reverted state", "err", rerr)
		}
		if rerr := e.switchProjectFile(ctx, name, old); rerr != nil {
			e.Log.Error("rename: restore nginx config", "project", old, "err", rerr)
		}
		cleanup()
		return err
	}
	progress("traffic switched to %s", name)

	// History is readable under the new name at once; what the old containers still say while
	// they drain is moved after them.
	if e.MoveLogs != nil {
		e.MoveLogs(old, name)
	}
	var wait time.Duration
	for _, v := range svcs {
		if r, ok := current[v.Name]; ok && !stopFirsts[v.Name] {
			wait = max(wait, r.Spec.Drain)
		}
	}
	progress("draining the old containers for %s, then removing them", wait)
	e.drains.Add(1)
	go func() {
		defer e.drains.Done()
		time.Sleep(wait)
		e.finishRename(context.Background(), old, name)
	}()
	e.event(ctx, name, "", "rename", false, fmt.Sprintf("renamed from %s by %s in %s", old, author, time.Since(start).Round(time.Second)))
	return nil
}

// checkRename refuses a rename that would lose data or break something, before anything changed.
// It returns the project's services and their current releases by service name.
func (e *Engine) checkRename(ctx context.Context, old, name string) ([]store.Service, map[string]store.Release, error) {
	if !spec.ValidName(name) {
		return nil, nil, fmt.Errorf("invalid project name %q", name)
	}
	if name == "clowdr" {
		return nil, nil, errors.New("project name clowdr is reserved")
	}
	if name == old {
		return nil, nil, fmt.Errorf("the project is already called %s", name)
	}
	p, err := e.Store.Project(old)
	if err != nil {
		return nil, nil, fmt.Errorf("project %s: %w", old, err)
	}
	if p.RenamedFrom != "" {
		return nil, nil, fmt.Errorf("the rename from %s is still finishing: the old containers are draining", p.RenamedFrom)
	}
	// platform.yml would create the old project again on the next start of clowdrd.
	if p.Seeded {
		return nil, nil, fmt.Errorf("project %s comes from platform.yml: rename it there", old)
	}
	if _, err := e.Store.Project(name); err == nil {
		return nil, nil, fmt.Errorf("project %s already exists", name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}
	others, err := e.Store.Projects()
	if err != nil {
		return nil, nil, err
	}
	for _, o := range others {
		if o.Name != old && spec.NamesClash(o.Name, name) {
			return nil, nil, fmt.Errorf("project name %s clashes with project %s: one must not start with the other followed by '-'", name, o.Name)
		}
		if o.RenamedFrom == name {
			return nil, nil, fmt.Errorf("project %s was renamed to %s a moment ago and its containers are still draining", name, o.Name)
		}
	}
	for _, ext := range e.Platform().External {
		if _, ok := ext.Share[old]; ok {
			return nil, nil, fmt.Errorf("platform.yml shares an external service with project %s: change it there first", old)
		}
	}

	svcs, err := e.Store.Services(old)
	if err != nil {
		return nil, nil, err
	}
	current := map[string]store.Release{}
	for _, v := range svcs {
		if v.Stopped {
			return nil, nil, fmt.Errorf("service %s is stopped: start or remove it first", v.Name)
		}
		if len(v.Spec.Volumes) > 0 {
			return nil, nil, fmt.Errorf("service %s has volumes: docker cannot rename a volume, its data would be left behind", v.Name)
		}
		if len(v.Spec.Uses) > 0 {
			return nil, nil, fmt.Errorf("service %s uses addons (%s): their data and containers carry the project name", v.Name, strings.Join(v.Spec.Uses, ", "))
		}
		rels, err := e.Store.Releases(old, v.Name)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rels {
			if r.Status == store.StatusDeploying {
				return nil, nil, fmt.Errorf("release r%d of %s is still deploying", r.ID, v.Name)
			}
			if r.ID == v.Current {
				current[v.Name] = r
			}
		}
	}

	vols, err := e.Docker.Volumes(ctx, runtime.LabelProject+"="+old)
	if err != nil {
		return nil, nil, err
	}
	if len(vols) > 0 {
		return nil, nil, fmt.Errorf("project %s has volumes (%s): docker cannot rename a volume, remove them first if they are not needed", old, volumeNames(vols))
	}
	addons, err := e.Docker.Containers(ctx, runtime.LabelProject+"="+old, runtime.LabelRole+"=addon")
	if err != nil {
		return nil, nil, err
	}
	if len(addons) > 0 {
		return nil, nil, fmt.Errorf("project %s runs addons: remove them first if they are not needed", old)
	}

	// Whatever carries the new name already belongs to a removed project or an interrupted rename.
	left, err := e.LeftoverVolumes(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if len(left) > 0 {
		return nil, nil, fmt.Errorf("volumes of a removed project %s are still on the host (%s)", name, strings.Join(left, ", "))
	}
	cs, err := e.Docker.Containers(ctx, runtime.LabelProject+"="+name)
	if err != nil {
		return nil, nil, err
	}
	if len(cs) > 0 {
		return nil, nil, fmt.Errorf("containers labelled for project %s are on the host (%s): clowctl gc removes what an interrupted rename left", name, cs[0].Name())
	}
	return svcs, current, nil
}

func volumeNames(vols []runtime.Volume) string {
	names := make([]string, 0, len(vols))
	for _, v := range vols {
		names = append(names, v.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// retagImages gives the project's pushed images their name under the new project: CheckImage
// and the registry look for them there. It returns the new reference of every image of the
// project, the ones no longer on the host, and the releases as they were, for a revert.
func (e *Engine) retagImages(ctx context.Context, old, name string, svcs []store.Service, tagged *[]string) (refs map[string]string, removed map[string]bool, saved map[uint64]store.Release, err error) {
	refs, removed, saved = map[string]string{}, map[string]bool{}, map[uint64]store.Release{}
	prefix := registry.ImagePrefix + old + "/"
	for _, v := range svcs {
		rels, err := e.Store.Releases(old, v.Name)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, r := range rels {
			saved[r.ID] = r
			if _, seen := refs[r.Image]; seen {
				continue
			}
			rest, ok := strings.CutPrefix(r.Image, prefix)
			if !ok {
				refs[r.Image] = r.Image // pulled from a registry: the name does not change
				continue
			}
			ref := registry.ImagePrefix + name + "/" + rest
			refs[r.Image] = ref
			if r.ImageRemoved {
				continue
			}
			if err := e.Docker.ImageTag(ctx, r.Image, ref); err != nil {
				if errors.Is(err, runtime.ErrNotFound) {
					removed[r.Image] = true
					continue
				}
				return nil, nil, nil, fmt.Errorf("tag %s: %w", r.Image, err)
			}
			*tagged = append(*tagged, ref)
			e.Pushed(ref) // GC keeps it until the state knows about it
		}
	}
	return refs, removed, saved, nil
}

// switchProjectFile replaces the project's nginx config in one reload: both files claim the same
// domains, so they must not be live together.
func (e *Engine) switchProjectFile(ctx context.Context, from, to string) error {
	e.renderM.Lock()
	defer e.renderM.Unlock()
	content, err := e.renderProject(to)
	if err != nil {
		return err
	}
	err = e.Ingress.Apply(ctx, ingress.File{Path: ingress.ProjectFile(to), Content: content}, ingress.File{Path: ingress.ProjectFile(from)})
	if err != nil {
		return err
	}
	if e.RoutesChanged != nil {
		e.RoutesChanged()
	}
	return nil
}

// finishRename removes what still carries the old name once traffic moved. It works from labels
// and state, not from memory: Recover runs it again for a clowdrd that stopped halfway.
func (e *Engine) finishRename(ctx context.Context, old, name string) {
	svcs, err := e.Store.Services("")
	if err != nil {
		e.Log.Error("rename: finish", "project", name, "err", err)
		return
	}
	grace := map[string]time.Duration{}
	for _, v := range svcs {
		if v.Project == name {
			grace[v.Name] = v.Spec.StopGrace
		}
	}
	cs, err := e.Docker.Containers(ctx, runtime.LabelProject+"="+old, runtime.LabelRole+"=service")
	if err != nil {
		e.Log.Error("rename: finish", "project", name, "err", err)
		return
	}
	for _, c := range cs {
		if err := e.Docker.ContainerStop(ctx, c.ID, grace[c.Labels[runtime.LabelService]]); err != nil {
			e.Log.Warn("rename: stop old container", "container", c.Name(), "err", err)
		}
		if err := e.Docker.ContainerRemove(ctx, c.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			e.Log.Warn("rename: remove old container", "container", c.Name(), "err", err)
		}
	}
	for _, v := range svcs {
		if !contains(v.Spec.ExposeTo, name) || v.Current == 0 {
			continue
		}
		if r, err := e.Store.Release(v.Project, v.Name, v.Current); err == nil {
			for _, c := range r.Containers {
				e.Docker.NetworkDisconnect(ctx, NetworkName(old), c)
			}
		}
	}
	e.Docker.NetworkDisconnect(ctx, NetworkName(old), NginxContainer)
	if err := e.Docker.NetworkRemove(ctx, NetworkName(old)); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		e.Log.Warn("rename: remove old network", "network", NetworkName(old), "err", err)
	}
	if lister, ok := e.Docker.(interface {
		Images(ctx context.Context, reference string) ([]runtime.ImageSummary, error)
	}); ok {
		imgs, err := lister.Images(ctx, registry.ImagePrefix+old+"/*")
		if err != nil {
			e.Log.Warn("rename: list old images", "err", err)
		}
		for _, img := range imgs {
			for _, tag := range img.RepoTags {
				if strings.HasPrefix(tag, registry.ImagePrefix+old+"/") {
					if err := e.Docker.ImageRemove(ctx, tag); err != nil {
						e.Log.Warn("rename: remove old tag", "image", tag, "err", err)
					}
				}
			}
		}
	}
	if e.MoveLogs != nil {
		e.MoveLogs(old, name)
	}
	if err := e.Store.FinishRename(name); err != nil {
		e.Log.Error("rename: finish", "project", name, "err", err)
	}
}
