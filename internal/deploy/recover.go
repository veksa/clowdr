package deploy

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/store"
)

// Recover cleans up after a clowdrd that was killed or a host that rebooted in the middle of
// an operation. Call it once at startup, after nginx runs and before the API serves: deploy
// locks and drains live in memory, so nothing is in flight yet.
//
//   - A release still marked deploying either got switched to (it is the service's current
//     release: only the bookkeeping after the switch is missing) or never did (failed, and
//     its containers are removed; they never got traffic).
//   - Containers of the current release are started when they are not running (a stop-first
//     deploy stopped them, or the host rebooted), and stopped for services that are stopped.
//   - Containers of the previous release are drained: the switch happened, the drain did not.
func (e *Engine) Recover(ctx context.Context) error {
	svcs, err := e.Store.Services("")
	if err != nil {
		return err
	}
	for _, v := range svcs {
		if err := e.recoverService(ctx, v); err != nil {
			e.Log.Error("recover", "service", v.Project+"/"+v.Name, "err", err)
		}
	}
	return nil
}

func (e *Engine) recoverService(ctx context.Context, v store.Service) error {
	rels, err := e.Store.Releases(v.Project, v.Name)
	if err != nil {
		return err
	}
	byID := map[uint64]store.Release{}
	for _, r := range rels {
		switch {
		case r.ID == v.Current && r.Status == store.StatusDeploying:
			r.Status, r.Finished = store.StatusActive, time.Now().UTC()
			e.Store.PutRelease(r)
			e.Log.Info("recover: release was switched to before clowdrd stopped", "service", v.Project+"/"+v.Name, "release", r.ID)
		case r.Status == store.StatusDeploying:
			r.Status, r.Finished = store.StatusFailed, time.Now().UTC()
			r.Error = "interrupted: clowdrd stopped during the deploy"
			e.Store.PutRelease(r)
			e.event(ctx, v.Project, v.Name, "deploy", true, fmt.Sprintf("r%d %s failed: %s", r.ID, short(r.Version), r.Error))
		case r.ID != v.Current && r.Status == store.StatusActive:
			r.Status = store.StatusSuperseded
			e.Store.PutRelease(r)
		}
		byID[r.ID] = r
	}

	cs, err := e.Docker.Containers(ctx, runtime.LabelProject+"="+v.Project, runtime.LabelService+"="+v.Name, runtime.LabelRole+"=service")
	if err != nil {
		return err
	}
	current := map[string]bool{}
	for _, c := range byID[v.Current].Containers {
		current[c] = true
	}
	drain := map[uint64]bool{}
	for _, c := range cs {
		name := c.Name()
		if current[name] {
			continue
		}
		id, _ := strconv.ParseUint(c.Labels[runtime.LabelRelease], 10, 64)
		if r, ok := byID[id]; ok && id == v.Previous && c.State == "running" && r.Status != store.StatusFailed {
			drain[id] = true
			continue
		}
		e.Log.Info("recover: removing container of an unfinished release", "container", name)
		e.Docker.ContainerRemove(ctx, c.ID)
	}
	for id := range drain {
		e.Log.Info("recover: draining the previous release", "service", v.Project+"/"+v.Name, "release", id)
		e.drain(byID[id], false, func(string, ...any) {})
	}

	if v.Current == 0 {
		return nil
	}
	cur := byID[v.Current]
	for _, name := range cur.Containers {
		ci, err := e.Docker.ContainerInspect(ctx, name)
		if err != nil {
			e.event(ctx, v.Project, v.Name, "recover", true, fmt.Sprintf("%s of the current release r%d is gone: %v (clowctl restart %s)", name, cur.ID, err, v.Name))
			continue
		}
		switch {
		case v.Stopped && ci.State.Running:
			e.Docker.ContainerStop(ctx, name, cur.Spec.StopGrace)
		case !v.Stopped && !ci.State.Running && !ci.State.Restarting:
			e.Log.Info("recover: starting a container of the current release", "container", name)
			if err := e.Docker.ContainerStart(ctx, name); err != nil {
				e.event(ctx, v.Project, v.Name, "recover", true, fmt.Sprintf("start %s: %v", name, err))
			}
		}
	}
	return nil
}
