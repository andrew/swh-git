# swh-git

swh-git is a local Go proxy for cloning archived repositories from [Software Heritage](https://www.softwareheritage.org/). It downloads bare repositories from the Vault and serves the cached copies through `git http-backend`.

Building requires Go 1.27.1 or newer, as specified in [go.mod](go.mod). Running the proxy requires Git 2.32.0 or newer, including `git http-backend`; this minimum covers the [`GIT_CONFIG_GLOBAL`](https://git-scm.com/docs/git/2.32.0) setting used to disable global Git configuration.

With Go and Git on your PATH, install and start the proxy. The install command places the binary in `GOBIN`, or `$(go env GOPATH)/bin` by default; add that directory to your PATH.

```sh
go install github.com/andrew/swh-git@latest
swh-git
```

Or run it from a local checkout:

```sh
go run .
```

Clone an archived origin in another terminal:

```sh
git clone http://127.0.0.1:8080/github.com/octocat/Hello-World.git
```

To clone a specific snapshot:

```sh
git clone http://127.0.0.1:8080/swh:1:snp:f72e9d06dd0e58236a34328f094ca894a809f230.git hello-archive
```

The proxy accepts core `snp`, `rev`, `rel`, and `dir` SWHIDs, with an optional `.git` suffix. It does not accept qualified SWHIDs or content IDs. The [Vault API](https://docs.softwareheritage.org/devel/swh-web/uri-scheme-api-vault.html) reconstructs a snapshot's branches and releases; a directory produces a single commit.

Origin paths default to HTTPS, but you can include the scheme: `http://127.0.0.1:8080/https://github.com/octocat/Hello-World.git`. Use an explicit `http://` prefix for HTTP origins. The proxy looks up the supplied origin URL, then retries without a trailing `.git` if the API returns 404.

An origin resolves to its [latest archived snapshot](https://docs.softwareheritage.org/devel/swh-web/uri-scheme-api-origin.html#get--api-1-origin-(origin_url)-visit-latest-). A temporary redirect keeps Git's requests on that snapshot throughout the clone. Later fetches resolve the origin again, while a SWHID URL stays fixed and works without API access once cached.

The first clone waits while Vault prepares the repository and the proxy downloads it. Status messages appear in the proxy's terminal, and preparation times out after 30 minutes by default. Change the timeout and polling interval with:

```sh
go run . -listen 127.0.0.1:8080 -cache /tmp/swh-git-cache -timeout 1h -poll 5s
```

Repositories remain in the cache across restarts, with no automatic eviction. The default location is `swh-git` inside your OS user cache directory. Use one proxy process per cache directory; concurrent requests within that process share preparation of the same SWHID. `-max-bytes` limits each unpacked bundle, including tar headers, to 4 GiB by default.

Set `SWH_TOKEN` to a [Software Heritage bearer token](https://docs.softwareheritage.org/devel/swh-web-client/index.html#authentication) before starting the proxy to authenticate API requests. For testing against another API server, use `-api` to override the API root.

The proxy supports clone and fetch, refuses pushes, and listens on loopback by default. Its HTTP server has no authentication. Cloning depends on what Software Heritage archived and whether Vault can prepare it; the proxy does not fetch submodule repositories or Git LFS content.

Run the tests:

```sh
go test -race ./...
```

The tests run real Git clones and fetches against a local fake Vault API, without calling Software Heritage. They verify repository contents and refs, cache reuse across restarts, concurrent requests, and error handling.

## License

MIT License. See [LICENSE](LICENSE).
