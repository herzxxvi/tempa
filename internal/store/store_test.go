package store

import (
	"strconv"
	"strings"
	"testing"

	"forge/internal/gitobj"
)

func status(err error) int {
	if err == nil {
		return 0
	}
	return StatusOf(err)
}

// seeded returns a store with one repository that has one commit on main holding config/token.css.
func seeded(t *testing.T, name string) (*Store, *Repo, FileResult) {
	t.Helper()
	s := New()
	r, err := s.CreateRepo(NewRepo{Name: name, Private: true, Description: "the tooling behind the thing", HasDescription: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.PutFile(FileWrite{Path: "config/token.css", Message: "Tidy issue pagination", Data: []byte("alpha\nbravo\n")})
	if err != nil {
		t.Fatal(err)
	}
	return s, r, res
}

func put(t *testing.T, r *Repo, branch, path, content, message string) FileResult {
	t.Helper()
	res, err := r.PutFile(FileWrite{Path: path, Message: message, Data: []byte(content), Branch: branch})
	if err != nil {
		t.Fatalf("PutFile %s on %s: %v", path, branch, err)
	}
	return res
}

func TestRepositories(t *testing.T) {
	s := New()
	r, err := s.CreateRepo(NewRepo{Name: "Amber-Thistle", Private: true})
	if err != nil || r.ID != 1 || r.DefaultBranch != "main" || !r.IsEmpty() || r.Head() != nil || r.HasDescription || !r.HasIssues {
		t.Fatalf("CreateRepo = %+v, %v", r, err)
	}
	if r.NodeID != "R_UmVwb3NpdG9yeTox" || r.FullName() != "adaiversen/Amber-Thistle" || r.HeadRef() != "refs/heads/main" {
		t.Fatalf("identity: %s %s %s", r.NodeID, r.FullName(), r.HeadRef())
	}
	// A duplicate, in any case, is the "already exists" 422 and leaves the repository alone.
	for _, name := range []string{"Amber-Thistle", "amber-thistle"} {
		_, err := s.CreateRepo(NewRepo{Name: name})
		e, ok := err.(*Error)
		if !ok || e.Status != 422 || !strings.Contains(e.Detail, "already exists") || e.Message != "Repository creation failed." {
			t.Fatalf("duplicate create: %v", err)
		}
	}
	for _, bad := range []string{"", "has space", "a/b", ".", "..", "x.git", strings.Repeat("n", 101)} {
		if _, err := s.CreateRepo(NewRepo{Name: bad}); status(err) != 422 {
			t.Errorf("CreateRepo(%q) = %v", bad, err)
		}
	}
	if s.Repo("Amber-Thistle") != r || s.Repo("amber-thistle") != r || s.RepoB([]byte("AMBER-thistle")) != r || s.Repo("other") != nil {
		t.Fatal("lookup by name")
	}
	if s.Lookup([]byte("AdaIversen"), []byte("Amber-Thistle")) != r || s.Lookup([]byte("olgabauer"), []byte("Amber-Thistle")) != nil {
		t.Fatal("lookup by owner and name")
	}
	second, _ := s.CreateRepo(NewRepo{Name: "second"})
	if len(s.Repos()) != 2 || s.Repos()[0] != r || s.Repos()[1] != second || s.RepoByID(2) != second {
		t.Fatal("repository list order")
	}

	desc := "updated"
	if err := s.UpdateRepo(r, RepoPatch{Description: &desc}); err != nil || r.Description != "updated" || !r.HasDescription {
		t.Fatalf("UpdateRepo description: %v", err)
	}
	taken := "SECOND"
	if err := s.UpdateRepo(r, RepoPatch{Name: &taken}); status(err) != 422 {
		t.Fatalf("rename onto an existing name: %v", err)
	}
	renamed := "renamed"
	gen := r.JSONGen
	if err := s.UpdateRepo(r, RepoPatch{Name: &renamed}); err != nil || s.Repo("renamed") != r || s.Repo("Amber-Thistle") != nil || r.JSONGen == gen {
		t.Fatalf("rename: %v", err)
	}

	// Delete: gone by name and id; the name is free again and the new repository starts clean.
	put(t, second, "", "f", "x", "m")
	is, _ := second.CreateIssue(NewIssue{Title: "t"})
	s.DeleteRepo(second)
	if s.Repo("second") != nil || s.RepoByID(2) != nil || s.IssueByID(is.ID) != nil || len(s.Repos()) != 1 {
		t.Fatal("deleted repository is still reachable")
	}
	again, err := s.CreateRepo(NewRepo{Name: "second"})
	if err != nil || again.ID != 3 || !again.IsEmpty() || again.Objects.Len() != 0 || len(again.Issues) != 0 {
		t.Fatalf("recreated repository: %+v %v", again, err)
	}
	if n, _ := again.CreateIssue(NewIssue{Title: "t"}); n.Number != 1 || n.ID == is.ID {
		t.Fatalf("recreated repository must number from 1 with new ids: %+v", n)
	}
}

func TestContents(t *testing.T) {
	s, r, first := seeded(t, "contents")
	if !first.Created || first.Branch != "main" || first.Size != 12 || len(first.Commit.Commit.Parents) != 0 {
		t.Fatalf("first write: %+v", first)
	}
	if first.Blob != gitobj.NewDB(&gitobj.Arena{}).ID(gitobj.TypeBlob, []byte("alpha\nbravo\n")) {
		t.Fatal("content sha is not the git blob id")
	}
	if tip, ok := r.Branch("main"); !ok || tip != first.Commit.ID || r.Head() != first.Commit || r.IsEmpty() {
		t.Fatal("first write must create main")
	}
	c, err := gitobj.ParseCommit(first.Commit.Data)
	if err != nil || c.Message != "Tidy issue pagination" || c.Author.Name != ViewerName || c.Author.Email != ViewerEmail || c.Author.When <= Epoch || c.Author.When != s.Now() {
		t.Fatalf("commit: %+v %v", c, err)
	}

	// Read back: file, directory, root, other refs.
	got, err := r.Contents("config/token.css", "")
	if err != nil || got.IsDir || string(got.Data) != "alpha\nbravo\n" || got.ID != first.Blob || got.Mode != gitobj.ModeFile {
		t.Fatalf("Contents file: %+v %v", got, err)
	}
	put(t, r, "", "config/one.txt", "1", "one")
	put(t, r, "", "zeta", "z", "zeta")
	dir, err := r.Contents("config", "main")
	if err != nil || !dir.IsDir || len(dir.Entries) != 2 || dir.Entries[0].Name != "one.txt" || dir.Entries[1].Name != "token.css" {
		t.Fatalf("Contents dir: %+v %v", dir, err)
	}
	if root, err := r.Contents("", ""); err != nil || len(root.Entries) != 2 || !root.Entries[0].IsDir() || root.Entries[1].Name != "zeta" {
		t.Fatalf("Contents root: %+v %v", root, err)
	}
	if old, err := r.Contents("config", first.Commit.ID.String()); err != nil || len(old.Entries) != 1 {
		t.Fatalf("Contents at a commit id: %+v %v", old, err)
	}
	if _, err := r.Contents("config/nope.css", ""); err != ErrNotFound {
		t.Fatalf("missing path: %v", err)
	}
	if _, err := r.Contents("zeta", "no-such-branch"); status(err) != 404 || !strings.Contains(err.Error(), "No commit found for the ref no-such-branch") {
		t.Fatalf("unknown ref: %v", err)
	}

	// Update rules: sha required, sha must be the CURRENT blob, new paths ignore sha.
	if _, err := r.PutFile(FileWrite{Path: "config/token.css", Message: "m", Data: []byte("x")}); status(err) != 422 || !strings.Contains(err.Error(), `"sha" wasn't supplied`) {
		t.Fatalf("update without sha: %v", err)
	}
	upd, err := r.PutFile(FileWrite{Path: "config/token.css", Message: "m", Data: []byte("charlie\n"), SHA: strings.ToUpper(first.Blob.String())})
	if err != nil || upd.Created || upd.Blob == first.Blob {
		t.Fatalf("update with the current sha: %+v %v", upd, err)
	}
	if _, err := r.PutFile(FileWrite{Path: "config/token.css", Message: "m", Data: []byte("alpha\nbravo\n"), SHA: first.Blob.String()}); status(err) != 409 || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("update with a stale sha: %v", err)
	}
	if res, err := r.PutFile(FileWrite{Path: "brand/new", Message: "m", Data: nil, SHA: "ignored"}); err != nil || !res.Created || res.Size != 0 {
		t.Fatalf("new path with a sha: %+v %v", res, err)
	}
	for _, bad := range []FileWrite{
		{Path: "ok", Data: []byte("x")},                      // no message
		{Path: "", Message: "m"},                             // no path
		{Path: "a/../b", Message: "m"},                       // dot-dot
		{Path: "/lead", Message: "m"},                        // leading slash
		{Path: "config", Message: "m"},                       // a directory
		{Path: "zeta/under-a-file", Message: "m"},            // through a file
		{Path: "f", Message: "m", Branch: "bad branch name"}, // unusable branch name
		{Path: "f", Message: "m", Branch: "double..dot"},     //
	} {
		if _, err := r.PutFile(bad); status(err) != 422 {
			t.Errorf("PutFile(%+v) = %v, want 422", bad, err)
		}
	}

	// Branches: a write with a branch lands there only; a missing branch is created from main.
	tip, _ := r.Branch("main")
	side := put(t, r, "develop", "side.txt", "s", "on develop")
	if side.Branch != "develop" || len(side.Commit.Commit.Parents) != 1 || side.Commit.Commit.Parents[0] != tip {
		t.Fatalf("write to a new branch: %+v", side)
	}
	if now, _ := r.Branch("main"); now != tip {
		t.Fatal("a write to another branch moved main")
	}
	if _, err := r.Contents("side.txt", ""); err != ErrNotFound {
		t.Fatal("a file on another branch is visible on the default branch")
	}
	if c, err := r.Contents("side.txt", "develop"); err != nil || string(c.Data) != "s" {
		t.Fatalf("Contents on a branch: %v", err)
	}

	// Delete: sha rules, then pruning of emptied directories.
	if _, err := r.DeleteFile(FileWrite{Path: "config/one.txt", Message: "m"}); status(err) != 422 {
		t.Fatalf("delete without sha: %v", err)
	}
	if _, err := r.DeleteFile(FileWrite{Path: "config/one.txt", Message: "m", SHA: first.Blob.String()}); status(err) != 409 {
		t.Fatalf("delete with a wrong sha: %v", err)
	}
	if _, err := r.DeleteFile(FileWrite{Path: "config/none", Message: "m", SHA: first.Blob.String()}); err != ErrNotFound {
		t.Fatalf("delete a missing file: %v", err)
	}
	if _, err := r.DeleteFile(FileWrite{Path: "zeta", Message: "m", SHA: "x", Branch: "nope"}); status(err) != 404 {
		t.Fatalf("delete on a missing branch: %v", err)
	}
	one, _ := r.Contents("config/one.txt", "")
	del, err := r.DeleteFile(FileWrite{Path: "config/one.txt", Message: "drop one", SHA: one.ID.String()})
	if err != nil || del.Commit.Commit.Parents[0] != tip {
		t.Fatalf("delete: %+v %v", del, err)
	}
	if _, err := r.Contents("config/one.txt", ""); err != ErrNotFound {
		t.Fatal("deleted file still readable")
	}
}

func TestDeleteLastFileLeavesEmptyTree(t *testing.T) {
	_, r, first := seeded(t, "last-file")
	del, err := r.DeleteFile(FileWrite{Path: "config/token.css", Message: "Tidy issue pagination", SHA: first.Blob.String()})
	if err != nil || del.Commit.Commit.Tree != gitobj.EmptyTree || !r.Objects.Has(gitobj.EmptyTree) {
		t.Fatalf("delete of the only file: %+v %v", del, err)
	}
	if _, err := r.Contents("config/token.css", ""); err != ErrNotFound {
		t.Fatalf("after delete: %v", err)
	}
	if root, err := r.Contents("", ""); err != nil || !root.IsDir || len(root.Entries) != 0 {
		t.Fatalf("root of an emptied repository: %+v %v", root, err)
	}
}

func TestEmptyRepository(t *testing.T) {
	s := New()
	r, _ := s.CreateRepo(NewRepo{Name: "empty"})
	if _, err := r.Contents("", ""); status(err) != 404 || err.Error() != MsgEmptyRepo {
		t.Fatalf("contents of an empty repository: %v", err)
	}
	if _, _, err := r.Log("", 0, 30, nil); status(err) != 409 || err.Error() != MsgEmptyGit {
		t.Fatalf("log of an empty repository: %v", err)
	}
	if len(r.Branches()) != 0 || r.Resolve("") != nil || r.Resolve("main") != nil {
		t.Fatal("an empty repository has no branches")
	}
	// The very first write may name another branch: that branch is created, main stays unborn.
	res := put(t, r, "trunk", "f", "x", "m")
	if _, ok := r.Branch("trunk"); !ok || res.Branch != "trunk" || r.Head() != nil || r.DefaultBranch != "main" {
		t.Fatal("first write to a named branch")
	}
}

func TestRefsAndLog(t *testing.T) {
	_, r, first := seeded(t, "refs")
	initial := first.Commit.ID
	if err := r.CreateRef("refs/heads/develop", initial); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateRef("refs/heads/develop", initial); status(err) != 422 || err.Error() != "Reference already exists" {
		t.Fatalf("duplicate ref: %v", err)
	}
	if err := r.CreateRef("refs/heads/ghost", gitobj.OID{9}); status(err) != 422 || err.Error() != "Object does not exist" {
		t.Fatalf("ref to a missing object: %v", err)
	}
	if err := r.CreateRef("refs/heads/blob", first.Blob); status(err) != 422 {
		t.Fatalf("branch at a blob: %v", err)
	}
	for _, bad := range []string{"heads/x", "refs/x", "refs/heads/", "refs/heads/a..b", "refs/heads/a b", "refs/heads/x.lock", "refs/heads/.hidden"} {
		if err := r.CreateRef(bad, initial); status(err) != 422 || err.Error() != "Reference name is invalid" {
			t.Errorf("CreateRef(%q) = %v", bad, err)
		}
	}
	if err := r.CreateRef("refs/heads/feature/with/slashes", initial); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateRef("refs/tags/v1", initial); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range r.Branches() {
		names = append(names, b.Name)
	}
	if strings.Join(names, ",") != "refs/heads/develop,refs/heads/feature/with/slashes,refs/heads/main" || len(r.Tags()) != 1 || len(r.Refs()) != 4 {
		t.Fatalf("branches = %v", names)
	}
	if id, ok := r.Branch("feature/with/slashes"); !ok || id != initial {
		t.Fatal("branch with slashes")
	}

	// A commit on develop shows in develop's log only; the message comes back exactly.
	gen := r.Gen
	c2 := put(t, r, "develop", "config/token-commits.css", "x\n", "Tidy issue pagination (commits)").Commit
	if r.Gen == gen {
		t.Fatal("a ref change must bump the generation")
	}
	log, more, err := r.Log("develop", 0, 30, nil)
	if err != nil || more || len(log) != 2 || log[0] != c2 || log[1].ID != initial {
		t.Fatalf("log of develop: %v %v %v", log, more, err)
	}
	if pc, _ := gitobj.ParseCommit(c2.Data); pc.Message != "Tidy issue pagination (commits)" {
		t.Fatalf("message = %q", pc.Message)
	}
	if log, _, _ := r.Log("", 0, 30, nil); len(log) != 1 || log[0].ID != initial {
		t.Fatal("the default log must hold the default branch only")
	}
	if page, more, _ := r.Log("develop", 0, 1, nil); len(page) != 1 || !more || page[0] != c2 {
		t.Fatal("log page 1")
	}
	if page, more, _ := r.Log("develop", 1, 1, nil); len(page) != 1 || more || page[0].ID != initial {
		t.Fatal("log page 2")
	}
	if _, _, err := r.Log("nope", 0, 30, nil); status(err) != 404 || err.Error() != "No commit found for SHA: nope" {
		t.Fatalf("log of an unknown ref: %v", err)
	}
	for _, rev := range []string{"develop", "refs/heads/develop", "heads/develop", c2.ID.String()} {
		if r.Resolve(rev) != c2 {
			t.Errorf("Resolve(%q) did not find the commit", rev)
		}
	}
	if o := r.Resolve("v1"); o == nil || o.ID != initial {
		t.Error("Resolve of a tag name")
	}

	// UpdateRef: forward only unless forced; DeleteRef.
	if err := r.UpdateRef("refs/heads/main", c2.ID, false); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	if err := r.UpdateRef("refs/heads/main", initial, false); status(err) != 422 || err.Error() != "Update is not a fast forward" {
		t.Fatalf("rewind without force: %v", err)
	}
	if err := r.UpdateRef("refs/heads/main", initial, true); err != nil {
		t.Fatalf("forced rewind: %v", err)
	}
	if err := r.UpdateRef("refs/heads/none", initial, true); status(err) != 422 {
		t.Fatalf("update of a missing ref: %v", err)
	}
	if err := r.DeleteRef("refs/tags/v1"); err != nil || len(r.Tags()) != 0 {
		t.Fatalf("delete ref: %v", err)
	}
	if err := r.DeleteRef("refs/tags/v1"); status(err) != 422 {
		t.Fatalf("delete of a missing ref: %v", err)
	}
}

func TestLabelsIssuesComments(t *testing.T) {
	s, r, _ := seeded(t, "issues")
	bug, err := r.CreateLabel("bug", "#EDEDED", "", false)
	if err != nil || bug.Color != "ededed" || bug.ID != 1 {
		t.Fatalf("CreateLabel: %+v %v", bug, err)
	}
	if _, err := r.CreateLabel("BUG", "", "", false); status(err) != 422 {
		t.Fatalf("duplicate label: %v", err)
	}
	if _, err := r.CreateLabel("", "", "", false); status(err) != 422 {
		t.Fatalf("label without a name: %v", err)
	}

	a, err := r.CreateIssue(NewIssue{Title: "Incorrect GraphQL node ids", Body: "body <b> & \"quotes\"\n", Labels: []string{"bug"}})
	if err != nil || a.Number != 1 || a.Closed || a.Body != "body <b> & \"quotes\"\n" || len(a.Labels) != 1 || a.Labels[0] != bug {
		t.Fatalf("CreateIssue: %+v %v", a, err)
	}
	b, _ := r.CreateIssue(NewIssue{Title: "second"})
	if b.Number != 2 || b.Body != "" || b.Labels != nil || b.ID != a.ID+1 || b.CreatedAt <= a.CreatedAt {
		t.Fatalf("second issue: %+v", b)
	}
	before := len(r.Issues)
	if _, err := r.CreateIssue(NewIssue{Title: "t", Labels: []string{"bug-not-a-label"}}); status(err) != 422 || err.(*Error).Value != "bug-not-a-label" || len(r.Issues) != before || len(r.Labels) != 1 {
		t.Fatalf("unknown label: %v", err)
	}
	if _, err := r.CreateIssue(NewIssue{}); status(err) != 422 || err.(*Error).Field != "title" {
		t.Fatalf("missing title: %v", err)
	}
	if r.Issue(1) != a || r.Issue(2) != b || r.Issue(3) != nil || r.Issue(0) != nil || r.Issue(999999999) != nil {
		t.Fatal("Issue lookup")
	}
	if s.IssueByID(a.ID) != a || s.Node(NodeID(KindIssue, b.ID)) != b || s.Node("I_bogus") != nil || s.Node("17") != nil {
		t.Fatal("lookup by id")
	}

	// Close, reopen.
	closed, open := "closed", "open"
	if err := r.UpdateIssue(b, IssuePatch{State: &closed}); err != nil || !b.Closed || b.ClosedAt == 0 || b.StateReason != "completed" || r.OpenIssues != 1 {
		t.Fatalf("close: %+v %v", b, err)
	}
	at := b.ClosedAt
	if err := r.UpdateIssue(b, IssuePatch{State: &closed}); err != nil || b.ClosedAt != at || r.OpenIssues != 1 {
		t.Fatal("closing twice must change nothing")
	}
	bad := "merged"
	if err := r.UpdateIssue(b, IssuePatch{State: &bad}); status(err) != 422 {
		t.Fatalf("bad state: %v", err)
	}
	if err := r.UpdateIssue(b, IssuePatch{State: &open}); err != nil || b.Closed || b.ClosedAt != 0 || b.StateReason != "reopened" || r.OpenIssues != 2 {
		t.Fatalf("reopen: %+v %v", b, err)
	}
	r.UpdateIssue(b, IssuePatch{State: &closed})

	c, _ := r.CreateIssue(NewIssue{Title: "third"})
	numbers := func(q IssueQuery) string {
		page, total := r.ListIssues(q, nil)
		out := strconv.Itoa(total) + ":"
		for _, is := range page {
			out += strconv.FormatInt(is.Number, 10) + " "
		}
		return out
	}
	for q, want := range map[*IssueQuery]string{
		{}:                                     "2:3 1 ",
		{Asc: true}:                            "2:1 3 ",
		{State: StateClosed}:                   "1:2 ",
		{State: StateAll}:                      "3:3 2 1 ",
		{State: StateAll, Asc: true}:           "3:1 2 3 ",
		{State: StateAll, Limit: 2}:            "3:3 2 ",
		{State: StateAll, Limit: 2, Offset: 2}: "3:1 ",
		{State: StateAll, Offset: 5}:           "3:",
		{State: StateAll, Labels: []string{"BUG"}}: "1:1 ",
		{State: StateAll, Sort: SortUpdated}:       "3:3 2 1 ",
		{State: StateAll, Sort: SortComments}:      "3:3 2 1 ",
	} {
		if got := numbers(*q); got != want {
			t.Errorf("ListIssues(%+v) = %q, want %q", *q, got, want)
		}
	}

	// Comments: creation order, on issues only by number, counted on the issue.
	k1, err := r.AddComment(1, "first comment")
	if err != nil || k1.ID != 1 || k1.Number != 1 {
		t.Fatalf("AddComment: %+v %v", k1, err)
	}
	k2, _ := r.AddComment(1, "second comment")
	if list, ok := r.Comments(1); !ok || len(list) != 2 || list[0] != k1 || list[1] != k2 || len(a.Comments) != 2 {
		t.Fatal("comment list")
	}
	if list, ok := r.Comments(3); !ok || len(list) != 0 {
		t.Fatal("an issue without comments has an empty list")
	}
	if _, ok := r.Comments(9); ok {
		t.Fatal("comments of an unknown issue")
	}
	if _, err := r.AddComment(9, "x"); err != ErrNotFound {
		t.Fatalf("comment on an unknown issue: %v", err)
	}
	if _, err := r.AddComment(1, ""); status(err) != 422 {
		t.Fatalf("empty comment: %v", err)
	}
	if got := numbers(IssueQuery{State: StateAll, Sort: SortComments}); got != "3:1 3 2 " {
		t.Errorf("sort by comments = %q", got)
	}
	if got := numbers(IssueQuery{State: StateAll, Sort: SortUpdated, Asc: true}); got != "3:2 3 1 " {
		t.Errorf("sort by updated asc = %q", got)
	}
	_ = c

	// Label edits follow onto issues; deleting a label takes it off them.
	name := "defect"
	if err := r.UpdateLabel(bug, LabelPatch{Name: &name}); err != nil || a.Labels[0].Name != "defect" || r.Label("bug") != nil {
		t.Fatalf("rename label: %v", err)
	}
	r.DeleteLabel(bug)
	if len(a.Labels) != 0 || len(r.Labels) != 0 || s.LabelByID(bug.ID) != nil {
		t.Fatal("delete label")
	}

	// Issues can be switched off.
	off := false
	s.UpdateRepo(r, RepoPatch{HasIssues: &off})
	if _, err := r.CreateIssue(NewIssue{Title: "t"}); status(err) != 410 {
		t.Fatalf("issues disabled: %v", err)
	}
}

// pullFixture is the REST checks' pull fixture: a branch from main's tip with one new file.
func pullFixture(t *testing.T, r *Repo, branch, path, content string) *Pull {
	t.Helper()
	tip, _ := r.Branch("main")
	if _, ok := r.Branch(branch); !ok {
		if err := r.CreateRef("refs/heads/"+branch, tip); err != nil {
			t.Fatal(err)
		}
	}
	put(t, r, branch, path, content, "Tidy issue pagination ("+branch+")")
	p, err := r.CreatePull(NewPull{Title: "Improve MCP tool schemas", Body: "It works.", Head: branch, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPullRequests(t *testing.T) {
	s, r, first := seeded(t, "pulls")
	initial := first.Commit.ID
	issue, _ := r.CreateIssue(NewIssue{Title: "an issue first"})

	p := pullFixture(t, r, "develop", "config/token-new.css", "one\ntwo\nthree\n")
	if p.Number != issue.Number+1 || p.Closed || p.Merged || p.HeadRef != "develop" || p.BaseRef != "main" || p.Body != "It works." {
		t.Fatalf("CreatePull: %+v", p)
	}
	if r.Pull(p.Number) != p || r.Pull(issue.Number) != nil || r.Issue(p.Number) != nil || r.Pull(999999999) != nil {
		t.Fatal("issues and pull requests share numbers but not lookups")
	}
	if s.Node(NodeID(KindPull, p.ID)) != p {
		t.Fatal("pull request by node id")
	}
	head, _ := r.Branch("develop")
	info := r.PullInfo(p)
	if info.Head != head || info.Base != initial || info.MergeBase != initial || info.Mergeable != MergeClean || info.Commits != 1 {
		t.Fatalf("PullInfo: %+v", info)
	}
	if len(info.Files) != 1 || info.Files[0] != (FileChange{Path: "config/token-new.css", Status: "added", Additions: 3, Blob: info.Files[0].Blob}) || info.Additions != 3 {
		t.Fatalf("files: %+v", info.Files)
	}
	if r.PullInfo(p) != info {
		t.Fatal("PullInfo must be cached while the tips stand still")
	}
	// A second commit on the head branch: both files listed, head moves.
	put(t, r, "develop", "config/second.css", "x", "second")
	info = r.PullInfo(p)
	if newHead, _ := r.Branch("develop"); info.Head != newHead || len(info.Files) != 2 || info.Files[0].Path != "config/second.css" || info.Commits != 2 {
		t.Fatalf("PullInfo after a second commit: %+v", info)
	}

	// Validation.
	for _, tc := range []struct {
		in    NewPull
		field string
	}{
		{NewPull{Head: "develop", Base: "main"}, "title"},
		{NewPull{Title: "t", Base: "main"}, "head"},
		{NewPull{Title: "t", Head: "develop"}, "base"},
		{NewPull{Title: "t", Head: "ghost", Base: "main"}, "head"},
		{NewPull{Title: "t", Head: "develop", Base: "ghost"}, "base"},
		{NewPull{Title: "t", Head: "main", Base: "main"}, "head"},
	} {
		if _, err := r.CreatePull(tc.in); status(err) != 422 || err.(*Error).Field != tc.field {
			t.Errorf("CreatePull(%+v) = %v, want 422 on %s", tc.in, err, tc.field)
		}
	}
	if _, err := r.CreatePull(NewPull{Title: "again", Head: "adaiversen:develop", Base: "main"}); status(err) != 422 || !strings.Contains(err.Error(), "A pull request already exists for adaiversen:develop.") {
		t.Fatalf("duplicate pull request: %v", err)
	}

	// Merge (squash): one commit on main whose parent is the old tip, carrying the head's files.
	if _, err := r.MergePull(p, MergeOptions{Method: "octopus"}); status(err) != 422 {
		t.Fatalf("unknown merge method: %v", err)
	}
	if _, err := r.MergePull(p, MergeOptions{Method: "squash", SHA: initial.String()}); status(err) != 409 {
		t.Fatalf("merge with a stale head sha: %v", err)
	}
	open := r.OpenIssues
	commit, err := r.MergePull(p, MergeOptions{Method: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	mc, _ := gitobj.ParseCommit(commit.Data)
	if len(mc.Parents) != 1 || mc.Parents[0] != initial || !strings.HasPrefix(mc.Message, "Improve MCP tool schemas (#2)\n\n* ") {
		t.Fatalf("squash commit: %+v", mc)
	}
	if tip, _ := r.Branch("main"); tip != commit.ID {
		t.Fatal("the base branch did not move to the merge commit")
	}
	if c, err := r.Contents("config/token-new.css", "main"); err != nil || string(c.Data) != "one\ntwo\nthree\n" {
		t.Fatalf("merged content on main: %v", err)
	}
	if !p.Closed || !p.Merged || p.MergedAt == 0 || p.ClosedAt != p.MergedAt || p.MergeCommit != commit.ID || p.Base != initial || r.OpenIssues != open-1 {
		t.Fatalf("merged pull request: %+v", p)
	}
	if _, ok := r.Branch("develop"); !ok {
		t.Fatal("the head branch must survive the merge")
	}
	if after := r.PullInfo(p); after.Mergeable != MergeUnknown || len(after.Files) != 2 || after.Base != initial {
		t.Fatalf("PullInfo of a merged pull request: %+v", after)
	}
	if _, err := r.MergePull(p, MergeOptions{Method: "squash"}); status(err) != 405 || err.Error() != MsgNotMergeable {
		t.Fatalf("merging twice: %v", err)
	}
	if log, _, _ := r.Log("main", 0, 0, nil); len(log) != 2 || log[0] != commit {
		t.Fatal("the merge commit must head main's log")
	}

	// A second pull request from the OLD tip that touches another file still merges (true 3-way).
	if err := r.CreateRef("refs/heads/develop-b", initial); err != nil {
		t.Fatal(err)
	}
	put(t, r, "develop-b", "config/b.css", "b\n", "b")
	pb, err := r.CreatePull(NewPull{Title: "b", Head: "develop-b", Base: "main"})
	if err != nil || pb.Body != "" {
		t.Fatalf("second pull request: %v", err)
	}
	if got := r.PullInfo(pb); got.Mergeable != MergeClean || got.MergeBase != initial || len(got.Files) != 1 {
		t.Fatalf("PullInfo of a diverged pull request: %+v", got)
	}

	list := func(q PullQuery) string {
		page, total := r.ListPulls(q, nil)
		out := strconv.Itoa(total) + ":"
		for _, x := range page {
			out += strconv.FormatInt(x.Number, 10) + " "
		}
		return out
	}
	for q, want := range map[PullQuery]string{
		{}:                                 "1:3 ",
		{State: StateAll}:                  "2:3 2 ",
		{State: StateAll, Asc: true}:       "2:2 3 ",
		{State: StateClosed}:               "1:2 ",
		{State: StateAll, Head: "develop"}: "1:2 ",
		{State: StateAll, Head: "adaiversen:develop-b", Base: "main"}: "1:3 ",
		{State: StateAll, Limit: 1, Offset: 1}:                        "2:2 ",
	} {
		if got := list(q); got != want {
			t.Errorf("ListPulls(%+v) = %q, want %q", q, got, want)
		}
	}

	if mm, err := r.MergePull(pb, MergeOptions{}); err != nil {
		t.Fatalf("merge commit: %v", err)
	} else if c, _ := gitobj.ParseCommit(mm.Data); len(c.Parents) != 2 || !strings.HasPrefix(c.Message, "Merge pull request #3 from adaiversen/develop-b\n\nb") {
		t.Fatalf("merge commit: %+v", c)
	}

	// Close and reopen without merging; a merged one cannot be reopened.
	pc := pullFixture(t, r, "develop-c", "c", "c")
	closed, opened := "closed", "open"
	if err := r.UpdatePull(pc, PullPatch{State: &closed}); err != nil || !pc.Closed || pc.Merged || r.PullInfo(pc).Mergeable != MergeUnknown {
		t.Fatalf("close a pull request: %v", err)
	}
	if _, err := r.MergePull(pc, MergeOptions{}); status(err) != 405 {
		t.Fatalf("merge a closed pull request: %v", err)
	}
	if err := r.UpdatePull(pc, PullPatch{State: &opened}); err != nil || pc.Closed {
		t.Fatalf("reopen: %v", err)
	}
	if err := r.UpdatePull(p, PullPatch{State: &opened}); status(err) != 422 {
		t.Fatalf("reopen a merged pull request: %v", err)
	}
	if k, err := r.AddComment(pc.Number, "on a pull request"); err != nil || len(pc.Comments) != 1 || k.Number != pc.Number {
		t.Fatalf("comment on a pull request: %v", err)
	}
}

// The conformance scenario: two branches from one base change the same file differently. Both pull
// requests open fine; the first merges; only then is the second in conflict, and nothing is written.
func TestMergeConflictIsDecidedAtMergeTime(t *testing.T) {
	_, r, first := seeded(t, "conflict")
	initial := first.Commit.ID
	var pulls []*Pull
	for _, side := range []string{"a", "b"} {
		branch := "develop-conflict-" + side
		if err := r.CreateRef("refs/heads/"+branch, initial); err != nil {
			t.Fatal(err)
		}
		if _, err := r.PutFile(FileWrite{Path: "config/token.css", Message: "conflict " + side, Data: []byte("changed (" + side + ")"), Branch: branch, SHA: first.Blob.String()}); err != nil {
			t.Fatal(err)
		}
		p, err := r.CreatePull(NewPull{Title: "Improve MCP tool schemas " + side, Head: branch, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		pulls = append(pulls, p)
	}
	if r.PullInfo(pulls[0]).Mergeable != MergeClean || r.PullInfo(pulls[1]).Mergeable != MergeClean {
		t.Fatal("both pull requests are clean before the first merge")
	}
	if _, err := r.MergePull(pulls[0], MergeOptions{Method: "squash"}); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if r.PullInfo(pulls[1]).Mergeable != MergeConflicting {
		t.Fatal("the second pull request must now show a conflict")
	}
	tip, _ := r.Branch("main")
	objects := r.Objects.Len()
	if _, err := r.MergePull(pulls[1], MergeOptions{Method: "squash"}); status(err) != 405 || err.Error() != MsgNotMergeable {
		t.Fatalf("second merge: %v", err)
	}
	if now, _ := r.Branch("main"); now != tip || r.Objects.Len() != objects || pulls[1].Closed {
		t.Fatal("a refused merge must change nothing")
	}
}

func TestNodeIDs(t *testing.T) {
	if ViewerNodeID != "U_VXNlcjoxMDAw" || NodeID(KindRepo, 17) != "R_UmVwb3NpdG9yeToxNw" {
		t.Fatalf("node ids: %s %s", ViewerNodeID, NodeID(KindRepo, 17))
	}
	for kind := KindUser; kind <= KindLabel; kind++ {
		id := NodeID(kind, 123456)
		if k, n, ok := ParseNodeID(id); !ok || k != kind || n != 123456 {
			t.Errorf("round trip of %s failed", id)
		}
	}
	for _, bad := range []string{"", "17", "R_", "R_!!!", "X_UmVwb3NpdG9yeToxNw", "I_UmVwb3NpdG9yeToxNw", NodeID(KindRepo, 1) + "x"} {
		if _, _, ok := ParseNodeID(bad); ok {
			t.Errorf("ParseNodeID(%q) accepted", bad)
		}
	}
	s := New()
	if _, ok := s.Node(ViewerNodeID).(Viewer); !ok {
		t.Fatal("the viewer's node id must resolve")
	}
	if !IsViewer([]byte("ADAiversen")) || IsViewer([]byte("olgabauer")) || IsViewer(nil) {
		t.Fatal("IsViewer")
	}
}

// Two stores fed the same operations must agree on every id, timestamp and SHA.
func TestDeterminism(t *testing.T) {
	run := func() string {
		s, r, first := seeded(t, "same")
		r.CreateLabel("bug", "", "", false)
		is, _ := r.CreateIssue(NewIssue{Title: "t", Labels: []string{"bug"}})
		p := pullFixture(t, r, "develop", "x", "y")
		commit, _ := r.MergePull(p, MergeOptions{Method: "squash"})
		k, _ := r.AddComment(is.Number, "c")
		return strings.Join([]string{first.Commit.ID.String(), commit.ID.String(), strconv.FormatInt(is.ID, 10), strconv.FormatInt(is.CreatedAt, 10),
			strconv.FormatInt(p.MergedAt, 10), strconv.FormatInt(k.ID, 10), strconv.FormatInt(s.Now(), 10), r.NodeID}, " ")
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("two identical runs differ:\n%s\n%s", a, b)
	}
}

func BenchmarkCreateIssue(b *testing.B) {
	s := New()
	r, _ := s.CreateRepo(NewRepo{Name: "bench-write"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.CreateIssue(NewIssue{Title: "Incorrect GraphQL node ids [bench 1]", Body: "A minimal case is attached."}); err != nil {
			b.Fatal(err)
		}
	}
}
