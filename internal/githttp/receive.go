package githttp

import (
	"bytes"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
)

// command is one ref update of a push: "<old> <new> <ref>".
type command struct {
	old, new gitobj.OID
	ref      []byte // view into the request body
	refusal  string // why the update was refused; empty = applied
}

// hasCap reports whether a space-separated capability list contains name.
func hasCap(caps []byte, name string) bool {
	for len(caps) > 0 {
		word := caps
		if i := bytes.IndexByte(caps, ' '); i >= 0 {
			word, caps = caps[:i], caps[i+1:]
		} else {
			caps = nil
		}
		if string(word) == name {
			return true
		}
	}
	return false
}

// receivePack is POST git-receive-pack: a list of ref updates, then (unless every update is a
// deletion) a pack with the objects the server lacks.
//
// The reply is always HTTP 200 with a report: "unpack ok" (or "unpack <error>"), then one "ok <ref>"
// or "ng <ref> <reason>" per update, in request order. Updates are independent: a refused one does
// not stop the others, and a refused update still leaves the pack's objects stored. When the client
// asked for side-band-64k, the report travels inside band-1 packets.
func (s *Server) receivePack(c *httpd.Ctx, r *store.Repo) {
	p := pktReader{buf: c.Body}
	s.cmds = s.cmds[:0]
	sideband, wantReport := false, false
	for {
		line, kind := p.next()
		if kind == pktFlush {
			break
		}
		if kind != pktData {
			c.JSON(400, badRequest)
			return
		}
		line = chomp(line)
		if _, ok := cutPrefix(line, "shallow "); ok {
			continue // sent by a client pushing from a shallow clone; nothing to do with it
		}
		var caps []byte
		if nul := bytes.IndexByte(line, 0); nul >= 0 {
			line, caps = line[:nul], line[nul+1:]
		}
		if len(s.cmds) == 0 {
			sideband = hasCap(caps, "side-band-64k")
			wantReport = hasCap(caps, "report-status") || hasCap(caps, "report-status-v2")
		}
		// "<40 hex> <40 hex> <ref>"
		if len(line) < 83 || line[40] != ' ' || line[81] != ' ' {
			c.JSON(400, badRequest)
			return
		}
		oldID, ok1 := gitobj.ParseOIDBytes(line[:40])
		newID, ok2 := gitobj.ParseOIDBytes(line[41:81])
		if !ok1 || !ok2 {
			c.JSON(400, badRequest)
			return
		}
		s.cmds = append(s.cmds, command{old: oldID, new: newID, ref: line[82:]})
	}

	db := r.Objects
	unpackError := ""
	if pack := p.rest(); len(pack) > 0 {
		if err := s.up.unpack(db, pack); err != nil {
			s.up.rollback(db)
			unpackError = err.Error()
		} else if !connected(db, s.up.added) {
			// The pack names objects nobody has. Take its objects out again: the updates that
			// needed them are then refused below, one by one, for missing objects.
			s.up.rollback(db)
		}
	}
	if unpackError == "" && len(s.cmds) > 0 {
		s.st.Tick()
		for i := range s.cmds {
			s.cmds[i].refusal = applyCommand(r, &s.cmds[i])
		}
	}

	// The report-status stream, built first on its own because side-band wraps it whole.
	inner := s.pack[:0]
	if unpackError == "" {
		inner = appendPkt(inner, "unpack ok\n")
	} else {
		var mark int
		inner, mark = beginPkt(inner)
		inner = append(inner, "unpack "...)
		inner = append(inner, unpackError...)
		inner = append(inner, '\n')
		inner = endPkt(inner, mark)
	}
	for i := range s.cmds {
		cmd := &s.cmds[i]
		reason := cmd.refusal
		if unpackError != "" {
			reason = "unpacker error"
		}
		var mark int
		inner, mark = beginPkt(inner)
		if reason == "" {
			inner = append(inner, "ok "...)
			inner = append(inner, cmd.ref...)
		} else {
			inner = append(inner, "ng "...)
			inner = append(inner, cmd.ref...)
			inner = append(inner, ' ')
			inner = append(inner, reason...)
		}
		inner = append(inner, '\n')
		inner = endPkt(inner, mark)
	}
	inner = append(inner, flushPkt...)
	s.pack = inner

	b := s.body[:0]
	switch {
	case !wantReport:
		if sideband {
			b = append(b, flushPkt...)
		}
	case sideband:
		b = appendSideband(b, inner)
		b = append(b, flushPkt...)
	default:
		b = append(b, inner...)
	}
	s.body = b
	c.Bytes(200, typeReceiveResult, b)
}

// connected checks the objects a pack added: everything a new commit, tree or tag names must be
// stored, either from before or from the same pack. Objects that were already stored passed this
// test when they arrived, so the whole database stays closed under references and the check costs
// what the pack costs, not what the history costs.
func connected(db *gitobj.DB, added []*gitobj.Object) bool {
	for _, o := range added {
		switch o.Type {
		case gitobj.TypeCommit:
			if o.Commit == nil || !db.Has(o.Commit.Tree) {
				return false
			}
			for _, parent := range o.Commit.Parents {
				if !db.Has(parent) {
					return false
				}
			}
		case gitobj.TypeTree:
			for data := o.Data; len(data) > 0; {
				mode, _, id, rest, ok := gitobj.NextEntry(data)
				if !ok || (mode != gitobj.ModeGitlink && !db.Has(id)) {
					return false
				}
				data = rest
			}
		case gitobj.TypeTag:
			if target, _, _, ok := gitobj.ParseTag(o.Data); !ok || !db.Has(target) {
				return false
			}
		}
	}
	return true
}

// applyCommand performs one ref update and returns "" or the reason it was refused.
//
//   - delete (new is zero): the ref goes away; deleting a ref that does not exist is not an error;
//   - create (old is zero): the ref must not exist yet (re-creating it at the same id is fine);
//   - update: old must be the ref's current value, and a branch only moves forward: the current
//     commit must be an ancestor of the new one, else "non-fast-forward".
func applyCommand(r *store.Repo, cmd *command) string {
	current, exists := r.RefB(cmd.ref)
	if cmd.new.IsZero() {
		if !exists {
			return ""
		}
		if !cmd.old.IsZero() && current != cmd.old {
			return "stale info"
		}
		r.RemoveRefB(cmd.ref)
		return ""
	}
	target := r.Objects.Get(cmd.new)
	if target == nil {
		return "missing necessary objects"
	}
	branch := len(cmd.ref) > 11 && string(cmd.ref[:11]) == "refs/heads/"
	if branch && target.Commit == nil {
		return "branch tip is not a commit"
	}
	if !exists {
		if !cmd.old.IsZero() {
			return "stale info"
		}
		if !store.ValidRefName(string(cmd.ref)) {
			return "funny refname"
		}
		r.SetRefB(cmd.ref, cmd.new)
		return ""
	}
	if current == cmd.new {
		return ""
	}
	if cmd.old.IsZero() {
		return "already exists"
	}
	if current != cmd.old {
		return "stale info"
	}
	if branch && !r.Objects.IsAncestor(current, cmd.new) {
		return "non-fast-forward"
	}
	r.SetRefB(cmd.ref, cmd.new)
	return ""
}
