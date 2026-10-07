package daemon

import (
	"context"
	"log/slog"
	"os/exec"
	"time"
)

// Containers reach the cloud metadata service (169.254.169.254) through the host unless
// something stops them: on DigitalOcean, AWS, GCP and Hetzner it serves the droplet's
// user-data, which often carries secrets, and on some clouds instance credentials. No
// project has a reason to see it, so container traffic to it is rejected in DOCKER-USER,
// the chain docker leaves to the host admin. The host itself keeps access (OUTPUT is not
// touched).
var metadataRule = []string{"DOCKER-USER", "-d", "169.254.169.254/32", "-m", "comment", "--comment", "clowdr: no cloud metadata for containers", "-j", "REJECT"}

// iptables runs iptables; swapped in tests.
var iptables = func(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "iptables", args...).Run()
}

// ensureMetadataBlocked adds the rule unless it is there. Docker recreates DOCKER-USER empty
// after a host reboot or a firewall reload, so it runs at start and then every minute.
func ensureMetadataBlocked(ctx context.Context, log *slog.Logger) bool {
	if iptables(ctx, append([]string{"-C"}, metadataRule...)...) == nil {
		return true
	}
	if err := iptables(ctx, append([]string{"-I"}, metadataRule...)...); err != nil {
		log.Warn("firewall: could not block cloud metadata for containers", "err", err)
		return false
	}
	log.Info("firewall: containers cannot reach cloud metadata (169.254.169.254)")
	return true
}

func keepMetadataBlocked(ctx context.Context, log *slog.Logger) {
	if !ensureMetadataBlocked(ctx, log) {
		return // no iptables or no DOCKER-USER chain: warned once, nothing to keep
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ensureMetadataBlocked(ctx, log)
		}
	}
}
