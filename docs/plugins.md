# Claude Code plugins

clowdrd turns the [npm registry](npm.md) into a private
[Claude Code](https://code.claude.com/docs/en/plugins/marketplace-reference) plugin marketplace.
A plugin is an npm package of the registry with one more field in its `package.json`; you publish
it like any other package, and every machine with the right token installs it in Claude Code:

- `https://deploy.<domain>/plugins/marketplace.json` lists, for the token that asks, the latest
  version of every plugin package that token may read;
- `https://deploy.<domain>/plugins/@acme/tool/1.2.0.zip` serves one version as the zip Claude
  Code installs, with its sha256 in the marketplace;
- `clowctl plugins setup` adds the marketplace to Claude Code once per machine; Claude Code then
  asks `clowctl plugins headers` for the token on every fetch.

It needs clowdrd 0.5.0 or newer and `domain` in `platform.yml`: Claude Code fetches over https
with a verified certificate, so a server on `https://<ip>:7443` with a self-signed certificate
cannot serve plugins.

## A plugin package

```text
tool/
├── package.json
├── .claude-plugin/
│   └── plugin.json
├── commands/hello.md
├── skills/…
└── hooks/run.sh
```

```json
{
  "name": "@acme/tool",
  "version": "1.2.0",
  "claude-plugin": true,
  "files": [".claude-plugin", "commands", "skills", "hooks"],
  "publishConfig": { "registry": "https://deploy.example.com/npm/" }
}
```

```json
{ "name": "tool", "version": "1.2.0", "description": "Acme's tools for Claude Code" }
```

`files` matters: npm leaves out what it is not told to pack, and a dot folder is easy to lose.
Publish with `clowctl npm publish`, `npm publish` or `yarn npm publish` and a token with
`npm:publish:@acme/*`.

### Publish rules

A package is a plugin when its tarball's `package.json` has `"claude-plugin": true`. The server
reads the tarball, not what the client claims, and refuses the publish (400) unless:

- `"claude-plugin"` is `true` or `false` (absent counts as `false`). A package without it, or with
  `false`, is an ordinary package even when it holds a `.claude-plugin/plugin.json`;
- the package root holds `.claude-plugin/plugin.json`: a regular file of at most 1 MiB with a JSON
  object;
- its `name` is the package name without the scope (`tool` for `@acme/tool`). Users install
  `tool@<marketplace>`, and the name a plugin calls itself must not differ from the one it is
  listed under;
- its `version` is set and equals the package version. Claude Code takes the version from
  `plugin.json` and updates an installed plugin only when it changes, so a plugin.json left at an
  old version would make a new release invisible;
- `description`, when present, is a string;
- the name after the scope uses only `a-z`, `0-9`, `.`, `_` and `-` and is at most 128 characters
  (npm also allows `~`, Claude Code does not);
- the package holds regular files and folders only: no symlinks or hard links, which a zip
  would drop;
- at most 100 000 files, and the zip comes out at most 256 MiB: Claude Code's own download and
  extraction limits.

Everything else is the npm registry's: one version per publish, immutable versions, retention.

## The marketplace

```json
{
  "name": "clowdr-example-com",
  "owner": { "name": "example.com" },
  "description": "Private Claude Code plugins on deploy.example.com",
  "plugins": [
    {
      "name": "tool",
      "description": "Acme's tools for Claude Code",
      "version": "1.2.0",
      "source": {
        "source": "archive",
        "url": "https://deploy.example.com/plugins/@acme/tool/1.2.0.zip",
        "sha256": "…64 hex…"
      }
    }
  ]
}
```

- **One entry per package**, at the version its `latest` dist-tag points at. A prerelease
  published with `--tag next` is not listed until `latest` moves to it; every live version can
  still be downloaded by its URL. When `latest` points at a version that is not a plugin, the
  package is not listed.
- **The entry `name` is the plugin name** (the unscoped package name). If two scopes the token
  reads hold a plugin of the same name (`@acme/tool` and `@beta/tool`), the first package by name
  keeps it and the other is left out (the audit entry of the listing names it): a marketplace
  lists a name once. Give plugins distinct names.
- **URLs are absolute and on the marketplace's own origin**, so Claude Code sends the
  marketplace's headers with each zip download too. A cross-origin redirect would drop them,
  and there is none.
- **The marketplace name** is `clowdr-` and the server's `domain` with every dot (and any other
  character but `a-z0-9`) as a dash: `example.com` gives `clowdr-example-com`. It only changes
  with the domain. To choose it, set it in `platform.yml`:

  ```yaml
  plugins:
    name: acme-tools   # letters, digits, '.', '_', '-'; no "claude" or "anthropic" in it
    owner: Acme Inc.   # shown as the owner (default: the domain)
  ```

  Users type the name after `@` (`claude plugin install tool@acme-tools`) and their settings are
  keyed by it, so changing it later means every machine runs `clowctl plugins setup` again and
  removes the old entry (setup warns about it).
- A token that may read no plugin gets a valid, empty marketplace (`"plugins": []`).

## Access

The marketplace takes the npm tokens of the registry. The scope `plugins:read` opens it; it
grants nothing alone:

| Token | `marketplace.json` | a plugin's zip |
|---|---|---|
| none, wrong, expired or revoked; an admin or project token | 401 | 401 |
| `npm:read:@acme/*` | 404 | 404 |
| `plugins:read` | 200, empty | 404 |
| `plugins:read` + `npm:read:@acme/*` (or `npm:publish:…`) | 200, `@acme`'s plugins | 200 |

A token without the right on a package gets for its zip exactly what a package that does not
exist gets: the same 404, byte for byte. Failed authentications count towards the registry's
limit (20 in 10 minutes per address, then 429 with any token).

```sh
clowctl npm login --scope plugins:read --scope @acme
```

A token's scopes never change: a machine logged in without `plugins:read` revokes its token and
logs in again (`clowctl npm token revoke <name>`, then the line above). For CI, create a token
through the admin API with `"scopes": ["plugins:read", "npm:read:@acme/*"]`.

## Machines

```sh
clowctl plugins setup                 # once per machine (--context picks the server)
claude plugin marketplace update clowdr-example-com
claude plugin install tool@clowdr-example-com
```

`setup` reads the marketplace with the machine's npm token (so it fails early when the token
lacks `plugins:read` or the server is older than 0.5.0) and writes Claude Code's user settings,
`~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR/settings.json`):

```json
{
  "extraKnownMarketplaces": {
    "clowdr-example-com": {
      "source": {
        "source": "url",
        "url": "https://deploy.example.com/plugins/marketplace.json",
        "headersHelper": "/usr/local/bin/clowctl plugins headers --registry https://deploy.example.com/npm/"
      },
      "autoUpdate": true
    }
  }
}
```

- **The token stays out of the settings.** Claude Code runs the `headersHelper` before each fetch
  of the marketplace and of each zip, with no prompt (it is in user settings), also on its
  background refresh. `clowctl plugins headers` prints `{"Authorization":"Bearer clwn_…"}` from
  `~/.clowdr/config` and nothing else on stdout, makes no request, and prints no expiry warning;
  without a token for the registry it exits non-zero with the reason on stderr. A rotated token
  (`clowctl npm token rotate`) is picked up on the next fetch.
- **The helper is the absolute path of the clowctl that ran `setup`**: Claude Code runs it with
  `sh` (`cmd.exe` on Windows) from `~/.claude`, where `PATH` may differ. Move or reinstall clowctl
  elsewhere and run `setup` again. A path with spaces is quoted for the shell (`'…'` for `sh`,
  `"…"` for `cmd.exe`); Claude Code takes at most 500 printable ASCII characters, so a path
  outside that is refused with a message.
- **`autoUpdate: true`**: Claude Code turns auto-update off for marketplaces it does not ship, so
  without it a new version would wait for `claude plugin update tool@clowdr-example-com`.
- Every other key of the file stays as it was, in its place; the file is rewritten with two-space
  indentation, atomically, keeping its mode. Run again with nothing to change, `setup` leaves the
  file alone. Like `npm login`, it refuses to write through a symlink that leads outside the home
  folder or into a git work tree.

```sh
clowctl plugins ls        # what the marketplace offers this machine's token
```

### Updates

Publish a new version with the same version in `package.json` and `plugin.json`, and move
`latest` if it is not the default tag. Claude Code sees it on its next marketplace refresh
(`claude plugin marketplace update <name>` refreshes at once) and, with `autoUpdate`, installs
it; `claude plugin update tool@<name>` upgrades by hand. An installed plugin lives in
`~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/`.

## The zip

The server builds a version's zip once, when it is published, from the stored tarball:

- the tarball's top folder (`package/`) is stripped, so the plugin root, with
  `.claude-plugin/plugin.json`, is the top of the zip;
- regular files only, sorted by path, all dated 1980-01-01 00:00 UTC;
- mode 0755 for a file with any execute bit (hook scripts), 0644 for the rest;
- deflated, except an entry that would expand more than 50 times on extraction (Claude Code's
  ratio limit) or would not shrink: that one is stored.

The same tarball gives the same bytes. The zip's sha256 is computed when it is built and kept
with the version: the marketplace's `sha256` is always the digest of the bytes served, and the
zip's `ETag`. Responses are `Cache-Control: private, no-cache` (marketplace: `private, no-store`).

A zip missing from disk is built again on the next request. Deflate's output is not promised
across Go releases, so a rebuild by a newer clowdrd may give other bytes; it then records the new
digest before it serves anything, and the next marketplace fetch names it.

A version published on clowdrd 0.4 with `"claude-plugin": true` is checked against the rules the
first time the marketplace would list it: if it passes, its zip is built then; if not, it is not
listed and the reason is logged once.

## Audit

Two more actions in `clowctl npm audit`: `plugin.list` for each fetch of `marketplace.json` (the
detail says how many plugins it listed, and which packages lost a name to another scope) and
`plugin.download` for each zip, with package and version. `--action plugin` matches both.

## Storage

- `npm/zips/<sha512 hex>.zip`: one zip per plugin tarball, keyed like the tarball it was built
  from, written atomically.
- The plugin's name, description, zip sha256 and size sit in the version's record in `state.db`.

Retention deletes a version's zip together with its tarball, unless another live version has the
same tarball; the periodic sweep also removes any zip no live version uses. A zip is a cache: losing it costs one rebuild, and backups may skip `npm/zips/`.

## nginx

Nothing to configure: the deploy host's one location passes every path to clowdrd, `/plugins/`
like `/npm/`, without a body limit and marked as the deploy host's own. The access log records the
path, never the `Authorization` header.

## Upgrading to 0.5.0

- Nothing to do on the server; the marketplace answers once 0.5.0 runs. Tokens need
  `plugins:read` to see it.
- **Rolling back to 0.4.x:** remove the `plugins:` section from `platform.yml` first (0.4.x refuses
  unknown keys). 0.4.x ignores the plugin fields of version records and may drop them when it
  rewrites a record (retention); 0.5.0 checks such a version again from its tarball, so nothing is
  lost. Zips of versions 0.4.x deletes stay in `npm/zips/` until the next 0.5.0 sweep, which removes
  every zip no live version uses.
- An older clowctl has no `plugins` command; a 0.5.0 clowctl against an older server stops with
  "server too old for plugins".
