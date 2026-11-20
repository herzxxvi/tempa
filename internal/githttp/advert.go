package githttp

import (
	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
)

// advertV2 is the whole response to GET info/refs?service=git-upload-pack under protocol v2. It
// does not depend on the repository, so it is built once.
//
//   - no "# service=" preamble: v2 advertisements do not have one;
//   - ls-refs=unborn: an empty repository can tell a client what its default branch will be;
//   - fetch=shallow: depth-limited fetches are supported; filters and the other optional fetch
//     features are not, so they are not advertised.
//
// agent and object-format are left out: both are optional, a client that sees them must echo them
// in every command, and an absent object-format means sha1.
var advertV2 = func() []byte {
	var b []byte
	b = appendPkt(b, "version 2\n")
	b = appendPkt(b, "ls-refs=unborn\n")
	b = appendPkt(b, "fetch=shallow\n")
	b = append(b, flushPkt...)
	return httpd.AppendResponse(nil, 200, typeUploadAdvert, b)
}()

const (
	// receiveCaps: report-status (the per-ref result), delete-refs, side-band-64k (the report
	// travels in band 1) and ofs-delta (packs may use offset deltas; thin packs are accepted too).
	receiveCaps = "report-status delete-refs side-band-64k ofs-delta"
	// uploadV0Caps is what the basic protocol v0 fetch implements.
	uploadV0Caps = "side-band-64k side-band thin-pack ofs-delta shallow no-progress"

	zeroHex = "0000000000000000000000000000000000000000"
)

// appendRefLine appends one "<oid> <name>" advertisement line; caps, when not empty, follow a NUL.
func appendRefLine(b []byte, id gitobj.OID, name, suffix, caps, symref string) []byte {
	b, mark := beginPkt(b)
	b = id.AppendHex(b)
	b = append(b, ' ')
	b = append(b, name...)
	b = append(b, suffix...)
	if caps != "" {
		b = append(b, 0)
		b = append(b, caps...)
		if symref != "" {
			b = append(b, " symref=HEAD:"...)
			b = append(b, symref...)
		}
	}
	b = append(b, '\n')
	return endPkt(b, mark)
}

// appendNoRefs appends the line an empty repository advertises so that it can still state its
// capabilities: the zero id and the pseudo-ref "capabilities^{}".
func appendNoRefs(b []byte, caps string) []byte {
	b, mark := beginPkt(b)
	b = append(b, zeroHex+" capabilities^{}\x00"...)
	b = append(b, caps...)
	b = append(b, '\n')
	return endPkt(b, mark)
}

// advertReceive returns the whole response to GET info/refs?service=git-receive-pack: every ref,
// sorted by name, capabilities on the first line. It is rendered once per ref state and kept on the
// repository; a push invalidates it.
func (s *Server) advertReceive(r *store.Repo) []byte {
	if len(r.Git.AdvReceive) > 0 {
		return r.Git.AdvReceive
	}
	b := append(s.body[:0], "001f# service=git-receive-pack\n0000"...)
	refs := r.Refs()
	if len(refs) == 0 {
		b = appendNoRefs(b, receiveCaps)
	}
	caps := receiveCaps
	for i := range refs {
		b = appendRefLine(b, refs[i].ID, refs[i].Name, "", caps, "")
		caps = ""
	}
	b = append(b, flushPkt...)
	s.body = b
	r.Git.AdvReceive = httpd.AppendResponse(r.Git.AdvReceive[:0], 200, typeReceiveAdvert, b)
	return r.Git.AdvReceive
}

// advertUploadV0 returns the whole response to GET info/refs?service=git-upload-pack for a client
// that did not ask for protocol v2: HEAD, then every ref, annotated tags followed by their peeled
// "^{}" line.
func (s *Server) advertUploadV0(r *store.Repo) []byte {
	if len(r.Git.AdvUploadV0) > 0 {
		return r.Git.AdvUploadV0
	}
	b := append(s.body[:0], "001e# service=git-upload-pack\n0000"...)
	caps := uploadV0Caps
	if head := r.Head(); head != nil {
		b = appendRefLine(b, head.ID, "HEAD", "", caps, r.HeadRef())
		caps = ""
	}
	refs := r.Refs()
	for i := range refs {
		b = appendRefLine(b, refs[i].ID, refs[i].Name, "", caps, "")
		caps = ""
		if o := r.Objects.Get(refs[i].ID); o != nil && o.Type == gitobj.TypeTag {
			if peeled := r.Objects.Peel(o.ID); peeled != nil {
				b = appendRefLine(b, peeled.ID, refs[i].Name, "^{}", "", "")
			}
		}
	}
	if caps != "" {
		b = appendNoRefs(b, caps)
	}
	b = append(b, flushPkt...)
	s.body = b
	r.Git.AdvUploadV0 = httpd.AppendResponse(r.Git.AdvUploadV0[:0], 200, typeUploadAdvert, b)
	return r.Git.AdvUploadV0
}
