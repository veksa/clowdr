package monitor

import (
	"regexp"
	"strings"
)

var containerID = regexp.MustCompile(`[0-9a-f]{64}`)

// oomKilled returns the container ids in a kernel log record of an OOM kill, from the cgroup
// of the killed task, e.g.
// "3,812,4361683816,-;oom-kill:constraint=CONSTRAINT_MEMCG,...,task_memcg=/system.slice/docker-<id>.scope,task=tail,pid=78938,uid=0"
// (/docker/<id> with the cgroupfs driver). A nested cgroup gives every id on its path.
func oomKilled(record string) []string {
	_, msg, ok := strings.Cut(record, ";")
	if !ok || !strings.HasPrefix(msg, "oom-kill:") {
		return nil
	}
	_, memcg, ok := strings.Cut(msg, ",task_memcg=")
	if !ok {
		return nil
	}
	if i := strings.IndexAny(memcg, ",\n"); i >= 0 {
		memcg = memcg[:i]
	}
	return containerID.FindAllString(memcg, -1)
}
