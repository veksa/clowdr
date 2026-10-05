package logs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Platform is the pseudo-project for nginx's own error log.
const Platform = "_platform"

// Tag is the fluentd tag clowdrd sets on a service container.
func Tag(project, service string, release uint64, replica int) string {
	return fmt.Sprintf("%s.%s.%d.%d", project, service, release, replica)
}

// SyslogTag is the nginx syslog tag of a project's access log. Syslog tags allow only
// letters, digits and '_', so '-' is replaced; Collector.Syslog maps it back. ingress renders
// it into nginx's access_log.
func SyslogTag(project string) string { return "access_" + strings.ReplaceAll(project, "-", "_") }

// Collector routes received lines into the store.
type Collector struct {
	Store *Store
	// Projects lists known projects, to map access tags back to project names.
	Projects func() []string
	// OnLine sees every stored line (alerts: error rates, 5xx).
	OnLine func(Key, Line)
}

func (c *Collector) emit(k Key, l Line) {
	c.Store.Append(k, l)
	if c.OnLine != nil {
		c.OnLine(k, l)
	}
}

// Forward handles a line from docker's fluentd driver.
func (c *Collector) Forward(tag string, r Record) {
	parts := strings.Split(tag, ".")
	if len(parts) != 4 {
		c.emit(Key{Platform, KindApp, "unknown"}, Line{T: r.Time.UnixNano(), E: r.Stderr, L: tag + ": " + r.Line})
		return
	}
	rel, _ := strconv.ParseUint(parts[2], 10, 64)
	rep, _ := strconv.Atoi(parts[3])
	c.emit(Key{parts[0], KindApp, parts[1]}, Line{T: r.Time.UnixNano(), R: rel, I: rep, E: r.Stderr, L: r.Line})
}

// Syslog handles a line from nginx.
func (c *Collector) Syslog(tag string, r Record) {
	if p, ok := strings.CutPrefix(tag, "access_"); ok {
		project := p
		if c.Projects != nil {
			for _, name := range c.Projects() {
				if SyslogTag(name) == tag {
					project = name
					break
				}
			}
		}
		c.emit(Key{project, KindAccess, "_"}, Line{T: r.Time.UnixNano(), L: r.Line})
		return
	}
	c.emit(Key{Platform, KindNginx, "_"}, Line{T: r.Time.UnixNano(), E: r.Stderr, L: r.Line})
}

// Tail returns the last n lines of one container (release/replica) for deploy errors.
func (s *Store) Tail(project, service string, release uint64, replica, n int) []string {
	var out []string
	s.Read(Key{project, KindApp, service}, Query{
		Since: time.Now().Add(-time.Hour),
		Tail:  n,
		Match: func(l Line) bool { return l.R == release && (replica == 0 || l.I == replica) },
	}, func(l Line) { out = append(out, l.L) })
	return out
}
