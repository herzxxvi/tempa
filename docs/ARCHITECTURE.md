# Architecture

The forge is one Go process: a hand-written HTTP/1.1 engine, one in-memory store, and four thin
surfaces over that store (REST v3, GraphQL v4, MCP, git smart HTTP). `main.ts` only starts the
binary. Everything is standard library; there is no git binary, no database and no file I/O at run
time.

Read sections 1 to 6 before you write code in any package. Section 7 is the store's API, the part
every surface calls. Section 9 lists the rules each surface must keep.

---

## 1. Layout

```
main.ts                 launcher: starts bin/forge-<platform>, passes its exit code on
build.sh                builds bin/forge-linux-amd64 (committed, mode 100755) and the Windows build
cmd/forge/              main.go (runtime tuning, wiring), lifecycle.go (register, /done polling)
internal/httpd/         HTTP/1.1 engine: parser, response writer, router, auth check, test harness
internal/gitobj/        git object model: ids, encodings, object DB, trees, history, merge, diff
internal/store/         the state and every domain operation (repos, refs, contents, issues, pulls)
internal/render/        the one JSON renderer per REST object type
internal/app/           builds the store and the surfaces; the dispatcher that locks and routes
internal/rest/          REST surface      (/api/v3/...)
internal/graphql/       GraphQL surface   (/api/graphql)
internal/mcp/           MCP surface       (/mcp)
internal/githttp/       git surface       (/{owner}/{repo}.git/...)
docs/ARCHITECTURE.md    this file
```

Dependencies point one way: `app -> {rest, graphql, mcp, githttp} -> render -> store -> gitobj`, and
every surface uses `httpd`. `store` and `gitobj` know nothing about HTTP or JSON. A surface never
imports another surface.

Who owns what: `httpd`, `gitobj`, `store`, `render`, `app`, `githttp` and `cmd/forge` are the core's.
`rest`, `graphql` and `mcp` each belong to their coder. If you need a change in a package that is
not yours, ask for it; do not work around it inside your package.

---

## 2. Life cycle

`node /bot/main.ts` spawns `bin/forge-linux-amd64` with inherited stdio and environment, forwards
SIGTERM and SIGINT, and exits with the binary's exit code. Two things make it more than a spawn:

- **Host name.** Before starting the binary it resolves the host of `MAKEFUN_ENV_URL` with the
  system resolver (`dns.lookup`, 2 s limit) and passes the URL with the address in `FORGE_ENV_URL`.
  The binary is static and resolves names with Go's own resolver; it tries `FORGE_ENV_URL` first and
  `MAKEFUN_ENV_URL` after it (and sticks with whichever answered), so neither resolver alone can
  cost the registration. An address, or a failed lookup, simply leaves `FORGE_ENV_URL` unset.
- **Exec bit.** If the binary cannot be executed (`EACCES`), the launcher copies it to the temp
  directory (then `/dev/shm`), sets mode 755 and starts the copy.

Development only: `FORGE_BIN=<path>` runs another build, and where the platform's build is not in
`bin/` (the Windows build is not committed, and the local grader runs a copy of the checkout made
from git's file list) the launcher uses the copy `build.sh` keeps in `~/.cache/forge/`.

The binary does everything else (`cmd/forge`):

1. `MAKEFUN_ENV_URL` unset or not `http://...`: one line on stderr, exit 1.
2. Listen on all interfaces, port chosen by the OS, never 8080 (`httpd.Listen`), start serving.
3. Probe its own `/healthz` over loopback until it answers 200.
4. `POST <env>/register {"port":N}`: 200 and 409 are success, 400 and 405 are fatal (exit 1),
   anything else is retried after 250 ms, doubling to 2 s.
5. Poll `GET <env>/done` with the same backoff until the body says `{"done":true}`; exit 0.

The life cycle lives in the Go binary, not in `main.ts`, on purpose: there is no port to hand over a
pipe, the Node process stays completely idle (it would otherwise run a `fetch` every two seconds on
the machine the benchmark is timed on), and the client is 150 lines with tests against both response
framings (`cmd/forge/lifecycle_test.go`). The process writes three lines to stderr in its whole life
and nothing per request.

Runtime settings (`tune()` in `main.go`; each yields to the standard environment variable, which
`main.ts` also sets): `GOMAXPROCS=1`, garbage collector off with a 512 MiB memory limit as backstop.
One P because requests arrive one at a time on three of the four timed workloads and a second P only
adds wake-ups; no GC because the hot paths do not allocate.

---

## 3. Request flow

```
socket -> httpd engine (parse)                      one goroutine per connection
       -> app.Serve (dispatch by path prefix)       takes the store mutex
       -> surface (rest / graphql / mcp / githttp)  decodes, calls ONE store operation
       -> store                                     changes state, returns live objects or *store.Error
       -> render                                    appends JSON bytes
       -> httpd (c.JSON / c.JSONTail / ...)         appends the response to the connection's buffer
       <- app.Serve returns                         mutex released
       <- engine writes the buffer                  one write for all responses of the read
```

Dispatch (`internal/app/app.go`):

| Path | Surface | Store locked |
|---|---|---|
| `/api/v3`, `/api/v3/...` | `rest.API.Serve(c, sub)`, `sub` = path after `/api/v3` | yes |
| `/{owner}/{repo}.git[/...]` | `githttp.Server.Serve(c, owner, repo, sub)` | yes |
| `/api/graphql` | `graphql.API.Serve(c)` | yes |
| `/mcp` | `mcp.Server.Serve(c)` | yes |
| `/healthz` | `200 {"ok":true}`, no token | no |
| anything else | `404 {"message":"Not Found"}` | no |

Authentication is each surface's first step (the 401 bodies differ per surface); the engine only
provides the check, `c.Auth()`.

---

## 4. The HTTP engine (`internal/httpd`)

### 4.1 What it guarantees

- Persistent connections. The server closes a connection only when the client sent
  `Connection: close` (or HTTP/1.0 without keep-alive), the request could not be parsed (the error
  reply says `Connection: close`), or the socket failed. There are no idle timeouts, no read
  deadlines and no request limits per connection.
- Pipelining: every complete request in the input buffer is answered, in order, and all responses
  leave in **one write**. A normal request/response exchange is one read and one write.
- `Content-Length` and chunked request bodies; `Expect: 100-continue`; bodies up to 256 MiB; a
  request line plus headers up to 16 KiB (431 beyond).
- Every response carries `Content-Length` (except 204/304). Never chunked, never compressed, no
  `Date`, no `Server`.
- A handler panic becomes `500 {"message":"Internal Server Error"}` with the stack on stderr; the
  connection and the process live on. A handler that returns without replying gets the same 500. A
  handler that replies twice panics (and so gets the 500): reply exactly once.
- Zero allocations per request in the engine itself (`BenchmarkEngineGet`: parse + route + auth +
  reply from cached bytes, about 0.4 us, 0 allocs).

### 4.2 The handler interface

```go
type Handler func(c *httpd.Ctx)
```

Request side. All byte slices are **views into the connection's input buffer**: valid until your
handler returns, never to be stored. Convert to `string` (which copies) before keeping anything.

| | |
|---|---|
| `c.Method` | `httpd.GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, `OTHER` |
| `c.Path`, `c.Query` | raw, undecoded; `Query` without the `?` |
| `c.Body` | complete body, already de-chunked; empty when there is none (DELETE can have one) |
| `c.Host` | Host header, validated (`[A-Za-z0-9.:_\[\]-]`), else the server's own address. Build absolute URLs as `"http://" + c.Host` |
| `c.Authorization`, `c.ContentType`, `c.Accept`, `c.GitProtocol` | trimmed header values, nil when absent |
| `c.Header("mcp-method")` | any other header; pass the name in lowercase |
| `c.Auth()` | `httpd.AuthNone` (no header), `AuthBad`, `AuthOK`. Accepts `Bearer <t>`, `token <t>` (scheme in any case) and Basic with the token as password |
| `c.Param(i)`, `c.ParamString(i)`, `c.ParamInt(i)` | i-th captured route parameter: raw / percent-decoded / digits only (anything else, or more than 18 digits: `ok == false`, which you answer with 404) |
| `c.QueryRaw(k)`, `c.QueryString(k)`, `c.QueryInt(k, def)` | query parameters: raw / decoded (`+` and `%XX`) / number |
| `httpd.Unescape(dst, src, plus)`, `httpd.ParseInt(b)` | the primitives behind those |

Response side. Call exactly one of these:

| | |
|---|---|
| `c.JSON(status, body)` | `Content-Type: application/json; charset=utf-8`, `X-GitHub-Request-Id`, `Content-Length`. `body` is copied, so a cached or constant slice is fine |
| `c.JSONTail(status, tail)` | same reply from a pre-rendered tail (`httpd.AppendTail(dst, body)` = `Content-Length` + blank line + body). Two appends, no rendering. This is how a cached issue is served |
| `c.Bytes(status, contentType, body)` | any content type, no request id (git) |
| `c.Prebuilt(response)` | a whole cached response, built with `httpd.AppendResponse(dst, status, contentType, body)` (git advertisements, cached fetch replies) |
| `c.Empty(status)` | no body. 204 has no `Content-Length`; other statuses say 0 (MCP's 202) |
| `c.AddHeader(name, value)`, `c.AddHeaderBytes` | an extra header for the reply that follows (`Link`, `X-OAuth-Scopes`, `Allow`, `WWW-Authenticate`). Not applied to `Prebuilt` |

`c.Scratch` is a per-connection buffer for building a body without allocating:

```go
b := c.Scratch[:0]
b = render.AppendRepo(b, c.Host, repo)
c.Scratch = b          // keep the grown capacity for the next request
c.JSON(200, b)
```

Never hand `c.Scratch` to something that keeps it, and never assign a slice you do not own (a
cached tail, a constant) to `c.Scratch`: the next request would overwrite it.

### 4.3 Router

`httpd.Router` matches a path against registered patterns segment by segment, without allocating.

```go
r.Handle(httpd.GET, "/repos/{owner}/{repo}/issues/{number}", a.getIssue)
r.Handle(httpd.PUT, "/repos/{owner}/{repo}/contents/{path...}", a.putContents)
h := r.Find(c, c.Method, path)   // nil: no such route, or no handler for this method
```

- `{name}` captures one non-empty segment; `{name...}` (last segment only) captures the rest of the
  path, slashes included, and needs at least one character. `/contents` and `/contents/` match the
  pattern `/contents`, not `/contents/{path...}`: register both.
- Literal segments beat parameters, parameters beat a catch-all, and the search backtracks:
  `/git/refs/heads/{branch...}` and `/git/refs/{ref...}` can coexist.
- Captures are raw bytes in pattern order: `c.Param(0)` is the owner above. Branch names and
  contents paths may arrive percent-encoded (`release%2F1.0`): use `c.ParamString(i)` for them.
  Repository names are `[A-Za-z0-9._-]` and need no decoding: pass `c.Param(i)` straight to
  `store.Lookup`.
- A route that exists for another method only is `nil`, i.e. a 404, as the REST spec wants.
- Registration panics on a malformed or duplicate pattern: that is a start-up bug, not a run-time
  condition.

### 4.4 Testing and benchmarking a surface

`httpd.NewTester(srv).Do(rawRequestBytes)` runs complete requests through the real parser, dispatch
and writer without a socket and returns the response bytes. Use it for handler tests
(`internal/app/app_test.go` shows the pattern) and for allocation benchmarks
(`internal/render/render_test.go`: `BenchmarkGetIssue` is the read workload's request end to end,
0 allocs/op; `BenchmarkPostIssue` is the write workload's). Run benchmarks with
`go test ./internal/<pkg>/ -run xxx -bench . -benchmem`.

---

## 5. Concurrency and determinism

### 5.1 One mutex

The store has a single `sync.Mutex`. `app.Serve` locks it before calling a surface and unlocks when
the surface returns, so:

- **surfaces and store methods never lock** (the mutex is not reentrant: locking again deadlocks);
- everything a surface reads is consistent, and every operation is atomic as seen by other requests;
- the lock is never held across a socket read or write;
- pointers the store returns (`*Repo`, `*Issue`, `*Pull`, `*Label`, `*Comment`, `*gitobj.Object`)
  are the **live objects**. Read them, never modify them, never keep them after the request.

With `GOMAXPROCS=1` the mutex is uncontended (about 20 ns). Concurrent clients (50 clones, MCP's two
calls in flight) are simply serialized; each handler runs for microseconds. It also makes lazily
built caches safe: the first of 50 identical requests builds, the other 49 hit.

Tests that call the store directly (no HTTP) need no locking; tests that go through `app.Serve`
must not hold the lock while they do.

### 5.2 Determinism

Two runs fed the same requests must produce the same state, so that the replayed trial matches the
live one. The rules, enforced in the store and expected from every surface:

- **No wall clock in data.** The store has a logical clock: `Epoch` is 2026-01-01T00:00:00Z
  (1767225600) and every mutating operation advances it by one second (`Store.Tick`). `created_at`,
  `updated_at`, `closed_at`, `merged_at`, `pushed_at` and the author/committer times of commits the
  server makes all come from it, `+0000`. Consequences: timestamps are strictly increasing in
  creation order, and the SHA of a commit made through the API depends only on the request sequence.
  Commits pushed over git keep whatever times the client wrote.
- **No map iteration in output.** Every list the store exposes is an ordered slice: repositories in
  creation order, refs sorted by name, labels, issues, pull requests and comments in creation order.
  If a surface needs a map for lookup, it must not range over it to produce output.
- **No random values.** Ids are counters, node ids are functions of ids, request ids are a counter.
- **No background work.** Nothing is computed on a timer or in another goroutine.

---

## 6. Identity, ids, numbers

- **Viewer.** The bearer token stands for one user, fixed in `store`: `ViewerLogin = "adaiversen"`,
  `ViewerID = 1000`, `ViewerName = "ada iversen"`, `ViewerEmail = "adaiversen@example.invalid"`. It
  owns every repository. `store.Lookup(owner, name)` returns nil for any other owner (compared
  without case), which surfaces answer with 404, never 403.
- **Numeric ids** (`ID`): one counter per object type for the whole process (repositories, issues,
  pull requests, comments, labels), starting at 1, assigned in creation order, never reused, not
  even after a delete. REST `id` and GraphQL `databaseId` are this number.
- **Node ids**: `<prefix>_<base64url, no padding, of "<TypeName>:<id>">` with `U`/`User`,
  `R`/`Repository`, `I`/`Issue`, `PR`/`PullRequest`, `IC`/`IssueComment`, `LA`/`Label`. Repository 17
  is `R_UmVwb3NpdG9yeToxNw`. `store.NodeID(kind, id)` and `store.AppendNodeID` build one,
  `store.ParseNodeID` decodes, `Store.Node(nodeID)` returns the live object (`*Repo`, `*Issue`,
  `*Pull`, `*Comment`, `*Label`, `store.Viewer{}` or nil). REST `node_id` and GraphQL `id` are the
  same string. A node id is never the bare numeric id or number.
- **Numbers.** Issues and pull requests share one sequence per repository, starting at 1.
  `Repo.Issue(n)` is nil for a pull request's number and `Repo.Pull(n)` is nil for an issue's.
  Comments attach to a number of either kind. A repository deleted and created again starts at 1
  with new ids.
- **Default branch.** Always `main` from creation, before any commit; `Repo.HeadRef()` is
  `refs/heads/main` whether the ref exists or not. A push that creates another branch first does not
  change it.

---

## 7. The store API (`internal/store`)

All of it assumes the store mutex is held. Errors are `*store.Error` (section 7.9) carrying the REST
status and message; a nil pointer return means "not found" where the signature has no error.

### 7.1 Store

| Function | Semantics |
|---|---|
| `store.New() *Store` | empty store, clock at `Epoch` |
| `s.Lock()`, `s.Unlock()` | the mutex; only `app.Serve` (and tests) call these |
| `s.Now() int64`, `s.Tick() int64` | read / advance the logical clock. Store operations tick themselves; the git surface ticks once per push |
| `store.IsViewer(owner []byte) bool` | owner is the viewer's login, case-insensitive |
| `s.CreateRepo(NewRepo{Name, Description, HasDescription, Private, NoIssues}) (*Repo, error)` | empty repository: no refs, no objects, HEAD -> unborn `main`. Name must be 1-100 of `[A-Za-z0-9._-]`, not `.`/`..`, not ending `.git`. Errors, all 422 with message `Repository creation failed.`: missing name (`missing_field`), invalid name (`custom`), and **name taken, compared without case: `custom`, detail `name already exists on this account`**, which leaves the existing repository untouched. `HasDescription=false` renders `"description": null` |
| `s.Repo(name string) *Repo`, `s.RepoB(name []byte) *Repo` | by name: exact match, then case-insensitive. `RepoB` does not allocate |
| `s.Lookup(owner, name []byte) *Repo` | the `{owner}/{repo}` of a URL: nil unless owner is the viewer and the repository exists |
| `s.Repos() []*Repo` | all repositories, creation order (the order of `GET /user/repos`); read-only slice |
| `s.UpdateRepo(r, RepoPatch{Name, Description, ClearDescription, Private, HasIssues, Archived, DefaultBranch}) error` | nil fields unchanged. Rename: valid unused name or 422; keeps id, git data, issues, pulls. New default branch must exist (422) |
| `s.DeleteRepo(r)` | removes the repository with refs, objects, labels, issues, comments, pulls. The name is free again |
| `s.RepoByID`, `s.IssueByID`, `s.PullByID`, `s.CommentByID`, `s.LabelByID` | by numeric id, nil when unknown or deleted |
| `s.Node(nodeID string) any` | by node id (section 6) |

### 7.2 Repo: fields

`ID`, `NodeID`, `Name`, `Description`+`HasDescription`, `Private`, `HasIssues`, `Archived`,
`DefaultBranch`, `CreatedAt`, `UpdatedAt`, `PushedAt`, `OpenIssues` (open issues + open pulls),
`Labels`, `Issues`, `Pulls` (ordered slices, read-only), `Objects *gitobj.DB`, `Gen` (bumped on every
ref change), `Git` (the git surface's response cache, emptied on every ref change), `JSONGen`
(bumped on rename; invalidates cached renderings). `r.FullName()` is `adaiversen/<name>`.

### 7.3 Refs and history

| Function | Semantics |
|---|---|
| `r.IsEmpty() bool` | no refs at all |
| `r.HeadRef() string`, `r.Head() *gitobj.Object` | `refs/heads/<default>`; the commit it resolves to, nil while unborn (GraphQL `defaultBranchRef` is null exactly then) |
| `r.Refs() []Ref`, `r.Branches() []Ref`, `r.Tags() []Ref` | sorted by name, **full names** (`refs/heads/x`); read-only slices |
| `r.Ref(full string) (OID, bool)`, `r.Branch(short string) (OID, bool)` | lookups; branch names may contain slashes |
| `r.CreateRef(full, id) error` | `POST /git/refs`. 422 `Reference name is invalid` (must be `refs/<kind>/<name>`, git-valid), 422 `Object does not exist` (unknown id, or a branch at a non-commit), 422 `Reference already exists` |
| `r.UpdateRef(full, id, force) error` | 422 `Reference does not exist`, 422 `Object does not exist`, 422 `Update is not a fast forward` unless `force` |
| `r.DeleteRef(full) error` | 422 `Reference does not exist` |
| `r.SetRef(full, id)`, `r.RemoveRef(full) bool` | unchecked primitives for the git surface (it does its own checks and calls `s.Tick()`); every ref change bumps `Gen` and empties `r.Git` |
| `store.ValidRefName(full) bool` | the name rule above |
| `r.Resolve(rev string) *gitobj.Object` | a commit from: `""` (default branch), a branch name, a tag name (peeled), `refs/...`, `heads/x`, `tags/x`, or 40 hex. nil when it names no commit |
| `r.Log(rev, offset, limit, dst) (commits, more, err)` | history of `rev`, newest committer time first; skips `offset`, appends at most `limit` (`<= 0`: all) to `dst`; `more` = commits remain. 409 `Git Repository is empty.` (no refs), 404 `No commit found for SHA: <rev>`. With `rev == ""` only the default branch's history: a commit made on another branch is not in it |

### 7.4 Contents

| Function | Semantics |
|---|---|
| `r.Contents(path, rev) (Content, error)` | file: `Content{Mode, ID (blob), Data}`; directory (`""` = root): `IsDir`, `ID` (tree), `Entries` sorted by name. `Commit` is the commit read. Errors, all 404: `This repository is empty.` (no refs), `No commit found for the ref <rev>`, `Not Found` (no such path) |
| `r.PutFile(FileWrite{Path, Message, Data, SHA, Branch, Author, Committer}) (FileResult, error)` | one commit that creates or updates one file. Branch empty = default. **Empty repository: root commit, the branch is created** (`Created`, REST 201). Branch missing in a repository with history: created from the default branch's tip. Path exists as a file: `SHA` required (422 `"sha" wasn't supplied`) and must equal the **current** blob id, case-insensitive (else **409 `<path> does not match <sha>`**); mode kept; `Created=false` (REST 200). Path absent: created with mode 100644, `SHA` ignored, `Created=true` (REST 201). 422 also for: empty message, invalid path (empty, leading/trailing slash, empty/`.`/`..`/`.git` segment), path is a directory, a path component is a file, invalid branch name. The message is stored byte for byte. `FileResult{Created, Branch, Path, Mode, Blob, Size, Commit}` |
| `r.DeleteFile(FileWrite{Path, Message, SHA, Branch}) (FileResult, error)` | one commit that removes one file; directories left empty are pruned; deleting the last file leaves the empty tree `4b825dc6...`. 422 missing message or SHA; 404 empty repository, missing branch, or path not a file; 409 SHA is not the current blob |

`Data` is the raw bytes: base64 decoding and encoding are the surface's business (decode after
stripping whitespace; an invalid encoding is the surface's 422). Author and committer default to the
viewer; the time is the logical clock.

### 7.5 Labels

| Function | Semantics |
|---|---|
| `r.Label(name) *Label` | exact, then case-insensitive |
| `r.CreateLabel(name, color, description, hasDescription) (*Label, error)` | 422 `Validation Failed`: `missing_field` name, `already_exists` name. Color: `#` stripped, lowercased, default `ededed` |
| `r.UpdateLabel(l, LabelPatch{Name, Color, Description}) error`, `r.DeleteLabel(l)` | issues carry the label object, so they follow; their cached JSON is dropped |

### 7.6 Issues and comments

| Function | Semantics |
|---|---|
| `r.CreateIssue(NewIssue{Title, Body, Labels}) (*Issue, error)` | open issue, next shared number, new id, body `""` when absent, strings stored byte for byte. 422 `Validation Failed`: empty title (`Issue`/`title`/`missing_field`), **unknown label (`Label`/`labels`/`invalid`, `value` = the name): labels are never created here**. 410 when issues are disabled |
| `r.Issue(number) *Issue` | nil when absent or the number is a pull request. O(1) |
| `r.ListIssues(IssueQuery{State, Labels, Sort, Asc, Offset, Limit}, dst) (page, total)` | appends the page to `dst`, returns the count before paging. `State`: `StateOpen` (zero value, the REST default), `StateClosed`, `StateAll` (`store.ParseState`). `Sort`: `SortCreated` (= number order, never timestamps), `SortUpdated`, `SortComments`, ties by number in the sort's direction. `Asc=false` is newest first (REST default); **GraphQL's default order is `Asc: true`**. Pull requests are never included. `Limit <= 0`: no limit |
| `r.UpdateIssue(is, IssuePatch{Title, Body, State, StateReason, Labels}) error` | nil fields unchanged. Close: `Closed`, `ClosedAt` = now, `StateReason` `completed` (or the given one); closing a closed issue changes nothing but `UpdatedAt`. Reopen: `ClosedAt` 0, `reopened`. 422: empty title, state not `open`/`closed`, unknown label |
| `r.AddComment(number, body) (*Comment, error)` | on an issue **or a pull request**. 404 unknown number, 422 empty body (`IssueComment`/`body`/`missing_field`) |
| `r.Comments(number) ([]*Comment, bool)` | creation order; `ok=false` for an unknown number |

`Issue` fields: `ID`, `Number`, `Title`, `Body`, `Closed`, `StateReason`, `Labels`, `Comments`
(the slice; its length is the comment count), `CreatedAt`, `UpdatedAt`, `ClosedAt` (0 = open), `Repo`.

### 7.7 Pull requests

| Function | Semantics |
|---|---|
| `r.CreatePull(NewPull{Title, Body, Head, Base, Draft}) (*Pull, error)` | `Head` may be `branch` or `owner:branch` (prefix dropped). Both branches must exist as `refs/heads/*`. 422 `Validation Failed`, resource `PullRequest`: `missing_field` title/head/base; `invalid` head (missing branch, or head == base); `invalid` base; `custom` `A pull request already exists for adaiversen:<head>.` when an **open** one has the same head and base. "No commits between" is not an error. Next shared number; body `""` when absent |
| `r.Pull(number) *Pull` | nil when absent or the number is an issue |
| `r.ListPulls(PullQuery{State, Head, Base, Sort, Asc, Offset, Limit}, dst) (page, total)` | default open only; `StateClosed` includes merged. `Sort`: created (number) or updated. For GraphQL's `MERGED` / `CLOSED` distinction filter `r.Pulls` yourself (it is in number order) |
| `r.UpdatePull(p, PullPatch{Title, Body, State}) error` | edit, close, reopen. 422: empty title, bad state, reopening a merged pull request |
| `r.PullInfo(p) *PullInfo` | everything that depends on git state, computed from the **current** refs and cached until a tip moves: `Head`, `Base` (branch tips; frozen once closed or merged, `Base` = the tip merged onto), `MergeBase`, `Mergeable` (`MergeClean`, `MergeConflicting`, `MergeUnknown` for closed/merged), `Files []FileChange{Path, Status ("added"/"removed"/"modified"), Additions, Deletions, Blob}` = merge base -> head, sorted by path, `Commits`, `Additions`, `Deletions`. Read-only |
| `r.MergePull(p, MergeOptions{Method, CommitTitle, CommitMessage, SHA}) (*gitobj.Object, error)` | decided **now, against the current base tip**, by a three-way tree merge (path level, no line merging). `squash` and `rebase`: one commit, parent = base tip, tree = merge result, message `<title> (#<n>)` plus `CommitMessage` or a `* subject` list. `merge` (the default when `Method` is empty): parents `[base tip, head tip]`. The base ref moves; the pull request becomes `Closed`, `Merged`, `MergedAt = ClosedAt = now`, `MergeCommit` set; the head branch stays. Errors: **405 `Pull Request is not mergeable`** (not open, a branch gone, or conflict: nothing is written), 409 `Head branch was modified...` (`SHA` given and not the head tip), 422 unknown method. Returns the new commit; the REST reply is `render.AppendMergeResult` |

`Pull` fields: `ID`, `Number`, `Title`, `Body`, `HeadRef`, `BaseRef` (short names), `Draft`,
`Closed`, `Merged`, `Head`, `Base`, `MergeCommit`, `Comments`, `CreatedAt`, `UpdatedAt`, `ClosedAt`,
`MergedAt`, `Repo`. For the live head/base sha always go through `r.PullInfo(p)`.

### 7.8 The git layer (`internal/gitobj`)

Shared by the store and the git surface, so objects made through the API and objects pushed over
git are the same bytes in the same database.

| | |
|---|---|
| `OID` | 20-byte id; `String`, `AppendHex`, `ParseOID`, `ParseOIDBytes`, `Zero`, `EmptyTree` |
| `DB` | per repository. `Get`, `Has`, `GetCommit`, `Put(type, data)` (hash, store, dedupe), `Add(id, type, data, z)` (pack ingestion, id already verified), `ID(type, data)`, `Deflated(o)` (zlib stream, built once), `Peel(id)` (through tags) |
| `Object` | `ID`, `Type`, `Data` (exact bytes, never re-encoded), `Z`, `Commit` (`Tree`, `Parents`, `Time`; parsed at insert) |
| encodings | `AppendCommit`, `ParseCommit`, `ParseTag`, `AppendTree`, `ParseTree`, `SortEntries` (git order: a directory sorts as `name/`), modes `ModeFile` 100644, `ModeDir` 40000, ... |
| trees | `db.Tree(id)`, `db.PutTree(entries)`, `db.Find(root, path)`, `db.SetPath(root, path, mode, id)`, `db.RemovePath(root, path)` (prunes empty directories), `ValidPath` |
| history | `db.IsAncestor(a, b)` (O(1) when `a` is a parent of `b`), `db.MergeBase(a, b)`, `db.Walk(tip, fn)` (newest first), `db.CountAhead(tip, base)` |
| merge, diff | `db.Merge3(base, ours, theirs, write)`, `db.DiffTrees(a, b) []Change`, `db.ChangeStats(c)`, `LineStats(old, new)` |

Object bytes live in an append-only arena and objects in slabs, so storing a pushed object does not
allocate (`BenchmarkPutBlob`, `BenchmarkIsAncestorParent`: 0 allocs/op).

### 7.9 Errors

```go
type Error struct {
    Status  int     // 404, 405, 409, 410, 422
    Message string  // REST "message"
    Resource, Field, Code, Value, Detail string  // one entry of REST "errors", when Code != ""
    DocURL  string
}
```

- REST: `rest.Fail(c, err)` replies `err.Status` with `render.AppendError`, which produces
  `{"message":..., "errors":[{resource, field, code, value, message}], "documentation_url":...}`.
- MCP: an `isError: true` result whose text is the same `render.AppendError` bytes.
- GraphQL: map `Status` to its error type (404 -> `NOT_FOUND`, 422/405/409 -> `UNPROCESSABLE`) with
  `err.Error()` as the message.
- `store.ErrNotFound` is the plain 404. `store.StatusOf(err)` reads the status of any error.
- Fixed texts are constants: `MsgNotMergeable`, `MsgEmptyRepo`, `MsgEmptyGit`, `MsgHeadModified`,
  `MsgMerged`.

Do not re-validate in a surface what the store validates, and do not invent a different message for
the same failure: call the store and report its error.

---

## 8. Rendering (`internal/render`)

One renderer per object type. REST handlers, MCP tools and anything else that shows a REST object
call the same function with the same host, so the object is byte for byte the same everywhere.

| Renderer | Object |
|---|---|
| `UserJSON`, `AppendViewer(dst, host)` | the viewer inside other objects (constant) / `GET /user` |
| `AppendRepo(dst, host, r)` | repository |
| `AppendLabel`, `AppendLabels` | label, label array |
| `IssueTail(host, is)`, `AppendIssue`, `AppendIssues` | issue (cached), issue array |
| `AppendComment`, `AppendComments` | issue comment |
| `AppendPull(dst, host, p)`, `AppendPulls` | pull request (calls `Repo.PullInfo`) |
| `AppendPullFiles(dst, host, r, info, files)` | pull request files |
| `AppendMergeResult(dst, commitID)` | `{"sha", "merged": true, "message"}` |
| `AppendBranch`, `AppendGitRef`, `AppendCommit` | branch, git ref, commit (list item form) |
| `AppendContent(dst, host, r, path, &content)` | contents: file object (base64, unwrapped) or directory array |
| `AppendFileResult(dst, host, r, &res, deleted)` | contents write reply; its `content` carries `"encoding":"base64"` |
| `AppendError(dst, e)`, `AppendMessage(dst, msg)` | REST error bodies |
| `AppendString(dst, s)`, `AppendTime(dst, t)` | JSON string escaping; `2006-01-02T15:04:05Z` |

Rules the renderers keep, and that any new renderer must keep:

- hand-written `append` code in a fixed key order; no `encoding/json`, no maps, no `fmt`;
- arrays are never `null` (`[]`), required keys are never omitted, `body` of an issue or pull
  request is always a string, ids and counts are JSON numbers, SHAs are 40 lowercase hex;
- strings go through `AppendString` only: it escapes `"`, `\` and control characters, and leaves
  `<`, `>`, `&` and all valid UTF-8 alone, so text round-trips byte for byte;
- absolute URLs are `"http://" + host`, where `host` is `c.Host` of the current request.

**The issue cache.** `render.IssueTail` renders an issue once into a reply tail and stores it on the
issue; `c.JSONTail(200, render.IssueTail(c.Host, is))` then serves it with two appends. The store
drops the tail whenever the issue changes (edit, close, reopen, comment, label change) and a
repository rename invalidates all of them (`JSONGen`); a different `Host` re-renders. Nothing else
needs to know: always go through `IssueTail` / `AppendIssue`, never cache issue JSON yourself.

If a surface needs to wrap a rendered object (MCP's `structuredContent`), embed the rendered bytes
as they are (`json.RawMessage`); do not decode and re-encode them.

---

## 9. Rules for every surface

1. **Do not lock.** `app.Serve` holds the store mutex around your `Serve`.
2. **Authenticate first**, before reading the body: `c.Auth()`. REST: 401 `Requires authentication`
   (no header) / `Bad credentials` (wrong token), for every route including unknown ones. GraphQL
   and MCP have their own 401 bodies (already in the stubs).
3. **One store operation per request**, then render. No business rule lives in a surface: if two
   surfaces need the same behaviour it belongs in the store.
4. **Request bytes die with the request.** Anything you keep must be a copy (`string(b)`;
   `encoding/json` copies into `string` fields by itself).
5. **Reply exactly once**, through `httpd`. Never write to a socket, never start a goroutine, never
   sleep, never log per request (each stderr line is a system call the benchmark pays for).
6. **Never 500.** Check every lookup for nil and every parameter for `ok`; a panic is caught, but it
   is a failed check.
7. **Deterministic output** (section 5.2): no `time.Now`, no ranging over maps for output, no
   random ids.
8. **Standard library only**, and not `net/http`.
9. **Hot paths do not allocate.** `GET .../issues/{n}` must stay at 0 allocs/op end to end and
   `POST .../issues` at a handful (the stored strings, the issue, its rendered tail). On these two
   routes: no `fmt`, no `strconv.Itoa`, no string concatenation, no `map[string]any`. Prove it with
   a benchmark next to your tests.
10. **Tests go through the real engine** (`httpd.NewTester`) so status, headers and body are checked
    together.

### REST (`internal/rest`)

`rest.go` is the frame: `Serve` (auth, router, 404), `routes()` where every route is registered,
`Fail` (store error -> reply), `Decode` (JSON object body or 400 `Problems parsing JSON`),
`a.Repo(c, i)` (owner/repo parameters -> `*store.Repo`, or 404 and nil), `NotFound`. `Message`
replies with a bare message, `DecodeOptional` is `Decode` for routes whose body may be absent.
Every route is registered in `routes()`; `HEAD` is answered by the `GET` handler.

Handlers live in `repos.go`, `git.go`, `contents.go`, `issues.go`, `pulls.go`; pagination and the
`Link` header in `page.go` (`per_page` default 30, clamped 1..100; `page` default 1; `rel="next"`
and `rel="last"` built from `"http://" + c.Host` and the request's own query with only `page`
replaced); body field types and the issue-creation scanner in `decode.go`. Statuses: 201 on create
(also for `PutFile` when `res.Created`), 200 on update, 204 on repository delete.

### GraphQL (`internal/graphql`)

`graphql.go` has the transport gate (POST only, 401 bodies). Resolve fields straight from the store
objects (section 7 lists every field) and node ids through `Store.Node`; mutations call the same
store operations REST calls (`CreateIssue`, `UpdateIssue` with state `closed`, `AddComment`,
`CreatePull`, `MergePull` with method `squash` for `SQUASH`). `defaultBranchRef` is null exactly
when `r.Head() == nil`. `mergeable` comes from `r.PullInfo(p).Mergeable`, `files` from
`r.PullInfo(p).Files`. Timestamps: `render.AppendTime`. `encoding/json` is fine here (not timed), but
keep response keys in selection order.

### MCP (`internal/mcp`)

`mcp.go` has the method gate (405 + `Allow`) and the auth gate (401 before anything else). Tools
call the store and embed the `render` bytes: `create_repo` -> `CreateRepo` + `AppendRepo`, `get_repo`
-> `Lookup` + `AppendRepo`, `create_issue` -> `CreateIssue` + `AppendIssue`, `list_issues` ->
`ListIssues` + `AppendIssues`, `create_pull_request` -> `CreatePull` + `AppendPull`,
`merge_pull_request` -> `MergePull` (method `squash`) + `AppendMergeResult`. Store errors become
`isError: true` results with the `render.AppendError` text. Use `c.Host` for the renderers so the
URLs match what REST returns to the same client.

### Git (`internal/githttp`)

Files: `githttp.go` (routing, auth), `advert.go` (advertisements), `receive.go` (push), `pack.go`
(pack reader with deltas, pack writer), `upload.go` (ls-refs, fetch, object selection), `pktline.go`.

| Request | Answer |
|---|---|
| `GET info/refs?service=git-upload-pack` with `Git-Protocol: version=2` | one constant response for every repository: `version 2`, `ls-refs=unborn`, `fetch=shallow` (no `agent`, no `object-format`: a client that sees them must echo them in every command) |
| same without the header | protocol v0 ref advertisement (`# service=git-upload-pack`), cached per ref state |
| `GET info/refs?service=git-receive-pack` | always v0: every ref sorted by name, capabilities `report-status delete-refs side-band-64k ofs-delta` on the first line, or on `0{40} capabilities^{}` for an empty repository. The whole HTTP response is rendered once per ref state (`Repo.Git.AdvReceive`) |
| `POST git-upload-pack`, `command=ls-refs` | HEAD first (`symref-target:` when asked, `unborn HEAD ...` for an unborn default branch when asked), refs sorted, `peeled:` for annotated tags, `ref-prefix` filtered on the server (HEAD included) |
| `POST git-upload-pack`, `command=fetch` | `[acknowledgments]` only without `done`; `[shallow-info]` only when deepening; `packfile` in band-1 packets of at most 65520 bytes; always at least the 32-byte empty pack. Whole, undeltified objects, each once |
| `POST git-upload-pack` starting with `want` | a basic protocol v0 fetch (shallow list, one ACK/NAK, pack with or without side-band) for old clients |
| `POST git-receive-pack` | always HTTP 200 with a report: `unpack ok` or `unpack <error>`, then `ok <ref>` / `ng <ref> <reason>` per update in request order, inside band 1 when the client asked for `side-band-64k` |

Push rules. The pack is verified (trailer SHA-1, every zlib stream and its Adler-32, declared sizes,
entry count), deltas are resolved against the pack or the store (thin packs), and every new
object's references must exist, in the pack or already stored; otherwise the pack's objects are
taken out again and the updates that needed them are refused with `missing necessary objects`.
Updates are independent: create (`old` zero) needs the ref to be absent, update needs `old` to be
the current value and, for `refs/heads/*`, the current commit to be an ancestor of the new one
(`non-fast-forward` otherwise), delete removes. A refused update still leaves `unpack ok` and the
objects stored. Nothing is derived from history: the cost of a push is the cost of its pack.

Fetch selection follows git's upload-pack (`selectObjects` in `upload.go` documents it): reachable
from the wants, minus reachable from the haves, where commits the client holds as shallow and the
new boundary of a `deepen` / `deepen-relative` / `deepen-since` / `deepen-not` request are cut points
no walk continues past, and unshallowed commits contribute their parents as wants.

Caching. `Repo.Git.Upload` maps the exact request body to the whole HTTP response for `ls-refs` and
for fetches without `have` or `shallow` lines (the clone shape), at most 16 entries of at most 4 MiB.
Every ref change (`refsChanged`, also from REST writes and merges) empties it and truncates the two
advertisement buffers; the caches hang off the `*Repo`, so a repository deleted and created again
starts clean. The store mutex makes cache fills single-flight: of fifty identical concurrent clones
one builds the pack. Pushed objects keep the zlib stream they arrived in (`Object.Z`) and it goes
out verbatim; objects made by the server get a stored-block stream on first use (`DB.Deflated`).

Store functions for this surface (`store/gitsurface.go`): `Repo.RefB`, `SetRefB`, `RemoveRefB` take
the ref name as bytes from the request body, so an update of an existing ref allocates nothing.
`gitobj.NextEntry` iterates a tree without allocating; `DB.Remove` exists only for the rollback
above. The surface calls `Store.Tick` once per push.

Measured on the development machine (`go test ./internal/githttp/ -run xxx -bench . -benchmem`):
one push of the push workload (advertisement GET + POST with a 3-object pack) 16 us, 0 allocs;
one clone of the clone workload (5 requests) 3.1 us, 0 allocs. `TestRealGit` runs the git command
line client against the surface (clone, shallow clone, deepen, unshallow, pull, delta pushes,
refused pushes, protocol v0) wherever a git binary is installed.

---

## 10. Building and grading

```
go vet ./... && go test ./...
bash build.sh        # both binaries; stages bin/forge-linux-amd64 with the exec bit; no commit
```

`build.sh` and `grade-local` must run while holding the shared lock described in the brief
(`kompetisi8/.grade-lock`); compiling and testing your own package needs no lock. The local grader
runs a copy of the checkout made from git's file list, so new files must at least be untracked and
not ignored, and the Windows build is found through `~/.cache/forge/` (written by `build.sh`).
A local grade needs a fresh process each time (it always gets one): repository names are fixed.

`build.sh` builds with `CGO_ENABLED=0 -trimpath -buildvcs=false -tags netgo,osusergo
-ldflags "-s -w -buildid="`: no libc, so the Linux binary is static (the script refuses a binary
that names a dynamic loader) and byte-for-byte reproducible from the sources.
