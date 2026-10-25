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

// metadataRule6 covers the IPv6 metadata address (AWS fd00:ec2::254) where docker has IPv6 on;
// without ip6tables or an IPv6 DOCKER-USER chain there is nothing to block.
var metadataRule6 = []string{"DOCKER-USER", "-d", "fd00:ec2::254/128", "-m", "comment", "--comment", "clowdr: no cloud metadata for containers", "-j", "REJECT"}

// iptables runs iptables; swapped in tests.
var iptables = func(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "iptables", args...).Run()
}

var ip6tables = func(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "ip6tables", args...).Run()
}

// ensureMetadataBlocked6 is best effort and silent: most hosts run docker without IPv6.
func ensureMetadataBlocked6(ctx context.Context) {
	if ip6tables(ctx, append([]string{"-C"}, metadataRule6...)...) != nil {
		ip6tables(ctx, append([]string{"-I"}, metadataRule6...)...)
	}
}

// ensureMetadataBlocked adds the rule unless it is there. Docker recreates DOCKER-USER empty
// after a host reboot, a firewall reload or a docker restart, so it runs at start and then every
// few seconds: containers started right after a docker restart must not get a window.
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
	ensureMetadataBlocked6(ctx)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ensureMetadataBlocked(ctx, log)
			ensureMetadataBlocked6(ctx)
		}
	}
}
