#!/usr/bin/env bash
# Local stand: an Ubuntu host with systemd and Docker in a privileged container on your own
# Docker (Colima on macOS), where clowdrd is installed, upgraded and rolled back for real.
#
#   test/stand/stand.sh         from scratch: remove an old stand, build, start, run scenario.sh
#   test/stand/stand.sh up      from scratch, without the scenario
#   test/stand/stand.sh scenario   run the scenario again in the running stand
#   test/stand/stand.sh shell   a shell on the stand host
#   test/stand/stand.sh down    remove the stand's container, volumes and image
#
# It touches only its own resources, all named here: the container clowdr-stand-host, the
# volumes clowdr-stand-docker, clowdr-stand-containerd and clowdr-stand-gomod, and the image
# clowdr-stand-ubuntu. It never stops the Docker runtime or other containers. Ports, on
# 127.0.0.1 only: 11500 HTTPS, 11501 HTTP, 11502 the self-signed API port.
#
# STAND_NEW_VERSION=v0.4.0 tests that release from GitHub instead of the working tree;
# STAND_OLD_VERSION picks the release to start from (v0.3.1).
#
# One run at a time: a second one is refused while the first holds the lock. Ctrl-C removes the
# stand (KEEP=1 keeps it); a run that fails leaves it up to look at.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME=clowdr-stand-host
IMAGE=clowdr-stand-ubuntu
VOLUMES=(clowdr-stand-docker clowdr-stand-containerd clowdr-stand-gomod)
BASE=127-0-0-1.sslip.io

LOCK="${TMPDIR:-/tmp}/clowdr-stand.lock"

say() { printf '\033[1m%s\033[0m\n' "$*"; }

# lock: one stand command at a time on this machine. A lock whose process is gone is taken over.
lock() {
	if ! mkdir "$LOCK" 2>/dev/null; then
		local pid
		pid="$(cat "$LOCK/pid" 2>/dev/null || true)"
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			say "another stand run is in progress (pid $pid): wait for it, or stop it first" >&2
			exit 1
		fi
		rm -f "$LOCK/pid"
		rmdir "$LOCK" 2>/dev/null || true
		mkdir "$LOCK" || { say "cannot take $LOCK" >&2; exit 1; }
	fi
	echo $$ >"$LOCK/pid"
	trap unlock EXIT
	trap interrupted INT TERM
}
unlock() {
	rm -f "$LOCK/pid"
	rmdir "$LOCK" 2>/dev/null || true
}
interrupted() {
	trap - INT TERM
	if [ "${KEEP:-}" = 1 ]; then
		say "interrupted: the stand stays (KEEP=1)"
	else
		say "interrupted: removing the stand (KEEP=1 keeps it)"
		down
	fi
	exit 130
}

down() {
	if docker container inspect "$NAME" >/dev/null 2>&1; then
		say "removing container $NAME"
		docker rm -f "$NAME" >/dev/null
	fi
	local v
	for v in "${VOLUMES[@]}"; do
		if docker volume inspect "$v" >/dev/null 2>&1; then
			say "removing volume $v"
			docker volume rm "$v" >/dev/null
		fi
	done
	if docker image inspect "$IMAGE" >/dev/null 2>&1; then
		say "removing image $IMAGE"
		docker image rm "$IMAGE" >/dev/null
	fi
}

up() {
	down
	local go
	go="$(sed -n 's/^go //p' "$ROOT/go.mod")"
	say "building $IMAGE (go $go)"
	docker build -q -t "$IMAGE" --label clowdr-stand=1 --build-arg "GO_VERSION=$go" "$ROOT/test/stand" >/dev/null
	say "starting $NAME"
	docker run -d --name "$NAME" --hostname clowdr-stand --label clowdr-stand=1 \
		--privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
		-v clowdr-stand-docker:/var/lib/docker -v clowdr-stand-containerd:/var/lib/containerd \
		-v clowdr-stand-gomod:/root/go/pkg/mod -v "$ROOT:/src:ro" \
		-p 127.0.0.1:11500:443 -p 127.0.0.1:11501:80 -p 127.0.0.1:11502:7443 \
		--add-host "deploy.$BASE:127.0.0.1" --add-host "shop.$BASE:127.0.0.1" \
		"$IMAGE" >/dev/null
	local i
	for i in $(seq 60); do
		if docker exec "$NAME" docker info >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	docker logs --tail 30 "$NAME" >&2
	say "the stand did not boot" >&2
	exit 1
}

scenario() {
	docker exec -e STAND_OLD_VERSION="${STAND_OLD_VERSION:-}" -e STAND_NEW_VERSION="${STAND_NEW_VERSION:-}" "$NAME" bash /src/test/stand/scenario.sh
	say "the stand is up: https://deploy.$BASE:11500 and https://shop.$BASE:11500 (Pebble certificates)"
	say "test/stand/stand.sh shell for a shell on it, test/stand/stand.sh down to remove it"
}

case "${1:-run}" in
run) lock && up && scenario ;;
up) lock && up ;;
scenario) lock && scenario ;;
shell) docker exec -it "$NAME" bash ;;
down) lock && down ;;
*)
	echo "usage: $0 [run|up|scenario|shell|down]" >&2
	exit 2
	;;
esac
