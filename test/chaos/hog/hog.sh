#!/bin/sh
# A service that runs out of memory on request: after `touch /tmp/oom` (clowctl exec) the
# main process keeps doubling a string until the kernel kills it at the memory limit.
rm -f /tmp/oom
httpd -p 8080 -h /www
while [ ! -f /tmp/oom ]; do sleep 1; done
rm -f /tmp/oom
exec awk 'BEGIN { s = "x"; while (1) s = s s }'
