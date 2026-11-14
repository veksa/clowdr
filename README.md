# clowdr

Deploy locally built images to a $6 VPS: one binary, a built-in registry, nginx as ingress,
health-gated switching, a token per project. No SSH and no Docker Hub.

- **clowdrd**: the daemon on the server (systemd). API, registry on top of containerd's image store,
  deploys, nginx config generation, Let's Encrypt.
- **clowctl**: the CLI on your machine (Windows, Linux, macOS). Builds locally, pushes only the new
  layers, and deploys.
- **nginx** handles traffic; clowdrd only rewrites its config and reloads it gracefully. If clowdrd
  stops, sites keep working.

## Compared to other self-hosted deploy tools

clowdr is for one small server (1 vCPU, 1 GB) that runs several projects, possibly of different
people. What it costs on the server and what a deploy needs:

| | Idle memory on the server | Deploy setup | Several projects on one host |
|---|---|---|---|
| **clowdr** | clowdrd ~22 MB + nginx ~12 MB (one worker per core, 1 core) | one `deploy.yml` next to the code, `clowctl deploy`; local build, only new layers are pushed; no SSH, no external registry | token, domains, memory quota and network per project |
| Kamal 2 | kamal-proxy ~15 MB | `config/deploy.yml` per app; SSH to the host, an external registry | one config per app; deploy access is SSH access to the host |
| Dokku | small (nginx + shell plugins) | `git push` builds on the server, or `git:from-image` | apps share the host, access by SSH keys |
| Dokploy | 300–400 MB | web UI, builds on the server | yes, through the UI |
| Coolify | 0.5–1.2 GB | web UI, builds on the server | yes, through the UI |
| Nomad | 100 MB+ per agent | job files (HCL), external registry and ingress | namespaces, ACLs |
| k3s | 512 MB minimum, ~1.6 GB with the usual components | Deployment + Service + Ingress manifests, external registry | namespaces, RBAC |

The clowdr numbers are RSS measured with no deploys running (Docker 29.9, nginx:alpine); a
worker of the shared nginx adds ~6 MB per CPU core. The others are their documented minimums or
published idle measurements and depend on the configuration. Kubernetes or Nomad pay off with
several machines; Coolify and Dokploy if you want a UI and build on the server; clowdr builds on
your machine, so the small server never compiles anything.

## Server

Requirements: Linux, Docker 29+ with the containerd image store (the default on new installs:
`curl -fsSL https://get.docker.com | sh`).

```sh
curl -fsSL https://github.com/veksa/clowdr/releases/latest/download/install.sh | sudo sh -s -- --domain example.com
```

- With `--domain`, the API and registry live at `https://deploy.example.com`. Point `deploy` at the
  server with a DNS-only (grey cloud) record; the certificate comes from Let's Encrypt.
- Without a domain, they listen on `https://<ip>:7443` with a self-signed certificate. `install.sh`
  prints its fingerprint, and `clowctl context add --fingerprint` pins it.

On first start clowdrd writes `/etc/clowdr/platform.yml`, starts nginx on :80/:443 and prints
the admin token.
To update the server, run `install.sh` again; it keeps the state. `clowctl self-update` updates
the CLI from the same GitHub Releases. Both accept a release only if its `checksums.txt` carries
an ed25519 signature by the project's release key, which is built into `install.sh` and clowctl
(`install.sh` verifies it with openssl 3). The signature covers the release tag too, so files of
an older release cannot be passed off as a newer one.

```sh
clowctl context add prod https://deploy.example.com --token <admin token>
clowctl project create shop --domains example.com,*.example.com --platform redis --memory 600m
clowctl token create --project shop --env prod      # give this to the project / CI
clowctl platform status                             # nginx, certificates, memory and disk, projects
clowctl project rename shop store                   # tokens, releases, images and logs move along
```

`project rename` starts every service's current release in new containers next to the old ones and
moves traffic when they are healthy, like a deploy. Volumes are copied to their new names, and the
old ones removed after the switch, so the disk needs room for a second copy; a service that mounts a
volume is down during its copy. It refuses a project with addons or a stopped service, and one
declared in `platform.yml`. Afterwards change `project:` in `deploy.yml`,
and `expose_to` / `accept_from` in the `deploy.yml` of other projects: the server rewrote its copy,
but their next deploy would bring the old name back. Anything outside clowdr (a database, a bucket,
secrets) keeps its name.

## Project

`deploy.yml` next to the code:

```yaml
project: shop
envs:
  prod: { server: https://deploy.example.com }

addons: [redis]          # own redis instance, reachable as `redis`

services:
  api:
    path: api
    build: yarn install && yarn build --type ${ENV}
    dockerfile: Dockerfile
    port: 3000
    health: /health
    memory: 300m
    uses: [redis]
    domains:
      prod: [api.example.com]
    routes:
      - { path: /ws, timeout: 24h, buffering: false }
    volumes:
      - uploads:/app/uploads
  web:
    path: web
    port: 8080
    memory: 32m
    domains: { prod: [example.com, www.example.com] }
```

```sh
export CLOWDR_TOKEN=<project token>    # or put it in .clowdr/token.prod (gitignored)
export CLOWDR_SERVER=https://deploy.example.com   # CI: send the token only to this server, whatever deploy.yml says
clowctl deploy api -e prod             # build → push new layers → start → health → switch → drain
clowctl diff -e prod                   # deploy.yml vs the server; clowctl apply applies it
clowctl ps -e prod
clowctl logs api -f -e prod
clowctl rollback api -e prod
```

Images are always built by a local docker engine (Docker Desktop on Windows and macOS, or
colima); the server only receives the new layers. Builds need buildx, which honors `--platform`:
Docker Desktop ships it, with Homebrew's docker it is `brew install docker-buildx`. `clowctl deploy` refuses a docker context or `DOCKER_HOST`
that points at another machine (`ssh://`, a remote `tcp://`). On Windows, `build:` commands run
with the `sh` of Git Bash.

Both sides keep two images per service, the live one and the one before it: the server removes
older ones after each deploy, and so does `clowctl deploy` on the machine that built them, which
also trims the docker build cache to `--keep-cache` (20GB). `--no-gc` skips the local part;
`clowctl gc --local` does it on demand.

### deploy.yml fields

| Field | Meaning |
|---|---|
| `path`, `build`, `dockerfile`, `build_args` | local build: `build` runs with `sh -c` in `path` (`${ENV}` is replaced; on Windows the sh of Git for Windows), then `docker build` with `path` as context |
| `image` | prebuilt image pulled by the server instead of building |
| `port`, `health` | container port; `health: /health` or `{path, interval, timeout, successes}` gates the switch |
| `memory`, `replicas` | container memory limit (required when the project has a quota); number of copies behind one upstream. Every container also gets `limits.pids` (1024) and `limits.cpus` from platform.yml, `no-new-privileges` and no `NET_RAW` |
| `drain`, `stop_grace`, `switch` | how long old copies keep serving; SIGTERM grace; `stop-first` for services that must not run twice |
| `domains` | per env: `{prod: [api.example.com]}`; must be allowed for the project on the server |
| `routes` | `{path, service, timeout, buffering, max_body}`; `service` routes a path to another service |
| `tls: direct` | domain not behind Cloudflare: clowdr gets a Let's Encrypt certificate. Without it the domain answers only connections from `trusted_proxies` (Cloudflare by default), so the site cannot be reached past Cloudflare by the server's IP |
| `max_body`, `timeout`, `nginx_extra` | nginx limits; raw snippet (only if the admin allowed it): headers, `proxy_*` tuning, `return`/`rewrite`/`set`, `location`/`if` blocks; `proxy_pass` only to `http://<project>__<service>`; file directives (`root`, `alias`, `include`, logs, certificates) are refused |
| `volumes` | named volumes `name:/path[:ro]`, never removed automatically |
| `env` | names whose values are set with `clowctl env set` |
| `uses`, `expose_to` | addons the service sees; projects whose network the service joins, so their services reach it as `<project>-<service>` |
| `accept_from` (top level) | projects allowed to join this project's network with `expose_to`; without it their deploy is refused, and dropping a project from the list disconnects its running containers |

All commands: `clowctl --help`.

## Logs and traces

Services only write to stdout. Plain lines are the log (`clowctl logs`); JSON lines starting with
`{"$":"trace"` are trace records, kept in a separate stream: one trace per request or job run,
with model calls, outgoing HTTP, SQL and log lines in order.

```sh
clowctl trace ls --since 2h --errors
clowctl trace show <traceId>
clowctl sdk install        # clowdr-node, served by the server itself, into vendor/clowdr (or from npm: @veksa/clowdr-node)
```

Record format, the Node package and how to wire a service: [docs/tracing.md](docs/tracing.md).

Metrics go the same way: one JSON point per series per minute on stdout, plus CPU and memory of
every container sampled by clowdr. `clowctl metrics ls`, `clowctl metrics show <name> --by <label>`;
format and the Node API: [docs/metrics.md](docs/metrics.md). CPU profiles of a Node service:
`clowctl profile <service> --seconds 30`, no endpoint needed.

## How a deploy works

1. `build` and `docker build --platform linux/amd64` run on your machine.
2. `docker push` sends only the layers the server does not have yet.
3. clowdrd starts the new containers next to the old ones and waits until `health` answers.
   If health fails, the new containers are removed and the old ones keep serving.
4. The nginx upstream switches (`nginx -t`, then a graceful reload). The old containers are
   drained and removed.
5. The current and previous images are kept for an instant rollback; older ones are removed.

When there is not enough memory for two copies (or with `switch: stop-first`), the old copy is
stopped first and started again if the new one fails.

## Development

```sh
go test ./...
go build -o bin/clowctl ./cmd/clowdr
GOOS=linux go build -o bin/linux/clowdrd ./cmd/clowdr
```

`examples/shop` is a minimal project. `test/e2e/run.sh` deploys it on a real Linux host with
Docker 29 (installs clowdrd system-wide, takes ports 80/443/7443): deploys under load with zero
failed requests, a failed health check, rollback, scale, restarts, logs. CI runs it on every
push; fuzz targets (`go test -fuzz`) run nightly.

License: Apache-2.0.
