# clowdr

Deploy locally built images to a $6 VPS: one binary, a built-in registry, nginx as ingress,
health-gated switching, a token per project. No SSH and no Docker Hub.

- **clowdrd**: the daemon on the server (systemd). API, registry on top of containerd's image store,
  deploys, nginx config generation, Let's Encrypt.
- **clowctl**: the CLI on your machine (Windows, Linux, macOS). Builds locally, pushes only the new
  layers, and deploys.
- **nginx** handles traffic; clowdrd only rewrites its config and reloads it gracefully. If clowdrd
  stops, sites keep working.

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
the CLI from the same GitHub Releases.

```sh
clowctl context add prod https://deploy.example.com --token <admin token>
clowctl project create shop --domains example.com,*.example.com --platform redis --memory 600m
clowctl token create --project shop --env prod      # give this to the project / CI
clowctl platform status                             # nginx, certificates, memory and disk, projects
```

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
| `memory`, `replicas` | container memory limit; number of copies behind one upstream |
| `drain`, `stop_grace`, `switch` | how long old copies keep serving; SIGTERM grace; `stop-first` for services that must not run twice |
| `domains` | per env: `{prod: [api.example.com]}`; must be allowed for the project on the server |
| `routes` | `{path, service, timeout, buffering, max_body}`; `service` routes a path to another service |
| `tls: direct` | domain not behind Cloudflare: clowdr gets a Let's Encrypt certificate. Without it the domain answers only connections from `trusted_proxies` (Cloudflare by default), so the site cannot be reached past Cloudflare by the server's IP |
| `max_body`, `timeout`, `nginx_extra` | nginx limits; raw snippet (only if the admin allowed it) |
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
clowctl sdk install        # clowdr-node, served by the server itself, into vendor/clowdr
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
