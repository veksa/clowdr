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

```sh
clowctl context add prod https://deploy.example.com --token <admin token>
clowctl project create shop --domains example.com,*.example.com --platform redis --memory 600m
clowctl token create --project shop --env prod      # give this to the project / CI
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
clowctl ps -e prod
clowctl logs api -f -e prod
clowctl rollback api -e prod
```

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
GOOS=linux go build -o bin/clowdrd ./cmd/clowdr
```

License: Apache-2.0.
