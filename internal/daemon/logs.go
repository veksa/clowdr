package daemon

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"

	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/ingress"
	"github.com/veksa/clowdr/internal/logs"
	"github.com/veksa/clowdr/internal/runtime"
	"github.com/veksa/clowdr/internal/store"
)

// ForwardAddr is where docker's fluentd driver delivers container output. The docker daemon
// connects from the host, so loopback is enough.
const ForwardAddr = "127.0.0.1:24224"

// fluentdLogConfig sends a service container's output to clowdrd. Docker's local copy
// ("dual logging") is disabled, otherwise every line would hit the disk again.
func fluentdLogConfig(project, service string, release uint64, replica int) *runtime.LogConfig {
	return &runtime.LogConfig{Type: "fluentd", Config: map[string]string{
		"fluentd-address":              "tcp://" + ForwardAddr,
		"tag":                          logs.Tag(project, service, release, replica),
		"fluentd-async":                "true",
		"fluentd-sub-second-precision": "true",
		"fluentd-buffer-limit":         "20000",
		"mode":                         "non-blocking",
		"max-buffer-size":              "4m",
		"cache-disabled":               "true",
	}}
}

func startLogs(ctx context.Context, o Options, st *store.Store, plat func() *config.Platform, onLine func(logs.Key, logs.Line), log *slog.Logger) (*logs.Store, func(), error) {
	ls := &logs.Store{
		BufDir:  filepath.Join(ingress.RunDir, "buf"),
		DataDir: filepath.Join(o.StateDir, "logs"),
		Log:     log,
		RetentionFor: func(k logs.Key) logs.Retention {
			p := plat()
			r := p.Logs.App
			switch k.Kind {
			case logs.KindTrace:
				r = p.Logs.Trace
			case logs.KindMetric:
				r = p.Logs.Metric
			case logs.KindAnalytics:
				r = p.Logs.Analytics
			}
			if k.Kind == logs.KindAccess || k.Kind == logs.KindNginx {
				r = p.Logs.Access
			}
			return logs.Retention{Age: r.Retention.D(), Max: int64(r.Max)}
		},
	}
	if err := ls.Open(); err != nil {
		return nil, nil, err
	}
	col := &logs.Collector{Store: ls, OnLine: onLine, Projects: func() []string {
		ps, _ := st.Projects()
		names := make([]string, len(ps))
		for i, p := range ps {
			names[i] = p.Name
		}
		return names
	}}
	fwd := &logs.ForwardServer{Handle: col.Forward, Log: log}
	ln, err := net.Listen("tcp", ForwardAddr)
	if err != nil {
		return nil, nil, err
	}
	sl, err := logs.ListenSyslog(ingress.LogSocket)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		if err := fwd.Serve(ctx, ln); err != nil {
			log.Error("log forward server", "err", err)
		}
	}()
	go func() {
		if err := (&logs.SyslogServer{Handle: col.Syslog}).Serve(ctx, sl); err != nil {
			log.Error("syslog server", "err", err)
		}
	}()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { ls.Run(stop); close(done) }()
	return ls, func() {
		cancel()
		close(stop)
		<-done
		ls.Close()
	}, nil
}
