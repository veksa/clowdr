# npm registry

clowdrd serves a private npm registry at `https://deploy.<domain>/npm/`, next to the deploy API,
for your own scoped packages: an SDK, shared types, a CLI your services install. It speaks the
part of the npm registry protocol that `npm`, yarn 1 and yarn 2+ use to install and publish, and
nothing more:

- **Scoped packages only** (`@acme/tool`). Unscoped names are refused on publish, so a package
  here can never shadow one from npmjs.
- **No proxy to npmjs.** Public packages still come from npmjs; the project maps only its private
  scopes here (see [Projects](#projects)).
- **Every route needs an npm token**, reads included. A token without rights on a package gets
  the same 404 as a package that does not exist, so a stranger cannot learn what is published.
- **No unpublish, no republish.** A version is immutable: the same name and version always mean
  the same bytes. Old versions leave only through [retention](#retention).
- No search, no users or passwords, no orgs or teams, no stars. `npm audit` and `yarn npm audit`
  get an empty answer (nothing here is in a public advisory database) instead of failing.

## Server

The registry needs `domain` in `platform.yml` (it lives on the deploy host, behind its TLS
certificate) and clowdrd 0.4.0 or newer. Reading works with no configuration; the `npm:` section
turns on publishing and tunes retention:

```yaml
npm:
  publish: true     # accept publishes and dist-tag changes on this server
  keep: 3           # newest versions kept per package by semver, besides every dist-tagged one
  grace: 14d        # how long a dropped version stays deprecated before it is deleted
  max_publish: 64m  # largest publish request (the tarball travels base64 inside it)
```

| Key | Default | Rule |
|---|---|---|
| `publish` | `false` | Turn it on on **one** server. Each server has its own registry; two publishing servers would hold different packages under the same names. |
| `keep` | `3` | at least 1 |
| `grace` | `14d` | longer than 0 |
| `max_publish` | `64m` | larger than 0. A tarball may also unpack to at most 4 × this, capped at 512 MiB, while it is checked. |

A server with `publish: false` answers publishes and tag changes with 405 and keeps serving what
it has.

## Tokens

npm tokens look like `clwn_<id>_<secret>`. They are a kind of their own: an admin or project token
(`clw_…`) is refused by the registry, and an npm token is refused everywhere else. The server keeps
only a SHA-256 of the secret, which is shown once.

A token carries scopes:

| Scope | Allows |
|---|---|
| `npm:read:@acme/*` | installing every package of `@acme` |
| `npm:read:@acme/tool` | installing one package |
| `npm:publish:@acme/*` | publishing and moving dist-tags (and reading) in `@acme` |
| `plugins:read` | the [Claude Code plugin marketplace](plugins.md) at `/plugins/`, listing the plugins of the packages the token may read; nothing alone |

A token lives one year unless asked otherwise (`--expires 90d`). Names are unique: a name still
on record cannot be reused until its token is revoked, or until 30 days after it expired, when the
server forgets it. The CLI warns on stderr when the local token expires within 30 days.

```sh
clowctl npm token ls                 # name, scopes, created, expires, last used
clowctl npm token revoke laptop      # stops on the next request, a token in a rotation's grace too
clowctl npm token rotate laptop      # admin: a new secret, the full lifetime again from now
clowctl npm token rotate             # this machine's own token, no admin needed
clowctl npm token rotate --now       # ...and the old secret stops at once
```

After a rotation the old secret keeps working for 24 hours, so machines that share it can catch
up; `--now` skips that. A token rotating itself keeps its expiry date: only an admin's rotation
renews a token, so a leaked secret cannot keep itself alive. A token can be rotated once; the
holder of the old secret cannot mint a second successor.

For CI or another machine without `clowctl`, create a token with an admin token and put it in the
CI's secrets:

```sh
curl -s -X POST https://deploy.example.com/v1/npm/tokens -H "Authorization: Bearer $CLOWDR_ADMIN" \
  -d '{"name":"ci-publish","scopes":["npm:publish:@acme/*"],"expires_in":"90d"}'
```

The admin API: `GET`/`POST /v1/npm/tokens`, `DELETE /v1/npm/tokens/{name}`,
`POST /v1/npm/tokens/{name}/rotate` (`{"now": true}` optional), `GET /v1/npm/audit`. The token
itself can `GET /npm/-/clowdr/token` (its own record) and `POST /npm/-/clowdr/token/rotate`.

## Machines

```sh
clowctl npm login --scope @acme                   # read everything in @acme
clowctl npm login --scope npm:publish:@acme/* --name release-box
```

`login` uses the admin context (`--context`) to create a token named after the host (`--name`)
and writes it, mode 0600, where every project on the machine finds it:

- `~/.yarnrc.yml`: `npmRegistries."https://deploy.example.com/npm/".npmAuthToken`
- `~/.npmrc`: `//deploy.example.com/npm/:_authToken=…`

Name, scopes and expiry go to `~/.clowdr/config`. The files are checked writable before the
server is asked, so a broken `~/.yarnrc.yml` does not cost a token. `login` refuses to write
through a symlink that leads outside the home folder or into a git work tree (a dotfiles repo
would publish the token): point `YARN_RC_FILENAME` or `NPM_CONFIG_USERCONFIG` elsewhere instead.
Running `login` again while the token works only rewrites the files; other scopes are refused,
because a token's scopes never change: revoke it, then log in. Plain `http://` registries are
allowed only on localhost.

Against a server older than 0.4.0 every `npm` command stops with "server too old for npm".

## Projects

```sh
clowctl npm setup --scope @acme
```

`setup` maps the scope in the project's `.yarnrc.yml` (the nearest one above, never the one in
the home folder; a new one next to the nearest `package.json`):

```yaml
npmScopes:
  acme:
    npmRegistryServer: "https://deploy.example.com/npm/"
npmPreapprovedPackages:
  - "@acme/*"
```

The second key is for yarn 4.10+, which can hold back versions younger than an age gate (on by
default, one day, since 4.15): your own fresh release must install at once. `setup` reads the yarn
version from `packageManager` (else `yarn --version`), adds the key only from 4.10 on, and warns
when it cannot tell. The token stays in the home folder; the project file holds no secret and is
committed.

For npm, put the mapping in the project's `.npmrc`:

```ini
@acme:registry=https://deploy.example.com/npm/
```

**Register the scope on npmjs too**, as an empty organisation. A machine that lacks the mapping
(a fresh CI runner, a teammate who skipped `setup`) asks npmjs for `@acme/tool`; if someone else
owns `@acme` there, that machine installs their code. An organisation you own and leave empty
turns that into a plain 404.

## Publishing

```sh
clowctl npm publish            # the package in this folder, tag from publishConfig.tag, else latest
clowctl npm publish packages/sdk --tag next
```

The package must have a scoped name and `publishConfig.registry` set to a clowdr registry the
machine is logged in to, so it cannot go to npmjs by mistake. It is packed with the project's yarn
or npm and sent with a sha512 integrity and a sha1 shasum. `npm publish` and `yarn npm publish`
work as well, with a publishing token.

The server rebuilds what it serves from the tarball, not from what the client claims. A publish is
refused unless:

- it carries exactly one version and one tarball, with a semver version;
- name and version in the URL, the document, the version object and the tarball's only top-level
  `package.json` all agree;
- `dist.integrity` (sha512) and `dist.shasum` match the bytes;
- the tarball is gzip+tar, has no entry outside its folder and no two entries landing on the same
  path, and fits the unpack limit, counted by the size each entry's header gives;
- every entry is a file, a folder or a link: no sparse file (a few hundred bytes that unpack to
  gigabytes) and no device or FIFO;
- the version was never published before, including versions retention has deleted (409);
- a package with `"claude-plugin": true` also passes the [plugin rules](plugins.md#publish-rules).

Dist-tags move with `npm dist-tag` or `yarn npm tag`; `latest` can be moved but not removed.
Unpublish answers 405.

## Retention

After every publish or tag change, and on the daemon's periodic sweep, each package keeps its
newest `keep` versions by semver and every version a dist-tag points at. Any other version is
deprecated with "removed on <date>, upgrade to <latest>", so `npm install` and yarn print a
warning, and the sweep deletes it once `grace` has passed. A deprecated version that is kept again
(tagged, or newer ones removed) loses the deprecation. A deleted version's tarball goes too, unless
another version has the same bytes; its record stays, so the version cannot be published again
with other contents.

`clowctl npm check` keeps projects off versions on their way out. It reads `yarn.lock` (yarn 1 and
2+), `package-lock.json` and `npm-shrinkwrap.json`, follows aliases
(`"tool": "npm:@acme/tool@^1"`) and workspaces, and reports a locked version that is deprecated or
gone, and a private scope used without a mapping to clowdr. It guards only scopes known to be
private: those of your `npm login` and those the project maps to a clowdr registry. A pnpm or bun
lockfile is reported as unreadable.

`clowctl deploy` runs the check first in a project that uses a mapped scope and stops on a
problem. `--allow-outdated` deploys anyway and marks the release in `clowctl releases`.

## Audit

Every read of a package document, tarball download, publish, tag change and token action is
recorded with the token's name and id, the client address, package, version, tag and status:

```sh
clowctl npm audit --package @acme/tool --since 24h
clowctl npm audit --token laptop --action token     # token.create, token.rotate, token.revoke
clowctl npm audit --action publish --limit 20
```

Actions are `read`, `download`, `publish`, `tags`, `tag.set`, `tag.delete`, `token.*` and, for the
[plugin marketplace](plugins.md), `plugin.list`, `plugin.clashes` and `plugin.download`; `--action token`
matches all three token actions, `--action plugin` all three plugin ones. Entries are kept for 90 days,
at most 200 000.
Requests with a missing or wrong token are not recorded per request: after 20 failures in 10
minutes an address (an IPv6 /64) gets 429 for 10 minutes, with a valid token too.

The client address is the TCP peer, or the last `X-Forwarded-For` hop only when it comes from
clowdr's own nginx. Tokens are never logged, and error bodies never repeat them.

## Storage

Under the state directory (`/var/lib/clowdr`):

- `state.db`: tokens, packages, versions, dist-tags and the audit log, in buckets of their own.
  A 0.3.x binary opens the same database and ignores them.
- `npm/tarballs/<sha512 hex>`: one file per distinct tarball, written atomically.
- `npm/zips/<sha512 hex>.zip`: the zip of each [plugin](plugins.md) tarball, a cache built again
  when missing.

Back up both together; a tarball without its record is never served, a record without its
tarball answers 404 on download.

## Upgrading to 0.4.0

- **Reload nginx by hand after upgrading to 0.4.0.** 0.4.0 tags requests of the deploy-host
  location with `X-Clowdr-Via: deploy-host` and refuses anything else that reaches the API through
  nginx, except ACME challenges. It writes the new `nginx.conf` on start but reloads nginx only
  when a project file changed too, so the API may answer 404 through nginx until you run
  `docker exec clowdr-nginx nginx -s reload -c /etc/clowdr/nginx/nginx.conf`. 0.4.1 and later do
  one checked reload (`nginx -t`, then a graceful reload) on every start; upgrading straight to
  0.4.1 needs nothing.
- **Old CLIs keep working.** A 0.3.x `clowctl` deploys as before; only the `npm` commands and
  `--allow-outdated` need 0.4.0 on both ends.
- **Rolling back to 0.3.x:** remove the `npm:` section from `platform.yml` first, because 0.3.x
  refuses unknown keys and will not start. Packages and tokens stay in the database for the next
  upgrade, but nothing serves them meanwhile. A 0.3.x server drops the `allow-outdated` mark of
  any release it rewrites.
