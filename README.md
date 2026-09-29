# swh-git

Clone archived repositories from [Software Heritage](https://www.softwareheritage.org/) with `git-remote-swh` using `swh::` addresses, or with the `swh-git` HTTP proxy. Both download bare repositories from the Vault and cache them locally.

Building requires Go 1.27.1 or newer, as specified in [go.mod](go.mod). Running either command requires Linux or macOS and Git 2.32.0 or newer; this minimum covers the [`GIT_CONFIG_GLOBAL`](https://git-scm.com/docs/git/2.32.0) setting used to disable global Git configuration. The HTTP proxy also requires `git http-backend`.

## Remote helper

Install the helper and add its install directory to your PATH: `GOBIN` if set, otherwise `$(go env GOPATH)/bin`. Git runs `git-remote-swh` automatically for `swh::` addresses.

```sh
go install github.com/andrew/swh-git/cmd/git-remote-swh@latest

git clone swh::github.com/octocat/Hello-World
git clone swh::swh:1:snp:f72e9d06dd0e58236a34328f094ca894a809f230 hello-archive
git ls-remote --symref swh::github.com/octocat/Hello-World
```

The helper also accepts full origin URLs such as `swh::https://github.com/octocat/Hello-World`, and `swh://` can replace `swh::`. It serves clone, fetch, and `ls-remote` through `git upload-pack`, supports shallow clones, and refuses pushes. To install from a checkout, run `go install ./cmd/git-remote-swh`.

## HTTP proxy

Install and start the proxy, then clone in another terminal. It listens on loopback by default and has no HTTP authentication.

```sh
go install github.com/andrew/swh-git@latest
swh-git
```

```sh
git clone http://127.0.0.1:8080/github.com/octocat/Hello-World.git
git clone http://127.0.0.1:8080/swh:1:snp:f72e9d06dd0e58236a34328f094ca894a809f230.git hello-archive
```

From a checkout, use `go run .`. Flags override the corresponding environment settings:

```sh
go run . -listen 127.0.0.1:8080 -cache /tmp/swh-git-cache -timeout 1h -poll 5s
```

## Addresses and caching

Both commands accept core `snp`, `rev`, `rel`, and `dir` SWHIDs, with an optional `.git` suffix. Qualified SWHIDs and content IDs are not accepted. The [Vault API](https://docs.softwareheritage.org/devel/swh-web/uri-scheme-api-vault.html) reconstructs a snapshot's branches and releases; a directory produces a single commit.

Origin paths default to HTTPS, so include the scheme for HTTP origins: `swh::http://example.org/repo` or `http://127.0.0.1:8080/http://example.org/repo`. If the API returns 404, the lookup retries without a trailing `.git`.

The helper uses an origin's [latest archived snapshot](https://docs.softwareheritage.org/devel/swh-web/uri-scheme-api-origin.html#get--api-1-origin-(origin_url)-visit-latest-) for each connection; the HTTP proxy selects it with a temporary redirect. Later fetches resolve the origin again, while a SWHID stays fixed and works without API access once cached.

The first request waits for Vault to prepare the repository and downloads the whole bundle, even for `git ls-remote` or a shallow clone. Progress goes to stderr, and preparation times out after 30 minutes by default.

Configure either command with the environment variables below. To authenticate API requests, set `SWH_TOKEN` to a [Software Heritage bearer token](https://docs.softwareheritage.org/devel/swh-web-client/index.html#authentication).

| Variable | Default | HTTP flag |
| --- | --- | --- |
| `SWH_CACHE` | `swh-git` inside your OS user cache directory | `-cache` |
| `SWH_API` | `https://archive.softwareheritage.org/api/1/` | `-api` |
| `SWH_TOKEN` | No token | |
| `SWH_TIMEOUT` | `30m` | `-timeout` |
| `SWH_POLL` | `2s` | `-poll` |
| `SWH_MAX_BYTES` | `4294967296` (4 GiB, including tar headers) | `-max-bytes` |

Helper and proxy processes can share a cache on a local filesystem, with a lock per SWHID to prevent duplicate preparation. Disconnecting an HTTP client leaves the shared download running; requests waiting on the same attempt receive its result. After an interrupted process, the next preparation of that SWHID removes its abandoned staging directories.

Cached repositories remain across restarts, with no automatic eviction. Cloning depends on what Software Heritage archived and whether Vault can prepare it. Neither command retrieves submodule repositories or Git LFS content.

Run the tests with `go test -race ./...`. They use real Git clones and fetches against a local fake Vault API, including refs, shared caches, interrupted processes, and unsafe bundles.

## License

MIT License. See [LICENSE](LICENSE).
