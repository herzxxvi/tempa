package githttp

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
)

// The tests drive the surface the way a git client does: real pkt-lines and real packs through the
// real HTTP engine (httpd.Tester), with a small client written here that checks every byte it gets
// back. The scenarios follow the conformance suite's git checks.

const token = "tok-0123456789abcdef"

// forge is a store with the git surface in front of it, dispatched like internal/app does.
type forge struct {
	t      testing.TB
	st     *store.Store
	git    *Server
	tester *httpd.Tester
}

func newForge(t testing.TB) *forge {
	f := &forge{t: t, st: store.New()}
	f.git = New(f.st)
	f.tester = httpd.NewTester(&httpd.Server{Token: token, Handler: f.dispatch})
	return f
}

func (f *forge) dispatch(c *httpd.Ctx) {
	// "/{owner}/{repo}.git{sub}"
	path := c.Path[1:]
	slash := bytes.IndexByte(path, '/')
	dot := bytes.Index(path, []byte(".git"))
	if slash < 0 || dot < slash {
		c.JSON(404, notFound)
		return
	}
	f.st.Lock()
	defer f.st.Unlock()
	f.git.Serve(c, path[:slash], path[slash+1:dot], path[dot+4:])
}

func (f *forge) create(name string) *store.Repo {
	f.t.Helper()
	r, err := f.st.CreateRepo(store.NewRepo{Name: name, Private: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

type response struct {
	status      int
	contentType string
	body        []byte
}

func (f *forge) do(method, path string, headers string, body []byte) response {
	f.t.Helper()
	req := method + " " + path + " HTTP/1.1\r\nhost: 127.0.0.1:4000\r\nauthorization: Bearer " + token + "\r\n" + headers
	if body != nil {
		req += "content-length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	raw := f.tester.Do(append([]byte(req+"\r\n"), body...))
	head, payload, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		f.t.Fatalf("%s %s: malformed response %q", method, path, raw)
	}
	res := response{body: bytes.Clone(payload)}
	lines := strings.Split(string(head), "\r\n")
	res.status, _ = strconv.Atoi(lines[0][9:12])
	length := -1
	for _, line := range lines[1:] {
		name, value, _ := strings.Cut(line, ": ")
		switch strings.ToLower(name) {
		case "content-type":
			res.contentType = value
		case "content-length":
			length, _ = strconv.Atoi(value)
		}
	}
	if length != len(payload) {
		f.t.Fatalf("%s %s: Content-Length %d but %d body bytes", method, path, length, len(payload))
	}
	return res
}

// ---- building objects the way a client does ----

type object struct {
	typ  gitobj.Type
	data []byte
	id   gitobj.OID
}

func mk(typ gitobj.Type, data []byte) object {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return object{typ, data, gitobj.OID(h.Sum(nil))}
}

func blob(content string) object { return mk(gitobj.TypeBlob, []byte(content)) }

// treeOf builds the trees for a set of files (paths may have directories) and returns every tree
// and blob, root tree first.
func treeOf(files map[string]string) []object {
	type dir struct {
		files map[string]string
		dirs  map[string]*dir
	}
	root := &dir{map[string]string{}, map[string]*dir{}}
	for path, content := range files {
		d := root
		parts := strings.Split(path, "/")
		for _, p := range parts[:len(parts)-1] {
			if d.dirs[p] == nil {
				d.dirs[p] = &dir{map[string]string{}, map[string]*dir{}}
			}
			d = d.dirs[p]
		}
		d.files[parts[len(parts)-1]] = content
	}
	var build func(d *dir) []object
	build = func(d *dir) []object {
		var entries []gitobj.Entry
		var below []object
		for name, content := range d.files {
			b := blob(content)
			below = append(below, b)
			entries = append(entries, gitobj.Entry{Mode: gitobj.ModeFile, Name: name, ID: b.id})
		}
		for name, sub := range d.dirs {
			objs := build(sub)
			below = append(below, objs...)
			entries = append(entries, gitobj.Entry{Mode: gitobj.ModeDir, Name: name, ID: objs[0].id})
		}
		gitobj.SortEntries(entries)
		return append([]object{mk(gitobj.TypeTree, gitobj.AppendTree(nil, entries))}, below...)
	}
	return build(root)
}

// commitOf returns a commit of files on top of parents, with everything it needs: commit first.
func commitOf(n int, files map[string]string, parents ...object) []object {
	objs := treeOf(files)
	who := gitobj.Signature{Name: "ada iversen", Email: "adaiversen@example.invalid", When: int64(1700000000 + n)}
	c := gitobj.Commit{Tree: objs[0].id, Author: who, Committer: who, Message: "commit " + strconv.Itoa(n) + "\n"}
	for _, p := range parents {
		c.Parents = append(c.Parents, p.id)
	}
	return append([]object{mk(gitobj.TypeCommit, gitobj.AppendCommit(nil, &c))}, objs...)
}

func tagOf(name string, target object) object {
	return mk(gitobj.TypeTag, []byte("object "+target.id.String()+"\ntype commit\ntag "+name+
		"\ntagger ada iversen <adaiversen@example.invalid> 1700000000 +0000\n\nrelease "+name+"\n"))
}

func entryHeader(typ int, size int) []byte {
	b := byte(typ)<<4 | byte(size&15)
	var out []byte
	for size >>= 4; size > 0; size >>= 7 {
		out = append(out, b|0x80)
		b = byte(size & 0x7f)
	}
	return append(out, b)
}

func deflate(data []byte) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(data)
	w.Close()
	return z.Bytes()
}

func finishPack(count int, entries []byte) []byte {
	pack := append([]byte("PACK\x00\x00\x00\x02"), binary.BigEndian.AppendUint32(nil, uint32(count))...)
	pack = append(pack, entries...)
	sum := sha1.Sum(pack)
	return append(pack, sum[:]...)
}

// packOf builds a pack of whole objects, zlib-compressed like a client's.
func packOf(objs ...object) []byte {
	var entries []byte
	for _, o := range objs {
		entries = append(entries, entryHeader(int(o.typ), len(o.data))...)
		entries = append(entries, deflate(o.data)...)
	}
	return finishPack(len(objs), entries)
}

// ---- the client's side of the protocol ----

type update struct {
	old, new gitobj.OID
	ref      string
}

func pkt(payload string) string { return fmt.Sprintf("%04x%s", len(payload)+4, payload) }

func pushBody(updates []update, caps string, pack []byte) []byte {
	var b []byte
	for i, u := range updates {
		line := u.old.String() + " " + u.new.String() + " " + u.ref
		if i == 0 {
			line += "\x00" + caps
		}
		b = append(b, pkt(line+"\n")...)
	}
	b = append(b, "0000"...)
	return append(b, pack...)
}

// pktLines splits a pkt-line stream; flush and delimiter appear as "0000" and "0001".
func pktLines(t testing.TB, data []byte) []string {
	t.Helper()
	var out []string
	p := pktReader{buf: data}
	for {
		payload, kind := p.next()
		switch kind {
		case pktEOF:
			return out
		case pktData:
			out = append(out, string(payload))
		case pktFlush:
			out = append(out, "0000")
		case pktDelim:
			out = append(out, "0001")
		default:
			t.Fatalf("malformed pkt-line stream at %d: %q", p.pos, data)
		}
	}
}

// push sends updates with the given objects and returns the report lines ("unpack ok",
// "ok refs/heads/main", "ng refs/heads/x reason").
func (f *forge) push(repo string, updates []update, objs ...object) []string {
	f.t.Helper()
	var pack []byte
	for _, u := range updates {
		if !u.new.IsZero() {
			pack = packOf(objs...)
			break
		}
	}
	return f.pushRaw(repo, pushBody(updates, "report-status side-band-64k ofs-delta", pack))
}

func (f *forge) pushRaw(repo string, body []byte) []string {
	f.t.Helper()
	res := f.do("POST", "/adaiversen/"+repo+".git/git-receive-pack", "content-type: application/x-git-receive-pack-request\r\n", body)
	if res.status != 200 || res.contentType != typeReceiveResult {
		f.t.Fatalf("push: %d %s %q", res.status, res.contentType, res.body)
	}
	// Outer stream: band-1 packets then a flush. Inner stream: the report's own pkt-lines.
	var inner []byte
	outer := pktLines(f.t, res.body)
	if len(outer) == 0 || outer[len(outer)-1] != "0000" {
		f.t.Fatalf("push response does not end with a flush: %q", res.body)
	}
	for _, packet := range outer[:len(outer)-1] {
		if packet[0] != 1 {
			f.t.Fatalf("push response packet in band %d", packet[0])
		}
		inner = append(inner, packet[1:]...)
	}
	lines := pktLines(f.t, inner)
	if lines[len(lines)-1] != "0000" {
		f.t.Fatalf("report does not end with a flush: %q", inner)
	}
	var out []string
	for _, l := range lines[:len(lines)-1] {
		if !strings.HasSuffix(l, "\n") {
			f.t.Fatalf("report line without newline: %q", l)
		}
		out = append(out, strings.TrimSuffix(l, "\n"))
	}
	return out
}

func (f *forge) mustPush(repo string, updates []update, objs ...object) {
	f.t.Helper()
	report := f.push(repo, updates, objs...)
	if len(report) != len(updates)+1 || report[0] != "unpack ok" {
		f.t.Fatalf("push report: %q", report)
	}
	for i, u := range updates {
		if report[i+1] != "ok "+u.ref {
			f.t.Fatalf("push report: %q", report)
		}
	}
}

func (f *forge) uploadPackPost(repo string, body string) response {
	f.t.Helper()
	return f.do("POST", "/adaiversen/"+repo+".git/git-upload-pack", "git-protocol: version=2\r\ncontent-type: application/x-git-upload-pack-request\r\n", []byte(body))
}

// lsRefs runs ls-refs the way the suite's client does and returns the ref lines.
func (f *forge) lsRefs(repo string, prefixes ...string) []string {
	f.t.Helper()
	body := pkt("command=ls-refs\n") + "0001" + pkt("peel\n") + pkt("symrefs\n") + pkt("unborn\n")
	for _, p := range prefixes {
		body += pkt("ref-prefix " + p + "\n")
	}
	res := f.uploadPackPost(repo, body+"0000")
	if res.status != 200 || res.contentType != typeUploadResult {
		f.t.Fatalf("ls-refs: %d %s %q", res.status, res.contentType, res.body)
	}
	lines := pktLines(f.t, res.body)
	if lines[len(lines)-1] != "0000" {
		f.t.Fatalf("ls-refs does not end with a flush: %q", res.body)
	}
	var out []string
	for _, l := range lines[:len(lines)-1] {
		out = append(out, strings.TrimSuffix(l, "\n"))
	}
	return out
}

type fetched struct {
	shallow   []string // lines of the shallow-info section
	objects   map[gitobj.OID]object
	packets   int // side-band packets in the packfile section
	maxPacket int // largest pkt-line of the response, header included
	pack      []byte
}

// fetch runs a v2 fetch and verifies the response's framing and the pack inside it.
func (f *forge) fetch(repo string, wants, haves, shallows []gitobj.OID, depth int) fetched {
	f.t.Helper()
	body := pkt("command=fetch\n") + "0001" + pkt("thin-pack\n") + pkt("no-progress\n") + pkt("ofs-delta\n")
	for _, id := range wants {
		body += pkt("want " + id.String() + "\n")
	}
	for _, id := range haves {
		body += pkt("have " + id.String() + "\n")
	}
	for _, id := range shallows {
		body += pkt("shallow " + id.String() + "\n")
	}
	if depth > 0 {
		body += pkt("deepen " + strconv.Itoa(depth) + "\n")
	}
	res := f.uploadPackPost(repo, body+pkt("done\n")+"0000")
	if res.status != 200 || res.contentType != typeUploadResult {
		f.t.Fatalf("fetch: %d %s %q", res.status, res.contentType, res.body)
	}
	return f.parseFetch(res.body, depth > 0)
}

func (f *forge) parseFetch(body []byte, deepen bool) fetched {
	f.t.Helper()
	out := fetched{objects: map[gitobj.OID]object{}}
	p := pktReader{buf: body}
	next := func() (string, int) {
		before := p.pos
		payload, kind := p.next()
		if kind == pktError || kind == pktEnd {
			f.t.Fatalf("malformed fetch response at %d", before)
		}
		out.maxPacket = max(out.maxPacket, p.pos-before)
		return string(payload), kind
	}
	section, kind := next()
	if deepen {
		if section != "shallow-info\n" {
			f.t.Fatalf("expected shallow-info, got %q", section)
		}
		for {
			line, kind := next()
			if kind == pktDelim {
				break
			}
			if kind != pktData {
				f.t.Fatalf("shallow-info ended with kind %d", kind)
			}
			out.shallow = append(out.shallow, strings.TrimSuffix(line, "\n"))
		}
		section, kind = next()
	}
	if kind != pktData || section != "packfile\n" {
		f.t.Fatalf("expected packfile, got %q (kind %d)", section, kind)
	}
	for {
		packet, kind := next()
		if kind == pktFlush {
			break
		}
		if kind != pktData || len(packet) < 2 || packet[0] != 1 {
			f.t.Fatalf("bad packet in packfile section: kind %d, %q", kind, packet[:min(len(packet), 20)])
		}
		out.packets++
		out.pack = append(out.pack, packet[1:]...)
	}
	if _, kind := p.next(); kind != pktEOF {
		f.t.Fatal("bytes after the final flush")
	}
	if out.packets == 0 {
		f.t.Fatal("no pack in the packfile section")
	}
	// Verify the pack independently: unpack it into a fresh database.
	db := gitobj.NewDB(&gitobj.Arena{})
	var u unpacker
	if err := u.unpack(db, out.pack); err != nil {
		f.t.Fatalf("the served pack does not parse: %v", err)
	}
	count := int(binary.BigEndian.Uint32(out.pack[8:]))
	if count != len(u.added) {
		f.t.Fatalf("pack header says %d objects, %d distinct objects inside", count, len(u.added))
	}
	for _, o := range u.added {
		out.objects[o.ID] = object{o.Type, o.Data, o.ID}
	}
	return out
}

func (f *fetched) has(objs ...object) bool {
	for _, o := range objs {
		got, ok := f.objects[o.id]
		if !ok || got.typ != o.typ || !bytes.Equal(got.data, o.data) {
			return false
		}
	}
	return true
}

func (f *fetched) hasAny(objs ...object) bool {
	for _, o := range objs {
		if _, ok := f.objects[o.id]; ok {
			return true
		}
	}
	return false
}

var zero = gitobj.Zero

// seed creates a repository and pushes C1 = one commit of files to main.
func (f *forge) seed(name string, files map[string]string) []object {
	f.t.Helper()
	f.create(name)
	c1 := commitOf(1, files)
	f.mustPush(name, []update{{zero, c1[0].id, "refs/heads/main"}}, c1...)
	return c1
}

// ---- advertisements ----

func TestAdvertisements(t *testing.T) {
	f := newForge(t)
	f.create("empty")

	// Protocol v2: the same constant bytes for every repository, no service preamble.
	v2 := f.do("GET", "/adaiversen/empty.git/info/refs?service=git-upload-pack", "git-protocol: version=2\r\n", nil)
	if v2.status != 200 || v2.contentType != typeUploadAdvert || string(v2.body) != "000eversion 2\n0013ls-refs=unborn\n0012fetch=shallow\n0000" {
		t.Fatalf("v2 advertisement: %d %s %q", v2.status, v2.contentType, v2.body)
	}
	for _, header := range []string{"Git-Protocol: version=2\r\n", "git-protocol: version=1:version=2\r\n"} {
		if res := f.do("GET", "/adaiversen/empty.git/info/refs?service=git-upload-pack", header, nil); !bytes.Equal(res.body, v2.body) {
			t.Errorf("%q did not select v2", header)
		}
	}

	// Empty repository, receive-pack: the capabilities ride on the capabilities^{} pseudo-ref.
	rp := f.do("GET", "/adaiversen/empty.git/info/refs?service=git-receive-pack", "git-protocol: version=2\r\n", nil)
	want := "001f# service=git-receive-pack\n0000" + pkt(zeroHex+" capabilities^{}\x00report-status delete-refs side-band-64k ofs-delta\n") + "0000"
	if rp.status != 200 || rp.contentType != typeReceiveAdvert || string(rp.body) != want {
		t.Fatalf("empty receive-pack advertisement: %d %s %q", rp.status, rp.contentType, rp.body)
	}
	// Empty repository, upload-pack v0.
	v0 := f.do("GET", "/adaiversen/empty.git/info/refs?service=git-upload-pack", "", nil)
	if lines := pktLines(t, v0.body); v0.contentType != typeUploadAdvert || lines[0] != "# service=git-upload-pack\n" || lines[1] != "0000" ||
		!strings.HasPrefix(lines[2], zeroHex+" capabilities^{}\x00") || lines[3] != "0000" {
		t.Fatalf("empty v0 advertisement: %q", v0.body)
	}

	c1 := f.seed("full", map[string]string{"a.txt": "a\n"})
	tag := tagOf("v1", c1[0])
	f.mustPush("full", []update{{zero, c1[0].id, "refs/heads/feature/x"}, {zero, tag.id, "refs/tags/v1"}}, tag)
	rp = f.do("GET", "/adaiversen/full.git/info/refs?service=git-receive-pack", "", nil)
	want = "001f# service=git-receive-pack\n0000" +
		pkt(c1[0].id.String()+" refs/heads/feature/x\x00report-status delete-refs side-band-64k ofs-delta\n") +
		pkt(c1[0].id.String()+" refs/heads/main\n") + pkt(tag.id.String()+" refs/tags/v1\n") + "0000"
	if string(rp.body) != want {
		t.Fatalf("receive-pack advertisement:\n%q\nwant\n%q", rp.body, want)
	}
	v0 = f.do("GET", "/adaiversen/full.git/info/refs?service=git-upload-pack", "", nil)
	lines := pktLines(t, v0.body)
	if lines[0] != "# service=git-upload-pack\n" || lines[1] != "0000" ||
		!strings.HasPrefix(lines[2], c1[0].id.String()+" HEAD\x00") || !strings.Contains(lines[2], " symref=HEAD:refs/heads/main") ||
		!strings.Contains(lines[2], "side-band-64k") || !strings.Contains(lines[2], "shallow") ||
		lines[5] != tag.id.String()+" refs/tags/v1\n" || lines[6] != c1[0].id.String()+" refs/tags/v1^{}\n" || lines[7] != "0000" {
		t.Fatalf("v0 advertisement: %q", lines)
	}

	// The advertisement is rendered once and replaced after a ref change.
	r := f.st.Repo("full")
	before := &f.git.advertReceive(r)[0]
	if before != &f.git.advertReceive(r)[0] {
		t.Fatal("the advertisement was rendered twice")
	}
	c2 := commitOf(2, map[string]string{"a.txt": "b\n"}, c1[0])
	f.mustPush("full", []update{{c1[0].id, c2[0].id, "refs/heads/main"}}, c2...)
	if rp = f.do("GET", "/adaiversen/full.git/info/refs?service=git-receive-pack", "", nil); !bytes.Contains(rp.body, []byte(c2[0].id.String()+" refs/heads/main\n")) {
		t.Fatalf("stale advertisement after a push: %q", rp.body)
	}
}

func TestNotFoundAuthAndBadRequests(t *testing.T) {
	f := newForge(t)
	c1 := f.seed("real", map[string]string{"a": "a"})
	for _, path := range []string{
		"/adaiversen/real-missing.git/info/refs?service=git-upload-pack",
		"/adaiversen/real-missing.git/info/refs?service=git-receive-pack",
		"/olgabauer/real.git/info/refs?service=git-upload-pack",
		"/adaiversen/real.git/info/refs?service=git-nope",
		"/adaiversen/real.git/info/refs",
		"/adaiversen/real.git/objects/info/packs",
		"/adaiversen/real.git/HEAD",
	} {
		if res := f.do("GET", path, "git-protocol: version=2\r\n", nil); res.status != 404 || !bytes.HasPrefix(res.body, []byte(`{"message":`)) {
			t.Errorf("GET %s: %d %q", path, res.status, res.body)
		}
	}
	for _, path := range []string{"/adaiversen/real-missing.git/git-upload-pack", "/adaiversen/real-missing.git/git-receive-pack"} {
		if res := f.do("POST", path, "", []byte("0000")); res.status != 404 {
			t.Errorf("POST %s: %d", path, res.status)
		}
	}
	if res := f.do("POST", "/adaiversen/real.git/info/refs?service=git-upload-pack", "", []byte("x")); res.status != 404 {
		t.Errorf("POST info/refs: %d", res.status)
	}

	// No token: 401 asking for Basic credentials; Basic with the token as password works.
	raw := f.tester.Do([]byte("GET /adaiversen/real.git/info/refs?service=git-upload-pack HTTP/1.1\r\nhost: h\r\n\r\n"))
	if !bytes.HasPrefix(raw, []byte("HTTP/1.1 401 ")) || !bytes.Contains(raw, []byte("WWW-Authenticate: Basic realm=\"forge\"\r\n")) {
		t.Fatalf("anonymous request: %q", raw)
	}
	raw = f.tester.Do([]byte("GET /adaiversen/real.git/info/refs?service=git-upload-pack HTTP/1.1\r\nhost: h\r\nauthorization: Basic " +
		"Z2l0OnRvay0wMTIzNDU2Nzg5YWJjZGVm\r\n\r\n")) // git:tok-0123456789abcdef
	if !bytes.HasPrefix(raw, []byte("HTTP/1.1 200 ")) {
		t.Fatalf("Basic credentials: %q", raw)
	}

	// Malformed bodies are answered at once with a 4xx, and the repository keeps working.
	for _, body := range []string{"zzzznot-a-pkt-line", "", "0000", "0003", "fffftoo short", pkt("command=nope\n") + "0000",
		pkt("command=fetch\n") + "0001" + pkt("want nothex\n") + "0000",
		pkt("command=fetch\n") + "0001" + pkt("want "+strings.Repeat("1", 40)+"\n") + pkt("done\n") + "0000",
		pkt("command=fetch\n") + "0001" + pkt("want "+c1[0].id.String()+"\n") + pkt("filter blob:none\n") + pkt("done\n") + "0000"} {
		if res := f.do("POST", "/adaiversen/real.git/git-upload-pack", "", []byte(body)); res.status != 400 {
			t.Errorf("upload-pack body %q: %d %q", body, res.status, res.body)
		}
	}
	for _, body := range []string{"zzzz", pkt("short line\n") + "0000", pkt(strings.Repeat("z", 40)+" "+c1[0].id.String()+" refs/heads/x\n") + "0000"} {
		if res := f.do("POST", "/adaiversen/real.git/git-receive-pack", "", []byte(body)); res.status != 400 {
			t.Errorf("receive-pack body %q: %d", body, res.status)
		}
	}
	if refs := f.lsRefs("real"); len(refs) != 2 || refs[1] != c1[0].id.String()+" refs/heads/main" {
		t.Fatalf("after bad requests: %q", refs)
	}
}

// ---- ls-refs ----

func TestLsRefs(t *testing.T) {
	f := newForge(t)
	f.create("empty")
	if refs := f.lsRefs("empty"); len(refs) != 1 || refs[0] != "unborn HEAD symref-target:refs/heads/main" {
		t.Fatalf("ls-refs of an empty repository: %q", refs)
	}
	// Without "unborn" an empty repository lists nothing at all.
	res := f.uploadPackPost("empty", pkt("command=ls-refs\n")+"0001"+pkt("peel\n")+pkt("symrefs\n")+"0000")
	if string(res.body) != "0000" {
		t.Fatalf("ls-refs without unborn: %q", res.body)
	}
	if res = f.uploadPackPost("empty", pkt("command=ls-refs\n")+"0000"); string(res.body) != "0000" {
		t.Fatalf("ls-refs without arguments: %q", res.body)
	}

	c1 := f.seed("r", map[string]string{"a": "a"})
	c2 := commitOf(2, map[string]string{"a": "b"}, c1[0])
	tag := tagOf("v1", c2[0])
	f.mustPush("r", []update{{c1[0].id, c2[0].id, "refs/heads/main"}, {zero, tag.id, "refs/tags/v1"}, {zero, c1[0].id, "refs/heads/feature/x"}}, append(c2, tag)...)
	head := c2[0].id.String()
	want := []string{
		head + " HEAD symref-target:refs/heads/main",
		c1[0].id.String() + " refs/heads/feature/x",
		head + " refs/heads/main",
		tag.id.String() + " refs/tags/v1 peeled:" + head,
	}
	if got := f.lsRefs("r"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ls-refs:\n%q\nwant\n%q", got, want)
	}
	// Agent and object-format lines before the delimiter are accepted and ignored.
	res = f.uploadPackPost("r", pkt("command=ls-refs\n")+pkt("agent=git/2.45\n")+pkt("object-format=sha1\n")+"0001"+pkt("ref-prefix refs/tags/\n")+"0000")
	if got := pktLines(t, res.body); len(got) != 2 || got[0] != tag.id.String()+" refs/tags/v1\n" {
		t.Fatalf("ls-refs without peel: %q", got)
	}
	for prefix, wantRefs := range map[string][]string{
		"refs/heads/":          want[1:3],
		"refs/tags/":           want[3:],
		"refs/does-not-exist/": nil,
		"refs/heads/main":      want[2:3],
		"refs/heads/feature/x": want[1:2],
		"HEAD":                 want[:1],
	} {
		if got := f.lsRefs("r", prefix); strings.Join(got, "|") != strings.Join(wantRefs, "|") {
			t.Errorf("ls-refs %s: %q, want %q", prefix, got, wantRefs)
		}
	}
	if got := f.lsRefs("r", "refs/heads/main", "refs/tags/"); len(got) != 2 {
		t.Errorf("two prefixes: %q", got)
	}
}

// ---- push ----

func TestPush(t *testing.T) {
	f := newForge(t)
	r := f.create("p")
	c1 := commitOf(1, map[string]string{"config/token.css": "one\n"})
	if report := f.push("p", []update{{zero, c1[0].id, "refs/heads/main"}}, c1...); strings.Join(report, "|") != "unpack ok|ok refs/heads/main" {
		t.Fatalf("first push: %q", report)
	}
	if head := r.Head(); head == nil || head.ID != c1[0].id || r.Objects.Len() != 4 {
		t.Fatalf("first push did not land: %v, %d objects", head, r.Objects.Len())
	}
	// The stored objects are the pushed bytes, and keep the client's zlib streams.
	for _, o := range c1 {
		got := r.Objects.Get(o.id)
		if got == nil || !bytes.Equal(got.Data, o.data) || !bytes.Equal(got.Z, deflate(o.data)) {
			t.Fatalf("object %s was not stored verbatim", o.id)
		}
	}

	// Create a branch at an existing commit: a pack with zero objects.
	if report := f.push("p", []update{{zero, c1[0].id, "refs/heads/feature"}}); strings.Join(report, "|") != "unpack ok|ok refs/heads/feature" {
		t.Fatalf("create branch: %q", report)
	}
	// Fast-forward over two commits in one pack, objects in an order where children come first.
	c2 := commitOf(2, map[string]string{"config/token.css": "two\n"}, c1[0])
	c3 := commitOf(3, map[string]string{"config/token.css": "three\n", "new": "n"}, c2[0])
	objs := append(append([]object{}, c3...), c2...)
	f.mustPush("p", []update{{c1[0].id, c3[0].id, "refs/heads/main"}}, objs...)

	// Non-fast-forward: refused per ref, HTTP 200, still "unpack ok", the ref does not move, and the
	// pack may resend objects the server has.
	sib := commitOf(4, map[string]string{"other": "s"}, c1[0])
	report := f.push("p", []update{{c3[0].id, sib[0].id, "refs/heads/main"}}, append(sib, c1...)...)
	if strings.Join(report, "|") != "unpack ok|ng refs/heads/main non-fast-forward" {
		t.Fatalf("non-fast-forward: %q", report)
	}
	if tip, _ := r.Branch("main"); tip != c3[0].id {
		t.Fatal("a refused update moved the ref")
	}
	// Mixed: one forward, one sideways. Exactly two lines, only the good one applied.
	fwd := commitOf(5, map[string]string{"config/token.css": "five\n"}, c3[0])
	f.mustPush("p", []update{{zero, c3[0].id, "refs/heads/side"}})
	report = f.push("p", []update{{c3[0].id, fwd[0].id, "refs/heads/main"}, {c3[0].id, sib[0].id, "refs/heads/side"}}, append(fwd, sib...)...)
	if strings.Join(report, "|") != "unpack ok|ok refs/heads/main|ng refs/heads/side non-fast-forward" {
		t.Fatalf("mixed report: %q", report)
	}
	if main, _ := r.Branch("main"); main != fwd[0].id {
		t.Fatal("the accepted update of a mixed push was not applied")
	}
	if side, _ := r.Branch("side"); side != c3[0].id {
		t.Fatal("the refused update of a mixed push was applied")
	}

	// Delete: no pack at all in the body.
	tag := tagOf("v1", c1[0])
	f.mustPush("p", []update{{zero, tag.id, "refs/tags/v1"}}, tag)
	if report := f.push("p", []update{{c1[0].id, zero, "refs/heads/feature"}, {tag.id, zero, "refs/tags/v1"}}); strings.Join(report, "|") != "unpack ok|ok refs/heads/feature|ok refs/tags/v1" {
		t.Fatalf("delete: %q", report)
	}
	if _, ok := r.Branch("feature"); ok || len(r.Tags()) != 0 {
		t.Fatal("deleted refs are still there")
	}

	// Other refusals.
	for _, tc := range []struct {
		u      update
		reason string
	}{
		{update{zero, c1[0].id, "refs/heads/main"}, "already exists"},
		{update{c1[0].id, fwd[0].id, "refs/heads/main"}, ""}, // already there: nothing to do
		{update{c1[0].id, c2[0].id, "refs/heads/main"}, "stale info"},
		{update{zero, gitobj.OID{7}, "refs/heads/ghost"}, "missing necessary objects"},
		{update{zero, c1[1].id, "refs/heads/tree"}, "branch tip is not a commit"},
		{update{zero, c1[0].id, "refs/heads/bad..name"}, "funny refname"},
		{update{zero, c1[0].id, "HEAD"}, "funny refname"},
		{update{c1[0].id, zero, "refs/heads/never-existed"}, ""},
	} {
		want := "ok " + tc.u.ref
		if tc.reason != "" {
			want = "ng " + tc.u.ref + " " + tc.reason
		}
		if report := f.push("p", []update{tc.u}); len(report) != 2 || report[1] != want {
			t.Errorf("update %s: %q, want %q", tc.u.ref, report, want)
		}
	}

	// Without side-band the report is the bare pkt-line stream; without report-status there is none.
	res := f.do("POST", "/adaiversen/p.git/git-receive-pack", "", pushBody([]update{{zero, c1[0].id, "refs/heads/plain"}}, "report-status", packOf()))
	if string(res.body) != "000eunpack ok\n0018ok refs/heads/plain\n0000" {
		t.Fatalf("report without side-band: %q", res.body)
	}
	res = f.do("POST", "/adaiversen/p.git/git-receive-pack", "", pushBody([]update{{zero, c1[0].id, "refs/heads/quiet"}}, "", packOf()))
	if len(res.body) != 0 {
		t.Fatalf("report although none was asked for: %q", res.body)
	}
	if _, ok := r.Branch("quiet"); !ok {
		t.Fatal("an update without report-status was not applied")
	}
	// The exact bytes of the usual reply.
	res = f.do("POST", "/adaiversen/p.git/git-receive-pack", "", pushBody([]update{{zero, c1[0].id, "refs/heads/exact"}}, "report-status side-band-64k", packOf()))
	if string(res.body) != "002f\x01000eunpack ok\n0018ok refs/heads/exact\n00000000" {
		t.Fatalf("side-band report: %q", res.body)
	}
}

func TestPushBadPacks(t *testing.T) {
	f := newForge(t)
	r := f.create("b")
	c1 := commitOf(1, map[string]string{"a": "a"})
	up := []update{{zero, c1[0].id, "refs/heads/main"}}
	good := packOf(c1...)

	corrupt := bytes.Clone(good)
	corrupt[len(corrupt)-1] ^= 1
	truncated := good[:len(good)-25]
	lying := finishPack(2, good[12:len(good)-20]) // header says 2 objects, 3 follow
	short := finishPack(4, good[12:len(good)-20]) // header says 4 objects, 3 follow
	badZlib := finishPack(1, append(entryHeader(3, 5), 'h', 'e', 'l', 'l', 'o'))
	wrongSize := finishPack(1, append(entryHeader(3, 4), deflate([]byte("hello"))...))
	for name, pack := range map[string][]byte{"checksum": corrupt, "truncated": truncated, "count low": lying, "count high": short,
		"not zlib": badZlib, "wrong size": wrongSize, "garbage": []byte("this is not a pack at all, not even close")} {
		report := f.pushRaw("b", pushBody(up, "report-status side-band-64k", pack))
		if len(report) != 2 || !strings.HasPrefix(report[0], "unpack ") || report[0] == "unpack ok" || report[1] != "ng refs/heads/main unpacker error" {
			t.Errorf("%s: %q", name, report)
		}
		if r.Objects.Len() != 0 || !r.IsEmpty() {
			t.Fatalf("%s: a rejected pack left %d objects behind", name, r.Objects.Len())
		}
	}

	// A well-formed pack whose commit names a tree nobody has: nothing is stored, the ref is refused.
	report := f.pushRaw("b", pushBody(up, "report-status side-band-64k", packOf(c1[0])))
	if strings.Join(report, "|") != "unpack ok|ng refs/heads/main missing necessary objects" || r.Objects.Len() != 0 {
		t.Fatalf("incomplete pack: %q, %d objects", report, r.Objects.Len())
	}
	// And then the same push, complete, works.
	f.mustPush("b", up, c1...)
}

// Sizes in a pack are claims. A pack that claims more than it holds, or more than the server
// accepts, is refused without the server ever reserving memory for the claim.
func TestPackSizeClaimsAreNotTrusted(t *testing.T) {
	f := newForge(t)
	r := f.create("limits")
	up := []update{{zero, gitobj.OID{1}, "refs/heads/main"}}
	refused := func(name string, pack []byte) {
		t.Helper()
		report := f.pushRaw("limits", pushBody(up, "report-status side-band-64k", pack))
		if len(report) != 2 || !strings.HasPrefix(report[0], "unpack ") || report[0] == "unpack ok" || report[1] != "ng refs/heads/main unpacker error" {
			t.Errorf("%s: %q", name, report)
		}
		if r.Objects.Len() != 0 {
			t.Fatalf("%s: %d objects stored from a refused pack", name, r.Objects.Len())
		}
		if cap(f.git.up.buf) > keepInflateBuf {
			t.Fatalf("%s: the inflate buffer was left at %d bytes", name, cap(f.git.up.buf))
		}
	}
	// Enough trailing bytes for the compression-ratio test to let a 48 MiB claim through.
	padding := make([]byte, 64<<10)
	x := uint32(3)
	for i := range padding {
		x = x*1664525 + 1013904223
		padding[i] = byte(x >> 24)
	}
	filler := append(entryHeader(3, len(padding)), deflate(padding)...)

	// An entry that claims 48 MiB and holds five bytes.
	lie := append(entryHeader(3, 48<<20), deflate([]byte("hello"))...)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	refused("48 MiB claim, 5 bytes", finishPack(2, append(bytes.Clone(lie), filler...)))
	runtime.ReadMemStats(&after)
	if spent := after.TotalAlloc - before.TotalAlloc; spent > 8<<20 {
		t.Fatalf("a false 48 MiB size claim made the server allocate %d bytes", spent)
	}
	// An entry that claims more than any object may be.
	refused("object over the limit", finishPack(2, append(append(entryHeader(3, maxObjectSize+1), deflate([]byte("hello"))...), filler...)))
	// A delta whose header promises a huge result.
	huge := []byte{5, 0x80, 0x80, 0x80, 0x80, 0x10, 5, 'h', 'e', 'l', 'l', 'o'} // base 5 bytes, result 2^32
	if _, err := applyDelta([]byte("hello"), huge); err != errPackTooLarge {
		t.Errorf("delta with a 4 GiB result: %v", err)
	}
	promise := []byte{5, 0x80, 0x80, 0x80, 0x10, 5, 'h', 'e', 'l', 'l', 'o'} // result 32 MiB, 5 bytes of instructions
	if out, err := applyDelta([]byte("hello"), promise); err != errPackDelta || cap(out) > growStep {
		t.Errorf("delta with an unkept promise: %v, %d bytes reserved", err, cap(out))
	}

	// A genuinely large object still goes through, and the buffer it needed is given back.
	large := bytes.Repeat([]byte("a line of text that compresses very well indeed\n"), 150000) // about 7 MiB
	c := commitOf(1, map[string]string{"large.txt": string(large)})
	f.mustPush("limits", []update{{zero, c[0].id, "refs/heads/main"}}, c...)
	if got := r.Objects.Get(blob(string(large)).id); got == nil || !bytes.Equal(got.Data, large) {
		t.Fatal("the large object was not stored intact")
	}
	if cap(f.git.up.buf) > keepInflateBuf {
		t.Fatalf("the inflate buffer was kept at %d bytes", cap(f.git.up.buf))
	}
	if got := f.fetch("limits", []gitobj.OID{c[0].id}, nil, nil, 0); !got.has(c...) {
		t.Fatal("clone of the large object is wrong")
	}
}

// delta builds a git delta that turns base into target using one copy of their common prefix and
// literal inserts for the rest.
func delta(base, target []byte) []byte {
	varint := func(n int) []byte {
		var out []byte
		for {
			b := byte(n & 0x7f)
			if n >>= 7; n == 0 {
				return append(out, b)
			}
			out = append(out, b|0x80)
		}
	}
	d := append(varint(len(base)), varint(len(target))...)
	common := 0
	for common < len(base) && common < len(target) && base[common] == target[common] {
		common++
	}
	if common > 0 {
		d = append(d, 0x80|0x10|0x20, byte(common), byte(common>>8)) // copy from offset 0, two size bytes
	}
	for rest := target[common:]; len(rest) > 0; {
		n := min(len(rest), 127)
		d = append(d, byte(n))
		d = append(d, rest[:n]...)
		rest = rest[n:]
	}
	return d
}

func TestPushDeltas(t *testing.T) {
	f := newForge(t)
	r := f.create("d")
	text := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 40)
	c1 := commitOf(1, map[string]string{"big.txt": text})
	f.mustPush("d", []update{{zero, c1[0].id, "refs/heads/main"}}, c1...)
	base := blob(text)

	// Next commit: the blob as an OFS_DELTA against... nothing in this pack, so use REF_DELTA against
	// the stored blob (a thin pack); then a second blob as an OFS_DELTA against the first delta's
	// result, which sits earlier in the same pack and is itself a delta.
	text2 := text + "one more line\n"
	text3 := text2 + strings.Repeat("and a long tail ", 30)
	b2, b3 := blob(text2), blob(text3)
	c2 := commitOf(2, map[string]string{"big.txt": text2, "bigger.txt": text3}, c1[0])
	var entries []byte
	offset := map[gitobj.OID]int{}
	add := func(id gitobj.OID, e []byte) {
		offset[id] = 12 + len(entries)
		entries = append(entries, e...)
	}
	d2 := delta(base.data, b2.data)
	add(b2.id, append(append(entryHeader(packRefDelta, len(d2)), base.id[:]...), deflate(d2)...))
	d3 := delta(b2.data, b3.data)
	back := 12 + len(entries) - offset[b2.id]
	if back >= 128 {
		// Two-byte offset encoding: big-endian base-128 with the +1 bias on the high byte.
		add(b3.id, append(append(entryHeader(packOfsDelta, len(d3)), byte((back>>7)-1)|0x80, byte(back&0x7f)), deflate(d3)...))
	} else {
		add(b3.id, append(append(entryHeader(packOfsDelta, len(d3)), byte(back)), deflate(d3)...))
	}
	count := 2
	for _, o := range c2 {
		if o.id == b2.id || o.id == b3.id {
			continue
		}
		add(o.id, append(entryHeader(int(o.typ), len(o.data)), deflate(o.data)...))
		count++
	}
	report := f.pushRaw("d", pushBody([]update{{c1[0].id, c2[0].id, "refs/heads/main"}}, "report-status side-band-64k ofs-delta", finishPack(count, entries)))
	if strings.Join(report, "|") != "unpack ok|ok refs/heads/main" {
		t.Fatalf("delta push: %q", report)
	}
	for _, o := range []object{b2, b3} {
		if got := r.Objects.Get(o.id); got == nil || !bytes.Equal(got.Data, o.data) {
			t.Fatalf("delta object %s was not rebuilt", o.id)
		}
	}
	// A clone gets every object back whole and correct.
	if got := f.fetch("d", []gitobj.OID{c2[0].id}, nil, nil, 0); !got.has(c2...) || !got.has(c1...) {
		t.Fatal("clone after a delta push is incomplete")
	}

	// A delta whose base is nowhere: unpack error, nothing stored.
	n := r.Objects.Len()
	orphan := append(append(entryHeader(packRefDelta, len(d2)), bytes.Repeat([]byte{9}, 20)...), deflate(d2)...)
	report = f.pushRaw("d", pushBody([]update{{zero, c2[0].id, "refs/heads/x"}}, "report-status side-band-64k", finishPack(1, orphan)))
	if len(report) != 2 || report[0] == "unpack ok" || report[1] != "ng refs/heads/x unpacker error" || r.Objects.Len() != n {
		t.Fatalf("orphan delta: %q", report)
	}
	for _, bad := range [][]byte{{}, {5}, {byte(len(base.data)), 3, 0x90}, {byte(len(base.data) & 0x7f), 3, 2, 'a'}, {0, 2, 0}} {
		if _, err := applyDelta(base.data[:min(len(base.data), 100)], bad); err == nil {
			t.Errorf("applyDelta accepted %v", bad)
		}
	}
}

// ---- fetch ----

func TestCloneAndFetch(t *testing.T) {
	f := newForge(t)
	files := map[string]string{"src/a/deep.txt": "deep\n", "src/b.txt": "b\n", "README": strings.Repeat("r", 4096), "x": ""}
	c1 := f.seed("c", files)
	files2 := map[string]string{"src/a/deep.txt": "deep\n", "src/b.txt": "changed\n", "README": strings.Repeat("r", 4096), "new": "n"}
	c2 := commitOf(2, files2, c1[0])
	tag := tagOf("v1", c2[0])
	f.mustPush("c", []update{{c1[0].id, c2[0].id, "refs/heads/main"}, {zero, tag.id, "refs/tags/v1"}}, append(c2, tag)...)

	// Full clone: wants are the branch tip and the tag object.
	clone := f.fetch("c", []gitobj.OID{c2[0].id, tag.id}, nil, nil, 0)
	if !clone.has(c1...) || !clone.has(c2...) || !clone.has(tag) {
		t.Fatal("full clone is missing objects or has wrong bytes")
	}
	distinct := map[gitobj.OID]bool{}
	for _, o := range append(append(append([]object{}, c1...), c2...), tag) {
		distinct[o.id] = true
	}
	if len(clone.objects) != len(distinct) {
		t.Fatalf("clone has %d objects, want exactly %d", len(clone.objects), len(distinct))
	}
	// Incremental: only what is new since C1; the unchanged blobs and subtree stay home.
	inc := f.fetch("c", []gitobj.OID{c2[0].id}, []gitobj.OID{c1[0].id}, nil, 0)
	if !inc.has(c2[0]) || inc.hasAny(c1...) {
		t.Fatal("incremental fetch sent the old commit or objects the client has")
	}
	var fresh []object
	old := map[gitobj.OID]bool{}
	for _, o := range c1 {
		old[o.id] = true
	}
	for _, o := range c2 {
		if !old[o.id] {
			fresh = append(fresh, o)
		}
	}
	if !inc.has(fresh...) || len(inc.objects) != len(fresh) {
		t.Fatalf("incremental fetch has %d objects, want %d", len(inc.objects), len(fresh))
	}
	// Nothing new: a valid pack with no objects, the 32 well-known bytes.
	none := f.fetch("c", []gitobj.OID{c2[0].id}, []gitobj.OID{c2[0].id}, nil, 0)
	if len(none.objects) != 0 || hex.EncodeToString(none.pack) != "5041434b0000000200000000029d08823bd8a8eab510ad6ac75c823cfd3ed31e" {
		t.Fatalf("nothing-new fetch: %d objects, pack %x", len(none.objects), none.pack)
	}
	// Unknown haves are ignored.
	if got := f.fetch("c", []gitobj.OID{c2[0].id}, []gitobj.OID{{1, 2, 3}}, nil, 0); len(got.objects) != len(distinct)-1 {
		t.Fatalf("fetch with an unknown have: %d objects", len(got.objects))
	}
	// A want that is a tag object alone still brings its history.
	if got := f.fetch("c", []gitobj.OID{tag.id}, nil, nil, 0); !got.has(tag) || !got.has(c1...) || !got.has(c2...) {
		t.Fatal("fetch of a tag is incomplete")
	}
}

func TestShallow(t *testing.T) {
	f := newForge(t)
	c1 := f.seed("s", map[string]string{"one": "1\n", "shared": "same\n"})
	c2 := commitOf(2, map[string]string{"two": "2\n", "shared": "same\n"}, c1[0])
	c3 := commitOf(3, map[string]string{"three": "3\n", "shared": "same\n"}, c2[0])
	f.mustPush("s", []update{{c1[0].id, c3[0].id, "refs/heads/main"}}, append(c3, c2...)...)
	tip, mid, root := c3[0], c2[0], c1[0]

	// Depth 1: the tip with its tree and blobs, nothing older, "shallow <tip>".
	d1 := f.fetch("s", []gitobj.OID{tip.id}, nil, nil, 1)
	if !d1.has(c3...) || d1.hasAny(mid, root) || len(d1.objects) != len(c3) || strings.Join(d1.shallow, "|") != "shallow "+tip.id.String() {
		t.Fatalf("depth 1: %d objects, shallow %q", len(d1.objects), d1.shallow)
	}
	// Deepen to 2 from a depth-1 clone: the parent commit, its tree and the blob the client lacks;
	// the shared blob and the tip stay home. The parent is the new boundary, the tip is unshallowed.
	d2 := f.fetch("s", []gitobj.OID{tip.id}, []gitobj.OID{tip.id}, []gitobj.OID{tip.id}, 2)
	if !d2.has(mid, c2[1], blob("2\n")) || d2.hasAny(tip, root, blob("same\n")) || len(d2.objects) != 3 {
		t.Fatalf("deepen 2: %d objects", len(d2.objects))
	}
	sort.Strings(d2.shallow)
	if strings.Join(d2.shallow, "|") != "shallow "+mid.id.String()+"|unshallow "+tip.id.String() {
		t.Fatalf("deepen 2 shallow-info: %q", d2.shallow)
	}
	// A new tip on a shallow clone of the middle commit, depth 1: only the tip.
	n3 := f.fetch("s", []gitobj.OID{tip.id}, []gitobj.OID{mid.id}, []gitobj.OID{mid.id}, 1)
	if !n3.has(tip, c3[1], blob("3\n")) || n3.hasAny(mid, root) || strings.Join(n3.shallow, "|") != "shallow "+tip.id.String() {
		t.Fatalf("new tip on a shallow clone: %d objects, %q", len(n3.objects), n3.shallow)
	}
	// Depth beyond the history: everything, and the root is the boundary.
	all := f.fetch("s", []gitobj.OID{tip.id}, nil, nil, 3)
	if !all.has(c1...) || !all.has(c2...) || !all.has(c3...) || strings.Join(all.shallow, "|") != "shallow "+root.id.String() {
		t.Fatalf("depth 3: %d objects, %q", len(all.objects), all.shallow)
	}
	if deep := f.fetch("s", []gitobj.OID{tip.id}, nil, nil, 50); len(deep.shallow) != 0 || len(deep.objects) != len(all.objects) {
		t.Fatalf("depth 50: %q", deep.shallow)
	}
	// The other ways to limit history, as git sends them.
	limited := func(args ...string) fetched {
		body := pkt("command=fetch\n") + "0001" + pkt("want "+tip.id.String()+"\n")
		for _, a := range args {
			body += pkt(a + "\n")
		}
		res := f.uploadPackPost("s", body+pkt("done\n")+"0000")
		if res.status != 200 {
			t.Fatalf("fetch %q: %d %q", args, res.status, res.body)
		}
		return f.parseFetch(res.body, true)
	}
	// deepen-relative: one more level below where the client's history ends now.
	rel := limited("have "+tip.id.String(), "shallow "+tip.id.String(), "deepen 1", "deepen-relative")
	sort.Strings(rel.shallow)
	if !rel.has(mid) || rel.hasAny(tip, root) || strings.Join(rel.shallow, "|") != "shallow "+mid.id.String()+"|unshallow "+tip.id.String() {
		t.Fatalf("deepen-relative: %d objects, %q", len(rel.objects), rel.shallow)
	}
	// "Everything" (git fetch --unshallow): the rest of the history, the client's boundary lifted.
	un := limited("have "+tip.id.String(), "shallow "+tip.id.String(), "deepen 2147483647")
	if !un.has(mid, root) || un.hasAny(tip) || strings.Join(un.shallow, "|") != "unshallow "+tip.id.String() {
		t.Fatalf("unshallow: %d objects, %q", len(un.objects), un.shallow)
	}
	// deepen-since: commits at or after a time (the three commits are one second apart).
	since := limited("deepen-since 1700000002")
	if !since.has(tip, mid) || since.hasAny(root) || strings.Join(since.shallow, "|") != "shallow "+mid.id.String() {
		t.Fatalf("deepen-since: %d objects, %q", len(since.objects), since.shallow)
	}
	// deepen-not: everything not reachable from a ref.
	f.mustPush("s", []update{{zero, root.id, "refs/heads/old"}})
	not := limited("deepen-not old")
	if !not.has(tip, mid) || not.hasAny(root) || strings.Join(not.shallow, "|") != "shallow "+mid.id.String() {
		t.Fatalf("deepen-not: %d objects, %q", len(not.objects), not.shallow)
	}
	for _, bad := range []string{"deepen-since 1800000000", "deepen-not no-such-ref", "deepen 0", "deepen x"} {
		res := f.uploadPackPost("s", pkt("command=fetch\n")+"0001"+pkt("want "+tip.id.String()+"\n")+pkt(bad+"\n")+pkt("done\n")+"0000")
		if res.status != 400 {
			t.Errorf("fetch with %q: %d", bad, res.status)
		}
	}
	// A full fetch from a shallow client that has only the tip: the older history, not the tip.
	rest := f.fetch("s", []gitobj.OID{tip.id}, []gitobj.OID{tip.id}, []gitobj.OID{tip.id}, 0)
	if len(rest.objects) != 0 {
		// Without a depth, the window stops at what the client has: nothing to send.
		t.Fatalf("full fetch from a shallow client sent %d objects", len(rest.objects))
	}
}

func TestSidebandFramingAndLargeObjects(t *testing.T) {
	f := newForge(t)
	// Incompressible content, so the pack is far larger than one side-band packet.
	big := make([]byte, 300<<10)
	x := uint32(1)
	for i := range big {
		x = x*1664525 + 1013904223
		big[i] = byte(x >> 24)
	}
	c1 := f.seed("big", map[string]string{"blob.bin": string(big), "small": "s"})
	got := f.fetch("big", []gitobj.OID{c1[0].id}, nil, nil, 0)
	if !got.has(c1...) || got.packets < 5 || got.maxPacket > maxPkt {
		t.Fatalf("large fetch: %d packets, largest %d", got.packets, got.maxPacket)
	}
	// 100 files in nested directories round-trip.
	files := map[string]string{}
	for i := 0; i < 100; i++ {
		files[fmt.Sprintf("dir%d/sub%d/file%d.txt", i%7, i%3, i)] = fmt.Sprintf("file %d revision 1\n", i)
	}
	c := f.seed("hundred", files)
	if got := f.fetch("hundred", []gitobj.OID{c[0].id}, nil, nil, 0); !got.has(c...) {
		t.Fatal("100-file clone is incomplete")
	}
}

func TestNegotiationWithoutDone(t *testing.T) {
	f := newForge(t)
	c1 := f.seed("n", map[string]string{"a": "a"})
	c2 := commitOf(2, map[string]string{"a": "b"}, c1[0])
	f.mustPush("n", []update{{c1[0].id, c2[0].id, "refs/heads/main"}}, c2...)
	want := pkt("command=fetch\n") + "0001" + pkt("want "+c2[0].id.String()+"\n")
	// Nothing in common yet: NAK and the round ends.
	res := f.uploadPackPost("n", want+pkt("have "+strings.Repeat("1", 40)+"\n")+"0000")
	if string(res.body) != pkt("acknowledgments\n")+pkt("NAK\n")+"0000" {
		t.Fatalf("round without common commits: %q", res.body)
	}
	// A common commit: ACK, ready, and the pack follows at once.
	res = f.uploadPackPost("n", want+pkt("have "+c1[0].id.String()+"\n")+"0000")
	prefix := pkt("acknowledgments\n") + pkt("ACK "+c1[0].id.String()+"\n") + pkt("ready\n") + "0001"
	if !bytes.HasPrefix(res.body, []byte(prefix)) {
		t.Fatalf("round with a common commit: %q", res.body[:min(len(res.body), 120)])
	}
	if got := f.parseFetch(res.body[len(prefix):], false); !got.has(c2[0]) || got.hasAny(c1...) {
		t.Fatal("pack after ready is wrong")
	}
}

func TestFetchV0(t *testing.T) {
	f := newForge(t)
	c1 := f.seed("v0", map[string]string{"a": "a"})
	c2 := commitOf(2, map[string]string{"a": "b"}, c1[0])
	f.mustPush("v0", []update{{c1[0].id, c2[0].id, "refs/heads/main"}}, c2...)
	post := func(body string) []byte {
		res := f.do("POST", "/adaiversen/v0.git/git-upload-pack", "", []byte(body))
		if res.status != 200 {
			t.Fatalf("v0 fetch: %d %q", res.status, res.body)
		}
		return res.body
	}
	unpackAll := func(pack []byte) map[gitobj.OID]bool {
		db := gitobj.NewDB(&gitobj.Arena{})
		var u unpacker
		if err := u.unpack(db, pack); err != nil {
			t.Fatalf("v0 pack: %v", err)
		}
		out := map[gitobj.OID]bool{}
		for _, o := range u.added {
			out[o.ID] = true
		}
		return out
	}
	// Clone with side-band: NAK, then the pack in band 1, then a flush.
	body := post(pkt("want "+c2[0].id.String()+" side-band-64k ofs-delta\n") + "0000" + pkt("done\n"))
	lines := pktLines(t, body)
	if lines[0] != "NAK\n" || lines[len(lines)-1] != "0000" {
		t.Fatalf("v0 clone framing: %q", lines[0])
	}
	var pack []byte
	for _, l := range lines[1 : len(lines)-1] {
		if l[0] != 1 {
			t.Fatalf("v0 packet in band %d", l[0])
		}
		pack = append(pack, l[1:]...)
	}
	if got := unpackAll(pack); len(got) != 6 || !got[c1[0].id] || !got[c2[0].id] {
		t.Fatalf("v0 clone: %d objects", len(got))
	}
	// Without side-band the pack follows the NAK raw.
	body = post(pkt("want "+c2[0].id.String()+"\n") + "0000" + pkt("have "+c1[0].id.String()+"\n") + pkt("done\n"))
	ack := pkt("ACK " + c1[0].id.String() + "\n")
	if !bytes.HasPrefix(body, []byte(ack)) {
		t.Fatalf("v0 fetch with a have: %q", body[:60])
	}
	if got := unpackAll(body[len(ack):]); len(got) != 3 || got[c1[0].id] {
		t.Fatalf("v0 incremental: %d objects", len(got))
	}
	// A request that stops after the wants: only the shallow list, or nothing at all.
	if body = post(pkt("want "+c2[0].id.String()+" side-band-64k\n") + pkt("deepen 1\n") + "0000"); string(body) != pkt("shallow "+c2[0].id.String()+"\n")+"0000" {
		t.Fatalf("v0 shallow list request: %q", body)
	}
	if body = post(pkt("want "+c2[0].id.String()+"\n") + "0000"); len(body) != 0 {
		t.Fatalf("v0 wants-only request: %q", body)
	}
	// A round without "done": just the ACK or NAK.
	if body = post(pkt("want "+c2[0].id.String()+"\n") + "0000" + pkt("have "+strings.Repeat("2", 40)+"\n") + "0000"); string(body) != pkt("NAK\n") {
		t.Fatalf("v0 round: %q", body)
	}
	// Shallow: the shallow lines and a flush come first.
	body = post(pkt("want "+c2[0].id.String()+" side-band-64k\n") + pkt("deepen 1\n") + "0000" + pkt("done\n"))
	if !bytes.HasPrefix(body, []byte(pkt("shallow "+c2[0].id.String()+"\n")+"0000"+pkt("NAK\n"))) {
		t.Fatalf("v0 shallow: %q", body[:80])
	}
}

func TestResponseCache(t *testing.T) {
	f := newForge(t)
	c1 := f.seed("k", map[string]string{"a": "a"})
	r := f.st.Repo("k")
	clone := func() fetched { return f.fetch("k", []gitobj.OID{c1[0].id}, nil, nil, 1) }
	clone()
	f.lsRefs("k")
	if len(r.Git.Upload) != 2 {
		t.Fatalf("%d cached responses, want 2 (ls-refs and the clone)", len(r.Git.Upload))
	}
	var first *byte
	for _, resp := range r.Git.Upload {
		if bytes.Contains(resp, []byte("packfile")) {
			first = &resp[0]
		}
	}
	clone()
	for _, resp := range r.Git.Upload {
		if bytes.Contains(resp, []byte("packfile")) && &resp[0] != first {
			t.Fatal("the cached clone response was rebuilt")
		}
	}
	// A fetch with haves is answered but never kept.
	f.fetch("k", []gitobj.OID{c1[0].id}, []gitobj.OID{c1[0].id}, nil, 0)
	if len(r.Git.Upload) != 2 {
		t.Fatal("a fetch with haves was cached")
	}
	// Any ref change empties the cache, and the next clone sees the new tip.
	c2 := commitOf(2, map[string]string{"a": "b"}, c1[0])
	f.mustPush("k", []update{{c1[0].id, c2[0].id, "refs/heads/main"}}, c2...)
	if len(r.Git.Upload) != 0 {
		t.Fatal("a push left cached responses behind")
	}
	if refs := f.lsRefs("k"); refs[0] != c2[0].id.String()+" HEAD symref-target:refs/heads/main" {
		t.Fatalf("stale ls-refs after a push: %q", refs)
	}
	// A repository deleted and created again under the same name starts with nothing cached and
	// nothing stored.
	f.st.DeleteRepo(r)
	f.create("k")
	if refs := f.lsRefs("k"); len(refs) != 1 || !strings.HasPrefix(refs[0], "unborn HEAD") {
		t.Fatalf("recreated repository: %q", refs)
	}
	f.mustPush("k", []update{{zero, c1[0].id, "refs/heads/main"}}, c1...)
	// The cache is bounded.
	for i := 0; i < 3*maxCachedResponses; i++ {
		f.lsRefs("k", "refs/heads/x"+strconv.Itoa(i))
	}
	if n := len(f.st.Repo("k").Git.Upload); n != maxCachedResponses {
		t.Fatalf("%d cached responses, want the cap %d", n, maxCachedResponses)
	}
}

func TestGzipRequest(t *testing.T) {
	f := newForge(t)
	c1 := f.seed("z", map[string]string{"a": "a"})
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	w.Write([]byte(pkt("command=ls-refs\n") + "0001" + pkt("peel\n") + pkt("symrefs\n") + "0000"))
	w.Close()
	res := f.do("POST", "/adaiversen/z.git/git-upload-pack", "content-encoding: gzip\r\ngit-protocol: version=2\r\n", z.Bytes())
	if lines := pktLines(t, res.body); res.status != 200 || len(lines) != 3 || !strings.HasPrefix(lines[1], c1[0].id.String()+" refs/heads/main") {
		t.Fatalf("gzip request: %d %q", res.status, res.body)
	}
}

// Objects made through the REST contents API are ordinary git objects: a clone gets them, and a
// push can build on them.
func TestStoreMadeCommitsOverGit(t *testing.T) {
	f := newForge(t)
	r := f.create("api")
	res, err := r.PutFile(store.FileWrite{Path: "config/token.css", Message: "Tidy issue pagination", Data: []byte("alpha\n")})
	if err != nil {
		t.Fatal(err)
	}
	tip := res.Commit.ID
	if refs := f.lsRefs("api"); refs[0] != tip.String()+" HEAD symref-target:refs/heads/main" {
		t.Fatalf("ls-refs after a contents write: %q", refs)
	}
	clone := f.fetch("api", []gitobj.OID{tip}, nil, nil, 0)
	if len(clone.objects) != 4 || string(clone.objects[res.Blob].data) != "alpha\n" {
		t.Fatalf("clone of an API-made commit: %d objects", len(clone.objects))
	}
	next := commitOf(9, map[string]string{"config/token.css": "alpha\n", "pushed": "p"}, object{id: tip})
	f.mustPush("api", []update{{tip, next[0].id, "refs/heads/main"}}, next...)
	if c, err := r.Contents("pushed", ""); err != nil || string(c.Data) != "p" {
		t.Fatalf("contents after a push: %v", err)
	}
}

// ---- benchmarks: the two git workloads, request for request ----

// benchChain builds the push workload's payloads: n commits, each with one 1 KiB blob in
// bench-data.txt, a one-entry tree and the previous commit as parent.
func benchChain(n int) (requests [][]byte, tips []gitobj.OID) {
	var prev gitobj.OID
	x := uint32(7)
	for i := 0; i < n; i++ {
		text := make([]byte, 1024)
		for j := range text {
			x = x*1664525 + 1013904223
			text[j] = "abcdefghijklmnopqrstuvwxyz \n"[x>>24%28]
		}
		b := mk(gitobj.TypeBlob, text)
		tree := mk(gitobj.TypeTree, gitobj.AppendTree(nil, []gitobj.Entry{{Mode: gitobj.ModeFile, Name: "bench-data.txt", ID: b.id}}))
		who := gitobj.Signature{Name: "Conformance", Email: "conformance@make.fun", When: int64(1700000000 + i)}
		c := gitobj.Commit{Tree: tree.id, Author: who, Committer: who, Message: "bench: push " + strconv.Itoa(i+1) + "\n"}
		if i > 0 {
			c.Parents = []gitobj.OID{prev}
		}
		commit := mk(gitobj.TypeCommit, gitobj.AppendCommit(nil, &c))
		body := pushBody([]update{{prev, commit.id, "refs/heads/main"}}, "report-status side-band-64k ofs-delta", packOf(b, tree, commit))
		requests = append(requests, []byte("POST /adaiversen/bench-push.git/git-receive-pack HTTP/1.1\r\nhost: 127.0.0.1:4000\r\nconnection: keep-alive\r\n"+
			"user-agent: makefun-git-client/1.0\r\naccept: application/x-git-receive-pack-result\r\nauthorization: Bearer "+token+"\r\n"+
			"content-type: application/x-git-receive-pack-request\r\ncontent-length: "+strconv.Itoa(len(body))+"\r\n"+
			"accept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n\r\n"+string(body)))
		prev = commit.id
		tips = append(tips, commit.id)
	}
	return requests, tips
}

const benchGetTail = " HTTP/1.1\r\nhost: 127.0.0.1:4000\r\nconnection: keep-alive\r\nuser-agent: makefun-git-client/1.0\r\naccept: */*\r\n" +
	"authorization: Bearer " + token + "\r\naccept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n"

// One push of the push workload: the receive-pack advertisement, then the POST with a three-object
// pack. Cost must not depend on how long the history already is.
func BenchmarkPush(b *testing.B) {
	const chain = 2000
	requests, _ := benchChain(chain)
	advert := []byte("GET /adaiversen/bench-push.git/info/refs?service=git-receive-pack" + benchGetTail + "\r\n")
	var f *forge
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%chain == 0 {
			b.StopTimer()
			f = newForge(b)
			f.create("bench-push")
			b.StartTimer()
		}
		if res := f.tester.Do(advert); len(res) < 150 {
			b.Fatalf("advertisement: %q", res)
		}
		res := f.tester.Do(requests[i%chain])
		if !bytes.HasSuffix(res, []byte("ok refs/heads/main\n00000000")) {
			b.Fatalf("push %d: %q", i, res)
		}
	}
}

// One clone of the clone workload: five requests against a 200-commit repository, depth 1.
func BenchmarkClone(b *testing.B) {
	f := newForge(b)
	f.create("bench-clone")
	// The setup push: 200 commits in one pack.
	var objs []object
	var prev object
	for i := 0; i < 200; i++ {
		var parents []object
		if i > 0 {
			parents = []object{prev}
		}
		c := commitOf(i, map[string]string{"history.txt": strings.Repeat(strconv.Itoa(i)+" history line\n", 30)}, parents...)
		objs = append(objs, c...)
		prev = c[0]
	}
	f.mustPush("bench-clone", []update{{zero, prev.id, "refs/heads/main"}}, objs...)

	post := func(body string) []byte {
		return []byte("POST /adaiversen/bench-clone.git/git-upload-pack HTTP/1.1\r\nhost: 127.0.0.1:4000\r\nconnection: keep-alive\r\n" +
			"user-agent: makefun-git-client/1.0\r\naccept: application/x-git-upload-pack-result\r\nauthorization: Bearer " + token + "\r\n" +
			"git-protocol: version=2\r\ncontent-type: application/x-git-upload-pack-request\r\ncontent-length: " + strconv.Itoa(len(body)) + "\r\n" +
			"accept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n\r\n" + body)
	}
	advert := []byte("GET /adaiversen/bench-clone.git/info/refs?service=git-upload-pack" + benchGetTail + "git-protocol: version=2\r\n\r\n")
	lsRefs := post(pkt("command=ls-refs\n") + "0001" + pkt("peel\n") + pkt("symrefs\n") + pkt("unborn\n") + "0000")
	fetch := post(pkt("command=fetch\n") + "0001" + pkt("thin-pack\n") + pkt("no-progress\n") + pkt("ofs-delta\n") +
		pkt("want "+prev.id.String()+"\n") + pkt("deepen 1\n") + pkt("done\n") + "0000")
	// Check the fetch once, then time the five requests.
	res := f.tester.Do(fetch)
	_, payload, _ := bytes.Cut(res, []byte("\r\n\r\n"))
	if got := f.parseFetch(bytes.Clone(payload), true); len(got.objects) != 3 || got.shallow[0] != "shallow "+prev.id.String() {
		b.Fatalf("clone fetch: %d objects, %q", len(got.objects), got.shallow)
	}
	b.Logf("fetch response: %d bytes", len(res))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.tester.Do(advert)
		f.tester.Do(advert)
		f.tester.Do(lsRefs)
		f.tester.Do(advert)
		if res := f.tester.Do(fetch); len(res) < 400 {
			b.Fatal("short fetch response")
		}
	}
}
