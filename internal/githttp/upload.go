package githttp

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
)

const (
	maxCachedResponses = 16      // per repository and ref state
	maxCachedResponse  = 4 << 20 // bytes; larger responses are rebuilt each time
	maxGzipRequest     = 64 << 20
)

// uploadPack is POST git-upload-pack. Protocol v2 requests start with "command=ls-refs" or
// "command=fetch"; a body that starts with "want" is a protocol v0 fetch; anything else is a 400.
//
// Identical requests against an unchanged repository get the stored response: the key is the
// exact request body, the cache lives on the repository object and every ref change empties it.
func (s *Server) uploadPack(c *httpd.Ctx, r *store.Repo) {
	body := c.Body
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		// git compresses large requests (Content-Encoding: gzip). A pkt-line never starts like this.
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			c.JSON(400, badRequest)
			return
		}
		plain, err := io.ReadAll(io.LimitReader(zr, maxGzipRequest))
		if err != nil {
			c.JSON(400, badRequest)
			return
		}
		body = plain
	}
	if response, ok := r.Git.Upload[string(body)]; ok {
		c.Prebuilt(response)
		return
	}
	p := pktReader{buf: body}
	first, kind := p.next()
	if kind != pktData {
		c.JSON(400, badRequest)
		return
	}
	first = chomp(first)
	switch {
	case string(first) == "command=ls-refs":
		if !skipCapabilities(&p) {
			c.JSON(400, badRequest)
			return
		}
		s.lsRefs(c, r, &p, body)
	case string(first) == "command=fetch":
		if !skipCapabilities(&p) {
			c.JSON(400, badRequest)
			return
		}
		s.fetchV2(c, r, &p, body)
	case bytes.HasPrefix(first, []byte("want ")):
		s.fetchV0(c, r, body)
	default:
		c.JSON(400, badRequest)
	}
}

// skipCapabilities reads the capability lines of a v2 command (agent=, object-format=, ...) up to
// the delimiter that starts the arguments. A flush instead means the command has no arguments; the
// reader is put back so the argument loop sees that flush.
func skipCapabilities(p *pktReader) bool {
	for {
		mark := p.pos
		_, kind := p.next()
		switch kind {
		case pktData:
		case pktDelim:
			return true
		case pktFlush:
			p.pos = mark
			return true
		default:
			return false
		}
	}
}

// reply sends an upload-pack response and, when cache is set and there is room, keeps it for the
// next identical request.
func (s *Server) reply(c *httpd.Ctx, r *store.Repo, request, body []byte, cache bool) {
	if cache && len(r.Git.Upload) < maxCachedResponses && len(body) <= maxCachedResponse {
		response := httpd.AppendResponse(make([]byte, 0, len(body)+128), 200, typeUploadResult, body)
		if r.Git.Upload == nil {
			r.Git.Upload = make(map[string][]byte, maxCachedResponses)
		}
		r.Git.Upload[string(request)] = response
		c.Prebuilt(response)
		return
	}
	c.Bytes(200, typeUploadResult, body)
}

// lsRefs answers "command=ls-refs": HEAD first, then every ref sorted by name, one pkt-line each:
//
//	<oid> <name>[ symref-target:<ref>][ peeled:<oid>]
//
// ref-prefix arguments filter on the server, HEAD included. symref-target appears on HEAD when the
// client sent "symrefs", peeled on refs that point at an annotated tag when it sent "peel". While
// the default branch does not exist yet, HEAD is "unborn HEAD symref-target:<ref>" if the client
// sent "unborn", and is left out otherwise. A zero id is never sent.
func (s *Server) lsRefs(c *httpd.Ctx, r *store.Repo, p *pktReader, request []byte) {
	var peel, symrefs, unborn bool
	prefixes := s.fetch.prefixes[:0]
	for {
		line, kind := p.next()
		if kind == pktFlush || kind == pktEOF {
			break
		}
		if kind != pktData {
			c.JSON(400, badRequest)
			return
		}
		line = chomp(line)
		switch string(line) {
		case "peel":
			peel = true
		case "symrefs":
			symrefs = true
		case "unborn":
			unborn = true
		default:
			if prefix, ok := cutPrefix(line, "ref-prefix "); ok {
				prefixes = append(prefixes, prefix)
			}
		}
	}
	s.fetch.prefixes = prefixes
	wanted := func(name string) bool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, string(prefix)) {
				return true
			}
		}
		return len(prefixes) == 0
	}

	b := s.body[:0]
	if wanted("HEAD") {
		var mark int
		if id, ok := r.Ref(r.HeadRef()); ok {
			b, mark = beginPkt(b)
			b = id.AppendHex(b)
			b = append(b, " HEAD"...)
			if symrefs {
				b = append(b, " symref-target:"...)
				b = append(b, r.HeadRef()...)
			}
			b = append(b, '\n')
			b = endPkt(b, mark)
		} else if unborn {
			b, mark = beginPkt(b)
			b = append(b, "unborn HEAD symref-target:"...)
			b = append(b, r.HeadRef()...)
			b = append(b, '\n')
			b = endPkt(b, mark)
		}
	}
	for _, ref := range r.Refs() {
		if !wanted(ref.Name) {
			continue
		}
		var mark int
		b, mark = beginPkt(b)
		b = ref.ID.AppendHex(b)
		b = append(b, ' ')
		b = append(b, ref.Name...)
		if peel {
			if o := r.Objects.Get(ref.ID); o != nil && o.Type == gitobj.TypeTag {
				if peeled := r.Objects.Peel(o.ID); peeled != nil {
					b = append(b, " peeled:"...)
					b = peeled.ID.AppendHex(b)
				}
			}
		}
		b = append(b, '\n')
		b = endPkt(b, mark)
	}
	b = append(b, flushPkt...)
	s.body = b
	s.reply(c, r, request, b, true)
}

// fetchRequest is a decoded fetch, v2 or v0.
type fetchRequest struct {
	wants    []gitobj.OID
	haves    []gitobj.OID
	shallows []gitobj.OID // commits the client holds without their parents
	depth    int          // "deepen N"; 0 = no depth limit
	relative bool         // "deepen-relative": count the depth from the client's shallow commits
	since    int64        // "deepen-since <time>"; 0 = none
	not      []string     // "deepen-not <ref>"
	done     bool
	sideband bool // v0 only: the client asked for side-band framing
}

// fetchState is the scratch space of object selection, reused between fetches.
type fetchState struct {
	req      fetchRequest
	prefixes [][]byte

	shallow     map[gitobj.OID]struct{} // req.shallows as a set
	cut         map[gitobj.OID]struct{} // commits whose parents no walk may follow
	boundarySet map[gitobj.OID]struct{}
	excluded    map[gitobj.OID]struct{} // reachable from a deepen-not ref
	haveCommits map[gitobj.OID]struct{}
	haveObjects map[gitobj.OID]struct{}
	sent        map[gitobj.OID]struct{}
	depth       map[gitobj.OID]int // window commits with their depth

	stack     []*gitobj.Object
	starts    []*gitobj.Object // commits the wants lead to
	tags      []*gitobj.Object // annotated tags among the wants
	loose     []*gitobj.Object // trees and blobs wanted through a tag
	window    []*gitobj.Object
	objects   []*gitobj.Object // what goes into the pack, in pack order
	boundary  []gitobj.OID     // "shallow" lines
	unshallow []gitobj.OID     // "unshallow" lines
}

func (f *fetchState) reset() {
	if f.shallow == nil {
		f.shallow = map[gitobj.OID]struct{}{}
		f.cut = map[gitobj.OID]struct{}{}
		f.boundarySet = map[gitobj.OID]struct{}{}
		f.excluded = map[gitobj.OID]struct{}{}
		f.haveCommits = map[gitobj.OID]struct{}{}
		f.haveObjects = map[gitobj.OID]struct{}{}
		f.sent = map[gitobj.OID]struct{}{}
		f.depth = map[gitobj.OID]int{}
	}
	clear(f.shallow)
	clear(f.cut)
	clear(f.boundarySet)
	clear(f.excluded)
	clear(f.haveCommits)
	clear(f.haveObjects)
	clear(f.sent)
	clear(f.depth)
	f.stack, f.starts, f.tags, f.loose = f.stack[:0], f.starts[:0], f.tags[:0], f.loose[:0]
	f.window, f.objects = f.window[:0], f.objects[:0]
	f.boundary, f.unshallow = f.boundary[:0], f.unshallow[:0]
}

// haveTree records a tree the client has, with everything below it.
func (f *fetchState) haveTree(db *gitobj.DB, id gitobj.OID) {
	if _, seen := f.haveObjects[id]; seen {
		return
	}
	f.haveObjects[id] = struct{}{}
	o := db.Get(id)
	if o == nil || o.Type != gitobj.TypeTree {
		return
	}
	for data := o.Data; len(data) > 0; {
		mode, _, child, rest, ok := gitobj.NextEntry(data)
		if !ok {
			return
		}
		switch mode {
		case gitobj.ModeDir:
			f.haveTree(db, child)
		case gitobj.ModeGitlink:
		default:
			f.haveObjects[child] = struct{}{}
		}
		data = rest
	}
}

// send adds one object to the pack unless the client has it or it is already in.
func (f *fetchState) send(o *gitobj.Object) bool {
	if _, has := f.haveObjects[o.ID]; has {
		return false
	}
	if _, in := f.sent[o.ID]; in {
		return false
	}
	f.sent[o.ID] = struct{}{}
	f.objects = append(f.objects, o)
	return true
}

// sendTree adds a tree and everything below it. A subtree the client has, or that is already in
// the pack, is skipped whole: its contents are covered by the same fact.
func (f *fetchState) sendTree(db *gitobj.DB, id gitobj.OID) {
	o := db.Get(id)
	if o == nil || !f.send(o) || o.Type != gitobj.TypeTree {
		return
	}
	for data := o.Data; len(data) > 0; {
		mode, _, child, rest, ok := gitobj.NextEntry(data)
		if !ok {
			return
		}
		switch mode {
		case gitobj.ModeDir:
			f.sendTree(db, child)
		case gitobj.ModeGitlink:
		default:
			if blob := db.Get(child); blob != nil {
				f.send(blob)
			}
		}
		data = rest
	}
}

// selectObjects decides what a fetch sends, the way git's upload-pack does.
//
// Without any shallow business it is simply: every object reachable from the wants (annotated tags
// included and peeled) that is not reachable from the haves.
//
// A depth limit and a shallow client add three ideas:
//
//   - The window: the commits the client is allowed to end up with. "deepen N" walks N commits deep
//     from the wants; "deepen-relative" walks N+1 deep from the client's current shallow commits
//     instead; "deepen-since" and "deepen-not" take the commits newer than a time, or not reachable
//     from a ref. A window commit with a parent outside the window is a boundary.
//   - Cut points: boundary commits and every commit the client holds as shallow. A walk includes a
//     cut point but never continues to its parents, because the client does not (and will not)
//     have them. Both the "client has" walk and the "to send" walk obey this.
//   - Unshallowing: a commit the client held as shallow that now lies inside the window, not on its
//     boundary, gets its parents: they join the wants.
//
// The reply names the new boundary commits ("shallow") and the unshallowed ones ("unshallow"); the
// pack holds the commits found from the wants that the client lacks, with their trees and blobs,
// minus whatever the client has, each object once.
//
// It reports false when a want (or a deepen-not ref) names nothing in the repository.
func (f *fetchState) selectObjects(r *store.Repo) bool {
	db := r.Objects
	req := &f.req
	f.reset()
	for _, id := range req.shallows {
		f.shallow[id] = struct{}{}
		f.cut[id] = struct{}{}
	}

	// The wants: tag objects are sent, the commits they lead to are where the walks start.
	for _, id := range req.wants {
		o := db.Get(id)
		if o == nil {
			return false
		}
		for o.Type == gitobj.TypeTag {
			f.tags = append(f.tags, o)
			target, _, _, ok := gitobj.ParseTag(o.Data)
			if !ok {
				break
			}
			if o = db.Get(target); o == nil {
				break
			}
		}
		switch {
		case o == nil || o.Type == gitobj.TypeTag:
		case o.Commit != nil:
			f.starts = append(f.starts, o)
		default:
			f.loose = append(f.loose, o) // a tag of a tree or blob
		}
	}

	// The window and its boundary.
	switch {
	case req.depth > 0:
		limit := req.depth
		if req.relative {
			// Count from where the client's history currently ends.
			limit++
			for _, id := range req.shallows {
				if c := db.GetCommit(id); c != nil {
					f.enter(c, 1)
				}
			}
		} else {
			for _, c := range f.starts {
				f.enter(c, 1)
			}
		}
		// Breadth first, so every commit is reached at its smallest depth.
		for i := 0; i < len(f.window); i++ {
			c := f.window[i]
			d := f.depth[c.ID]
			if d >= limit {
				f.markBoundary(c.ID)
				continue
			}
			for _, parent := range c.Commit.Parents {
				if po := db.GetCommit(parent); po != nil {
					f.enter(po, d+1)
				}
			}
		}
	case req.since > 0 || len(req.not) > 0:
		for _, rev := range req.not {
			c := r.Resolve(rev)
			if c == nil {
				return false
			}
			f.stack = append(f.stack, c)
		}
		for len(f.stack) > 0 { // everything reachable from the excluded refs
			c := f.stack[len(f.stack)-1]
			f.stack = f.stack[:len(f.stack)-1]
			if _, seen := f.excluded[c.ID]; seen {
				continue
			}
			f.excluded[c.ID] = struct{}{}
			for _, parent := range c.Commit.Parents {
				if po := db.GetCommit(parent); po != nil {
					f.stack = append(f.stack, po)
				}
			}
		}
		inside := func(c *gitobj.Object) bool {
			_, out := f.excluded[c.ID]
			return !out && c.Commit.Time >= req.since
		}
		for _, c := range f.starts {
			if inside(c) {
				f.enter(c, 1)
			}
		}
		for i := 0; i < len(f.window); i++ {
			c := f.window[i]
			for _, parent := range c.Commit.Parents {
				po := db.GetCommit(parent)
				switch {
				case po == nil:
				case inside(po):
					f.enter(po, 1)
				default:
					f.markBoundary(c.ID)
				}
			}
		}
		if len(f.window) == 0 {
			return false // the limits select no commit at all
		}
		kept := f.starts[:0]
		for _, c := range f.starts {
			if _, in := f.depth[c.ID]; in {
				kept = append(kept, c)
			}
		}
		f.starts = kept
	}
	// Unshallow what the window now reaches past; its parents become wanted.
	if len(f.window) > 0 {
		for _, id := range req.shallows {
			if _, in := f.depth[id]; !in {
				continue
			}
			if _, edge := f.boundarySet[id]; edge {
				continue
			}
			f.unshallow = append(f.unshallow, id)
			for _, parent := range db.GetCommit(id).Commit.Parents {
				if po := db.GetCommit(parent); po != nil {
					f.starts = append(f.starts, po)
				}
			}
		}
	}

	// What the client has: its haves and what they reach, stopping at cut points.
	for _, id := range req.haves {
		o := db.Get(id)
		for o != nil && o.Type == gitobj.TypeTag {
			f.haveObjects[o.ID] = struct{}{}
			o = db.Peel(o.ID)
		}
		switch {
		case o == nil:
		case o.Commit != nil:
			f.stack = append(f.stack, o)
		default:
			f.haveTree(db, o.ID)
		}
	}
	for len(f.stack) > 0 {
		c := f.stack[len(f.stack)-1]
		f.stack = f.stack[:len(f.stack)-1]
		if _, seen := f.haveCommits[c.ID]; seen {
			continue
		}
		f.haveCommits[c.ID] = struct{}{}
		f.haveObjects[c.ID] = struct{}{}
		f.haveTree(db, c.Commit.Tree)
		if _, stop := f.cut[c.ID]; stop {
			continue
		}
		for _, parent := range c.Commit.Parents {
			if po := db.GetCommit(parent); po != nil {
				f.stack = append(f.stack, po)
			}
		}
	}

	// What to send: tags, then commits from the wants down to what the client has or a cut point,
	// then the trees and blobs of those commits.
	for _, o := range f.tags {
		f.send(o)
	}
	first := len(f.objects)
	for i := len(f.starts) - 1; i >= 0; i-- {
		f.stack = append(f.stack, f.starts[i])
	}
	for len(f.stack) > 0 {
		c := f.stack[len(f.stack)-1]
		f.stack = f.stack[:len(f.stack)-1]
		if _, has := f.haveCommits[c.ID]; has {
			continue
		}
		if !f.send(c) {
			continue
		}
		if _, stop := f.cut[c.ID]; stop {
			continue
		}
		for _, parent := range c.Commit.Parents {
			if po := db.GetCommit(parent); po != nil {
				f.stack = append(f.stack, po)
			}
		}
	}
	for _, c := range f.objects[first:len(f.objects):len(f.objects)] {
		f.sendTree(db, c.Commit.Tree)
	}
	for _, o := range f.loose {
		f.sendTree(db, o.ID)
	}
	return true
}

// enter puts a commit into the window at the given depth unless it is already there.
func (f *fetchState) enter(c *gitobj.Object, depth int) {
	if _, seen := f.depth[c.ID]; !seen {
		f.depth[c.ID] = depth
		f.window = append(f.window, c)
	}
}

// markBoundary records a window commit whose parents stay behind. It becomes a cut point, and a
// "shallow" line unless the client already holds it as shallow.
func (f *fetchState) markBoundary(id gitobj.OID) {
	if _, seen := f.boundarySet[id]; seen {
		return
	}
	f.boundarySet[id] = struct{}{}
	f.cut[id] = struct{}{}
	if _, already := f.shallow[id]; !already {
		f.boundary = append(f.boundary, id)
	}
}

// deepening reports whether the request limits history, which is what makes a reply carry shallow
// information.
func (req *fetchRequest) deepening() bool {
	return req.depth > 0 || req.since > 0 || len(req.not) > 0
}

// parseFetchArg handles the argument lines v2 and v0 share. ok is false for a malformed line or a
// feature this server does not implement.
func (req *fetchRequest) parseFetchArg(line []byte) (ok bool) {
	if hex, is := cutPrefix(line, "want "); is {
		if len(hex) < 40 {
			return false
		}
		id, valid := gitobj.ParseOIDBytes(hex[:40])
		if !valid {
			return false
		}
		for _, have := range req.wants {
			if have == id {
				return true
			}
		}
		req.wants = append(req.wants, id)
		return true
	}
	if hex, is := cutPrefix(line, "have "); is {
		id, valid := gitobj.ParseOIDBytes(hex)
		if valid {
			req.haves = append(req.haves, id)
		}
		return valid
	}
	if hex, is := cutPrefix(line, "shallow "); is {
		id, valid := gitobj.ParseOIDBytes(hex)
		if valid {
			req.shallows = append(req.shallows, id)
		}
		return valid
	}
	if n, is := cutPrefix(line, "deepen "); is {
		depth, valid := httpd.ParseInt(n)
		if !valid || depth < 1 || depth > 1<<31 { // git asks for 2^31-1 to mean "everything"
			return false
		}
		req.depth = int(depth)
		return true
	}
	if t, is := cutPrefix(line, "deepen-since "); is {
		since, valid := httpd.ParseInt(t)
		req.since = since
		return valid && since > 0
	}
	if rev, is := cutPrefix(line, "deepen-not "); is {
		req.not = append(req.not, string(rev))
		return len(rev) > 0
	}
	switch string(line) {
	case "done":
		req.done = true
		return true
	case "deepen-relative":
		req.relative = true
		return true
	case "thin-pack", "no-progress", "ofs-delta", "include-tag":
		return true // accepted; this server sends whole objects, no progress and no extra tags
	}
	// filter, want-ref, sideband-all, ...: not advertised, not served.
	return false
}

func (f *fetchState) resetRequest() *fetchRequest {
	req := &f.req
	req.wants, req.haves, req.shallows = req.wants[:0], req.haves[:0], req.shallows[:0]
	req.not = req.not[:0]
	req.depth, req.relative, req.since, req.done, req.sideband = 0, false, 0, false, false
	return req
}

// appendShallowLines appends the "shallow" and "unshallow" pkt-lines of a deepening fetch.
func (f *fetchState) appendShallowLines(b []byte) []byte {
	for _, id := range f.boundary {
		var mark int
		b, mark = beginPkt(b)
		b = append(b, "shallow "...)
		b = id.AppendHex(b)
		b = append(b, '\n')
		b = endPkt(b, mark)
	}
	for _, id := range f.unshallow {
		var mark int
		b, mark = beginPkt(b)
		b = append(b, "unshallow "...)
		b = id.AppendHex(b)
		b = append(b, '\n')
		b = endPkt(b, mark)
	}
	return b
}

// fetchV2 answers "command=fetch". The response is a sequence of sections:
//
//	[acknowledgments]  only when the client has not said "done": the haves we share, or NAK
//	[shallow-info]     only when the request had "deepen"
//	packfile           the pack in band-1 side-band packets, at least one, then a flush
//
// A fetch that has nothing to send still gets a valid, empty pack.
func (s *Server) fetchV2(c *httpd.Ctx, r *store.Repo, p *pktReader, request []byte) {
	f := &s.fetch
	req := f.resetRequest()
	for {
		line, kind := p.next()
		if kind == pktFlush || kind == pktEOF {
			break
		}
		if kind != pktData || !req.parseFetchArg(chomp(line)) {
			c.JSON(400, badRequest)
			return
		}
	}
	db := r.Objects
	b := s.body[:0]
	if !req.done {
		// A negotiation round. Acknowledge what we share; once anything is shared we are ready
		// and the pack follows in the same response.
		b = appendPkt(b, "acknowledgments\n")
		common := false
		for _, id := range req.haves {
			if db.Has(id) {
				var mark int
				b, mark = beginPkt(b)
				b = append(b, "ACK "...)
				b = id.AppendHex(b)
				b = append(b, '\n')
				b = endPkt(b, mark)
				common = true
			}
		}
		if !common {
			b = appendPkt(b, "NAK\n")
			b = append(b, flushPkt...)
			s.body = b
			s.reply(c, r, request, b, false)
			return
		}
		b = appendPkt(b, "ready\n")
		b = append(b, delimPkt...)
	}
	if !f.selectObjects(r) {
		c.JSON(400, badRequest)
		return
	}
	if req.deepening() {
		b = appendPkt(b, "shallow-info\n")
		b = f.appendShallowLines(b)
		b = append(b, delimPkt...)
	}
	b = appendPkt(b, "packfile\n")
	s.pack = appendPack(s.pack[:0], db, f.objects)
	b = appendSideband(b, s.pack)
	b = append(b, flushPkt...)
	s.body = b
	// Only the clone shape is worth keeping: no haves and no shallow lines, so the answer depends
	// on nothing but the refs.
	s.reply(c, r, request, b, req.done && len(req.haves) == 0 && len(req.shallows) == 0)
}

// fetchV0 answers a protocol v0 fetch (clients older than git 2.26, or configured for v0):
//
//	want <oid> [capabilities] ... [shallow <oid> ...] [deepen <n>] flush [have <oid> ...] [done]
//
// The reply is the shallow lines and a flush (when deepening), then one ACK or NAK, then, if the
// client said "done", the pack: in side-band packets when asked for, raw otherwise. Without "done"
// the reply ends after the ACK or NAK and the client sends its next round. A request that ends
// right after the wants (a deepening client asking for the shallow list first) gets the shallow
// lines and nothing else.
func (s *Server) fetchV0(c *httpd.Ctx, r *store.Repo, request []byte) {
	f := &s.fetch
	req := f.resetRequest()
	p := pktReader{buf: request}
	wantsOnly := false
	for {
		line, kind := p.next()
		if kind == pktEOF {
			break
		}
		if kind == pktFlush {
			// The flush that ends the wants; if the body ends here too, there are no haves yet.
			wantsOnly = p.pos == len(p.buf) && len(req.haves) == 0
			continue
		}
		if kind != pktData {
			c.JSON(400, badRequest)
			return
		}
		line = chomp(line)
		if len(req.wants) == 0 && len(line) > 45 {
			caps := line[45:] // after "want <40 hex> "
			req.sideband = hasCap(caps, "side-band-64k") || hasCap(caps, "side-band")
		}
		if !req.parseFetchArg(line) {
			c.JSON(400, badRequest)
			return
		}
	}
	db := r.Objects
	if len(req.wants) == 0 || !f.selectObjects(r) {
		c.JSON(400, badRequest)
		return
	}
	b := s.body[:0]
	if req.deepening() {
		b = f.appendShallowLines(b)
		b = append(b, flushPkt...)
	}
	if wantsOnly && !req.done {
		s.body = b
		s.reply(c, r, request, b, false)
		return
	}
	acked := false
	for i := len(req.haves) - 1; i >= 0 && !acked; i-- {
		if db.Has(req.haves[i]) {
			var mark int
			b, mark = beginPkt(b)
			b = append(b, "ACK "...)
			b = req.haves[i].AppendHex(b)
			b = append(b, '\n')
			b = endPkt(b, mark)
			acked = true
		}
	}
	if !acked {
		b = appendPkt(b, "NAK\n")
	}
	if req.done {
		s.pack = appendPack(s.pack[:0], db, f.objects)
		if req.sideband {
			b = appendSideband(b, s.pack)
			b = append(b, flushPkt...)
		} else {
			b = append(b, s.pack...)
		}
	}
	s.body = b
	s.reply(c, r, request, b, false)
}
