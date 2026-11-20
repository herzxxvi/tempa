# Tempa

Tempa is a GitHub-compatible git forge in one process. It serves git smart HTTP, the REST v3 API,
the GraphQL v4 API and a Model Context Protocol endpoint over a single in-memory store, so a branch
pushed with `git`, an issue opened through REST, a pull request merged through GraphQL and a tool
call over MCP all act on the same objects and read back identically. It is written in Go against the
standard library only: its own HTTP/1.1 engine, git object database, pack reader and writer,
three-way merge, and GraphQL lexer, parser, validator and executor. There is no git binary, no
database and no third-party module. A small Node 22 launcher, `main.ts`, starts the binary.

Tempa was built as an entry for the make.fun bounty `github-core-clone-01`: a standalone,
GitHub-compatible forge, graded by a conformance suite and four timed workloads. The name is
Indonesian for "to forge"; the Go module and the binary are called `forge`.

Scope, stated plainly: one user (the owner of the bearer token), all state in memory for the life of
the process, plain HTTP, no web interface.

## The four surfaces

| Surface | Mount point | Protocol |
|---|---|---|
| Git | `/{owner}/{repo}.git/` | smart HTTP: upload-pack protocol v2 (and a basic v0), receive-pack v0 |
| REST | `/api/v3/` | GitHub REST v3 shapes, status codes, error bodies and `Link` pagination |
| GraphQL | `/api/graphql` | GitHub GraphQL v4 subset with full introspection |
| MCP | `/mcp` | JSON-RPC 2.0 over Streamable HTTP, one JSON body per answer |

Every surface authenticates first. The token is accepted as `Authorization: Bearer <token>`,
`token <token>`, or as the password of Basic credentials (what git clients send). `/healthz` needs
no token.

### Git smart HTTP

| Area | Supported |
|---|---|
| Fetch, protocol v2 | `ls-refs` (`symrefs`, `peel`, `unborn`, server-side `ref-prefix`); `fetch` with negotiation rounds (`have` without `done` gets `acknowledgments`) |
| Fetch, protocol v0 | a basic fetch for older clients: `side-band-64k`, `side-band`, `shallow`, single ACK/NAK (no `multi_ack`) |
| Shallow history | `deepen`, `deepen-relative`, `deepen-since`, `deepen-not`; `shallow` and `unshallow` replies, so `--depth`, `--deepen` and `--unshallow` work from a real client |
| Push | `report-status`, `delete-refs`, `side-band-64k`, `ofs-delta`; several ref updates per push, each judged on its own (create, fast-forward update, delete) with git's refusal reasons such as `non-fast-forward` |
| Incoming packs | version 2 packs with `OFS_DELTA` and `REF_DELTA` entries and thin packs (bases taken from the store); trailer SHA-1, every zlib stream, declared sizes and the entry count are verified |
| Connectivity | everything a pushed commit, tree or tag names must exist; otherwise the pack is taken out again and the update is refused |
| Outgoing packs | whole objects, each once, in band 1 of `side-band-64k`; a pushed object leaves in the zlib stream it arrived in |
| Requests | gzip-compressed upload-pack bodies, chunked bodies, `Expect: 100-continue` |

Not served: partial clone filters, `want-ref`, `sideband-all`, `report-status-v2`, push options.

### REST v3

| Resource | Routes (below `/api/v3`) |
|---|---|
| Root and user | `GET /`, `GET /meta`, `GET /user`, `GET /users/{user}` |
| Repositories | `GET`, `POST /user/repos` (with `auto_init`); `GET /users/{user}/repos`; `GET`, `PATCH`, `DELETE /repos/{owner}/{repo}` (rename, description, visibility, issues on or off, archive, default branch) |
| Branches and refs | `GET .../branches`, `.../branches/{branch}`; `GET`, `POST .../git/refs`; `GET`, `PATCH`, `DELETE .../git/refs/{ref}`; `GET .../git/ref/{ref}`, `.../git/matching-refs/{ref}` |
| Commits | `GET .../commits` (`sha`), `GET .../commits/{ref}` |
| Contents | `GET .../contents[/{path}]` (`ref`; file or directory), `PUT` and `DELETE .../contents/{path}` (one commit each, `sha` precondition, optional `branch`) |
| Labels | `GET`, `POST .../labels`; `GET`, `PATCH`, `DELETE .../labels/{name}` |
| Issues | `GET`, `POST .../issues` (`state`, `labels`, `sort`, `direction`); `GET`, `PATCH .../issues/{number}`; `GET .../issues/{number}/labels`; `GET`, `POST .../issues/{number}/comments`; `GET .../issues/comments/{id}` |
| Pull requests | `GET`, `POST .../pulls`; `GET`, `PATCH .../pulls/{number}`; `GET .../pulls/{number}/files`; `GET`, `PUT .../pulls/{number}/merge` (`merge`, `squash`, `rebase`) |

List routes take `per_page` (default 30, at most 100) and `page`, and carry a `Link` header while
more pages exist. `HEAD` is answered by the `GET` handler. Errors use GitHub's bodies
(`message`, `errors[]`, `documentation_url`) and statuses (400, 401, 404, 405, 409, 410, 422).

Pull requests are computed from the git data: mergeability, changed files and line counts come from
the merge base and the current branch tips, and a merge is a three-way tree merge at path level
(no line-level merging: two sides changing the same file differently is a conflict).

### GraphQL v4

- **Queries:** `viewer`, `repository`, `node`, `nodes`, `user`, `repositoryOwner`, `search`.
- **Mutations:** `createIssue`, `closeIssue`, `reopenIssue`, `addComment`, `createPullRequest`,
  `mergePullRequest`, `closePullRequest`. They call the same store operations as REST.
- **Types:** `Repository`, `Ref`, `Commit`, `Issue`, `PullRequest`, `PullRequestChangedFile`,
  `IssueComment`, `Label`, `User`, with Relay connections (`edges`, `nodes`, `pageInfo`,
  `totalCount`; `first`/`after`, `last`/`before`) and cursors that name an item, not a position.
- **Language:** variables, fragments and inline fragments, aliases, `@skip` and `@include`,
  `operationName`, block strings. Documents are validated against the schema before anything runs.
- **Introspection:** `__schema`, `__type` and `__typename`, served from the same registry that
  executes queries, so the two cannot disagree.
- **Batching:** a JSON array of operations is answered with an array of results in the same order.
- **Ids:** `id` is the same node id as REST's `node_id`, and `databaseId` the same number as REST's
  `id`.

The type system lives in one SDL file, `internal/graphql/schema.graphql`, parsed at start-up by the
package's own parser. A field without a resolver, or a resolver without a field, stops the process
from starting. Requests are bounded: 1 MiB per document, a depth limit, and a ceiling on the number
of fields and list items one operation may produce.

### MCP

- **Revisions:** `2026-07-28` (`server/discover`, no handshake required) and the `initialize`
  handshake of `2025-11-25`, `2025-06-18`, `2025-03-26` and `2024-11-05`.
- **Methods:** `server/discover`, `initialize`, `ping`, `tools/list`, `tools/call`; notifications
  are accepted with 202.
- **Tools:** `create_repo`, `get_repo`, `create_issue`, `list_issues`, `create_pull_request`,
  `merge_pull_request`.

Stateless: no sessions and no event streams. A tool result carries the bytes of the REST renderer
for the same object, as text and, for a single object, as `structuredContent` too; a refusal by the
store is an `isError: true` result with the body REST would have sent. `tools/list` and argument
checking are generated from one table, so the advertised schema and the accepted input cannot
drift.

## Design in brief

The full description, including the store's API and the conventions each surface keeps, is in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The main decisions:

- **A hand-written HTTP/1.1 engine** (`internal/httpd`) instead of `net/http`. Requests are views
  into the connection's input buffer and responses are appended to its output buffer. Every complete
  request of a read is answered and all responses leave in one write. No `Date` header, no
  compression, no per-request logging, no allocation in the engine itself.
- **One store, one mutex.** The dispatcher locks the store around each request, so every operation
  is atomic, lazily built caches are filled once even under concurrent identical requests, and no
  surface contains locking code.
- **One renderer per object type.** REST, MCP and anything else that shows a REST object call the
  same hand-written `append` renderer, so an object is byte for byte the same everywhere. Issues
  keep their rendered response until they change.
- **Git work is done once.** The v2 capability advertisement is a constant; ref advertisements,
  `ls-refs` replies and clone-shaped fetch responses are cached per ref state and dropped on any ref
  change, whichever surface caused it.
- **A push costs what its pack costs.** Objects are inflated, hashed and checked against what they
  reference; nothing is derived from history. The fast-forward test looks at the new commit's
  parents first, so the usual push is decided without a walk, and generation numbers bound the walk
  otherwise.
- **A logical clock.** Every mutation advances a counter by one second from a fixed epoch;
  timestamps, ids, node ids and the SHAs of commits made through the API depend only on the sequence
  of requests. Two runs fed the same requests end in the same state.
- **Runtime settings for small, mostly sequential requests:** `GOMAXPROCS=1`, the garbage collector
  off with a 256 MiB memory limit as backstop (a full conformance and benchmark session allocates
  about 21 MiB in total), and a `GODEBUG` setting that keeps Go's monitor thread from being woken
  with a futex call on every request. Each yields to the standard environment variable.
- **A Linux epoll loop.** On Linux all connections are served from one goroutine on raw,
  level-triggered epoll: one wait, one read and one send per exchange (`FORGE_IO=netpoll` switches
  back to Go's netpoller, which every other platform uses). The connection state machine is separate from the system calls and is tested on every
  platform against a scripted kernel.
- **Bounded input.** Request bodies up to 32 MiB, headers up to 16 KiB, packs up to 96 MiB inflated
  and 32 MiB per object; a size claimed by a client is never allocated in advance.

Micro-benchmarks on the development machine (`go test -bench`, Windows, Go 1.26.4), each a complete
request through parser, router, store and response writer:

| Request | Time | Allocations |
|---|---|---|
| `GET` of one issue | about 0.45 µs | 0 |
| `POST` of one issue | about 1.8 µs | 3 |
| One push (advertisement, then a pack of 3 objects) | about 16 µs | 0 |
| One shallow clone (5 requests) | about 2.4 µs | 0 |

## Running

Requirements: Node 22.18 or newer. On Linux amd64 nothing else, because the binary is committed in
`bin/`. On Windows, Go 1.26 and `bash build.sh`; on any other platform, build `./cmd/forge` and
name the result in `FORGE_BIN`.

```sh
export MAKEFUN_ENV_URL=http://127.0.0.1:9000   # the environment to register with
export MAKEFUN_SHOP_TOKEN=change-me            # the token clients must present
node main.ts
```

The forge listens on a port chosen by the operating system and prints it
(`[forge] listening on port 40871 (netpoll)`), posts `{"port": N}` to `$MAKEFUN_ENV_URL/register`,
polls `$MAKEFUN_ENV_URL/done` and exits 0 once that answers `{"done": true}`. It serves from the
moment it listens and keeps retrying the registration, so for a manual session any `http://` URL
will do:

```sh
curl -H "Authorization: Bearer change-me" -d '{"name":"demo"}' http://127.0.0.1:40871/api/v3/user/repos
git clone http://x:change-me@127.0.0.1:40871/adaiversen/demo.git
```

| Variable | Meaning |
|---|---|
| `MAKEFUN_ENV_URL` | required; `http://` URL of the environment that receives the registration |
| `MAKEFUN_SHOP_TOKEN` | the bearer token; if empty, any credentials are accepted (a warning is logged) |
| `FORGE_IO` | `epoll` (Linux only, the default there) or `netpoll` (the default elsewhere) |
| `FORGE_PREDICT`, `FORGE_DEFER_ACCEPT`, `FORGE_STATS` | `1` switches on an experiment of the epoll loop: predictive reads, the deferred accept path, a line of I/O counters at exit |
| `FORGE_BIN` | development: path of another build for `main.ts` to start |
| `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, `GODEBUG` | override the runtime defaults described above |

## Building and testing

```sh
go vet ./... && go test ./...   # needs only Go; about 130 tests
bash build.sh                   # builds bin/forge-linux-amd64 (static) and a Windows binary
```

`build.sh` produces a static, stripped, reproducible Linux binary (`CGO_ENABLED=0`, `-trimpath`,
empty build id) and refuses one that is dynamically linked or reaches 8 MiB. Without the launcher,
`go run ./cmd/forge` starts the same server. Surface tests send complete requests through the real
parser, dispatcher and response writer, so status, headers and body are checked together.

`TestRealGit` drives the git command line client against the server when a git binary is installed:
clone of an empty repository, pushes with thin-pack deltas, clone with `fsck --strict`, shallow
clone, `--deepen`, `--unshallow`, refused non-fast-forward pushes, branch deletion and protocol v0.
Benchmarks for the request paths above sit next to the tests (`go test -run xxx -bench . -benchmem`
in `internal/httpd`, `internal/rest`, `internal/githttp`, `internal/gitobj`).

## Verification

The bounty's conformance suite has 120 checks: git 30, repositories 15, issues 15, pull requests
20, protocol 5, GraphQL 20 and MCP 15. It also times four workloads (push, clone, issue read, issue
write) and exports the resulting state, which must not differ between a session and its replay.

| Where | What ran | Result |
|---|---|---|
| Windows 11, development machine | the suite through its local grader | 120/120, replay matched |
| Linux (Ubuntu 24.04, GitHub Actions) | `go vet`, `go test` including the epoll loop on a real kernel and `TestRealGit` with git 2.56.1, then the suite twice per run | 120/120 both times, the two state exports byte-identical |
| gVisor (`runsc` release-20261116.0, systrap, Docker, 2 CPUs) | the suite in a fresh sandbox per run | 120/120 in all 131 runs of the first measurement round and all 65 runs of the epoll comparison (35 of them on the epoll loop) |

Every recorded state export, on all three, has the same SHA-256,
`a0e98d49854b961230227867235c8615858e006db3763f341ac6154af1445590`, which is what the logical clock
and the ordered store are for. The Linux job also rebuilds the binary and compares it with the
committed one; they are the same bytes whether built on Windows or on Linux.

Measurements that shaped the design, from the same Linux and gVisor jobs:

- System calls for one pass of the suite (about 9,500 responses, `strace -f -c` on plain Linux):
  61,000 to 71,000 with stock runtime settings, about 43,000 with the monitor-thread setting
  (4.5 per response), and about 30,500 on the epoll loop (3.2 per response).
- Under gVisor the monitor-thread setting shortened the timed workloads by 3.8 % (push), 4.5 %
  (read), 6.5 % (write) and 1.1 % (clone) over 15 paired runs. The epoll loop took a further
  3.7 % off read and 1.4 % off push over 15 paired runs (clone and write within noise), and a third
  off the forge's CPU time.
- A full session allocates 20.5 MiB in total; the garbage collector does not run.

Absolute timings depend on the machine and are not quoted here: the same binary varied by more
between two CI runners than between any two of the variants compared.

## Repository layout

```
main.ts                 launcher: starts the binary, forwards signals, passes its exit code on
build.sh                builds bin/forge-linux-amd64 (committed) and a Windows binary
bin/                    the committed static Linux binary
cmd/forge/              process entry: runtime settings, wiring, register and /done life cycle
internal/httpd/         HTTP/1.1 engine: parser, response writer, router, auth, the two I/O loops
internal/gitobj/        git objects: ids, encodings, object database, trees, history, merge, diff
internal/store/         the state and every domain operation (repos, refs, contents, issues, pulls)
internal/render/        one JSON renderer per REST object type
internal/app/           builds the store and the surfaces; the dispatcher that locks and routes
internal/rest/          REST v3
internal/graphql/       GraphQL v4: language, schema.graphql, validation, execution, introspection
internal/mcp/           MCP transport and tools
internal/githttp/       git smart HTTP: advertisements, upload-pack, receive-pack, packs, pkt-lines
docs/ARCHITECTURE.md    the design in detail
.makefun/               bounty metadata
```

Dependencies point one way: `app -> {rest, graphql, mcp, githttp} -> render -> store -> gitobj`.
`store` and `gitobj` know nothing about HTTP or JSON, and no surface imports another.

## Not implemented

Persistence, multiple users and organisations, forks, webhooks, a web interface, TLS. In REST:
editing and deleting comments, assignees and milestones, adding or removing single labels on an
issue, the commits of a pull request, the tags, trees and blobs APIs, and `since`, `path` and
`author` filters on lists. In git: partial clone, and `multi_ack` on protocol v0.

## License

MIT. See [LICENSE](LICENSE).
