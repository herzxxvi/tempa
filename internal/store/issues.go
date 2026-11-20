package store

import (
	"sort"
	"strings"
)

// Label is a repository label.
type Label struct {
	ID             int64
	Name           string
	Color          string // six hex digits, no '#'
	Description    string
	HasDescription bool
	Repo           *Repo
}

// Comment is a comment on an issue or a pull request.
type Comment struct {
	ID        int64
	Number    int64 // the issue or pull request it belongs to
	Body      string
	CreatedAt int64
	UpdatedAt int64
	Repo      *Repo
}

// Issue is an issue (never a pull request; those are Pull).
type Issue struct {
	ID          int64
	Number      int64
	Title       string
	Body        string // "" when the issue was created without one; never rendered as null
	Closed      bool
	StateReason string // "", "completed", "not_planned" or "reopened"
	Labels      []*Label
	Comments    []*Comment // creation order
	CreatedAt   int64
	UpdatedAt   int64
	ClosedAt    int64 // 0 while open
	Repo        *Repo

	// Pre-rendered JSON reply tail (see httpd.AppendTail), valid for wireHost and wireGen only.
	wire     []byte
	wireHost string
	wireGen  uint64
}

// Wire returns the cached JSON reply tail of the issue for the given request host, or nil when
// there is none or it is stale. The renderer in internal/render is the only writer.
func (is *Issue) Wire(host []byte) []byte {
	if is.wire != nil && is.wireGen == is.Repo.JSONGen && is.wireHost == string(host) {
		return is.wire
	}
	return nil
}

// SetWire stores the rendered tail for host. The store drops it whenever the issue changes.
func (is *Issue) SetWire(host []byte, tail []byte) {
	is.wire = tail
	is.wireHost = is.Repo.store.InternHost(host)
	is.wireGen = is.Repo.JSONGen
}

// touch records a change to the issue at time now.
func (is *Issue) touch(now int64) {
	is.UpdatedAt = now
	is.wire = nil
}

// normalizeColor strips a leading '#' and lowercases; anything that is not six hex digits becomes
// the default grey.
func normalizeColor(c string) string {
	c = strings.ToLower(strings.TrimPrefix(c, "#"))
	if len(c) != 6 {
		return "ededed"
	}
	for i := 0; i < len(c); i++ {
		if !(c[i] >= '0' && c[i] <= '9' || c[i] >= 'a' && c[i] <= 'f') {
			return "ededed"
		}
	}
	return c
}

// Label returns the label called name (compared without case), or nil.
func (r *Repo) Label(name string) *Label {
	for _, l := range r.Labels {
		if l.Name == name {
			return l
		}
	}
	for _, l := range r.Labels {
		if strings.EqualFold(l.Name, name) {
			return l
		}
	}
	return nil
}

// CreateLabel is POST /labels. Errors (422 Validation Failed): missing name (missing_field), name
// taken (already_exists).
func (r *Repo) CreateLabel(name, color, description string, hasDescription bool) (*Label, error) {
	if name == "" {
		return nil, errMissing("Label", "name")
	}
	if r.Label(name) != nil {
		return nil, errValidation("Label", "name", "already_exists", "", "")
	}
	s := r.store
	s.Tick()
	l := &Label{Name: name, Color: normalizeColor(color), Description: description, HasDescription: hasDescription, Repo: r}
	s.labelIDs = append(s.labelIDs, l)
	l.ID = int64(len(s.labelIDs))
	r.Labels = append(r.Labels, l)
	return l, nil
}

// LabelPatch is the input of UpdateLabel; nil fields are left alone.
type LabelPatch struct {
	Name        *string
	Color       *string
	Description *string
}

// UpdateLabel renames or recolours a label. Issues carry the label object itself, so they follow.
func (r *Repo) UpdateLabel(l *Label, p LabelPatch) error {
	if p.Name != nil && *p.Name != l.Name {
		if *p.Name == "" {
			return errMissing("Label", "name")
		}
		if other := r.Label(*p.Name); other != nil && other != l {
			return errValidation("Label", "name", "already_exists", "", "")
		}
	}
	now := r.store.Tick()
	if p.Name != nil {
		l.Name = *p.Name
	}
	if p.Color != nil {
		l.Color = normalizeColor(*p.Color)
	}
	if p.Description != nil {
		l.Description, l.HasDescription = *p.Description, true
	}
	r.relabeled(l, now, false)
	return nil
}

// DeleteLabel removes a label from the repository and from every issue that carries it.
func (r *Repo) DeleteLabel(l *Label) {
	now := r.store.Tick()
	for i, x := range r.Labels {
		if x == l {
			r.Labels = append(r.Labels[:i], r.Labels[i+1:]...)
			break
		}
	}
	r.store.labelIDs[l.ID-1] = nil
	r.relabeled(l, now, true)
}

// relabeled refreshes every issue that carries l after the label changed or was removed.
func (r *Repo) relabeled(l *Label, now int64, remove bool) {
	for _, is := range r.Issues {
		for i, x := range is.Labels {
			if x != l {
				continue
			}
			if remove {
				is.Labels = append(is.Labels[:i:i], is.Labels[i+1:]...)
			}
			is.touch(now)
			break
		}
	}
}

// resolveLabels maps label names to the repository's labels. An unknown name is the 422 with
// {resource: Label, field: labels, code: invalid, value: <name>}; labels are never created here.
func (r *Repo) resolveLabels(names []string) ([]*Label, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]*Label, 0, len(names))
next:
	for _, name := range names {
		l := r.Label(name)
		if l == nil {
			return nil, errValidation("Label", "labels", "invalid", name, "")
		}
		for _, have := range out {
			if have == l {
				continue next
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// NewIssue is the input of CreateIssue.
type NewIssue struct {
	Title  string
	Body   string
	Labels []string // names of existing labels
}

// nextNumber reserves the next number of the repository's shared issue / pull request sequence.
func (r *Repo) nextNumber() int64 {
	if r.items == nil {
		r.items = make([]numbered, 0, 64)
	}
	r.items = append(r.items, numbered{})
	return int64(len(r.items))
}

// CreateIssue is POST /issues: a new open issue with the next number of the sequence it shares with
// pull requests. Title and body are stored byte for byte. Errors: 410 when issues are disabled, 422
// Validation Failed for an empty title (missing_field) or an unknown label (invalid, with the name).
func (r *Repo) CreateIssue(in NewIssue) (*Issue, error) {
	if !r.HasIssues {
		return nil, errStatus(410, "Issues are disabled for this repo")
	}
	if in.Title == "" {
		return nil, errMissing("Issue", "title")
	}
	labels, err := r.resolveLabels(in.Labels)
	if err != nil {
		return nil, err
	}
	s := r.store
	now := s.Tick()
	is := &Issue{Title: in.Title, Body: in.Body, Labels: labels, CreatedAt: now, UpdatedAt: now, Repo: r}
	s.issueIDs = append(s.issueIDs, is)
	is.ID = int64(len(s.issueIDs))
	is.Number = r.nextNumber()
	r.items[is.Number-1].issue = is
	r.Issues = append(r.Issues, is)
	r.OpenIssues++
	return is, nil
}

// Issue returns the issue with the given number, or nil when there is none or the number belongs
// to a pull request.
func (r *Repo) Issue(number int64) *Issue {
	if number < 1 || number > int64(len(r.items)) {
		return nil
	}
	return r.items[number-1].issue
}

// IssuePatch is the input of UpdateIssue; nil fields are left alone.
type IssuePatch struct {
	Title       *string
	Body        *string
	State       *string // "open" or "closed"
	StateReason *string // "completed", "not_planned", "reopened"
	Labels      *[]string
}

// UpdateIssue is PATCH /issues/{n}. Closing sets closed_at and state_reason "completed" (or the
// given reason) and is idempotent; reopening clears closed_at and sets state_reason "reopened".
// Errors (422 Validation Failed): empty title, a state other than open/closed, an unknown label.
func (r *Repo) UpdateIssue(is *Issue, p IssuePatch) error {
	if p.Title != nil && *p.Title == "" {
		return errMissing("Issue", "title")
	}
	if p.State != nil && *p.State != "open" && *p.State != "closed" {
		return errValidation("Issue", "state", "invalid", *p.State, "")
	}
	if p.StateReason != nil {
		switch *p.StateReason {
		case "completed", "not_planned", "reopened", "":
		default:
			return errValidation("Issue", "state_reason", "invalid", *p.StateReason, "")
		}
	}
	var labels []*Label
	if p.Labels != nil {
		var err error
		if labels, err = r.resolveLabels(*p.Labels); err != nil {
			return err
		}
	}
	now := r.store.Tick()
	if p.Title != nil {
		is.Title = *p.Title
	}
	if p.Body != nil {
		is.Body = *p.Body
	}
	if p.Labels != nil {
		is.Labels = labels
	}
	if p.State != nil {
		switch closing := *p.State == "closed"; {
		case closing && !is.Closed:
			is.Closed, is.ClosedAt, is.StateReason = true, now, "completed"
			r.OpenIssues--
		case !closing && is.Closed:
			is.Closed, is.ClosedAt, is.StateReason = false, 0, "reopened"
			r.OpenIssues++
		}
	}
	if p.StateReason != nil && *p.StateReason != "" && is.Closed == (*p.StateReason != "reopened") {
		is.StateReason = *p.StateReason
	}
	is.touch(now)
	return nil
}

// State filters for issue and pull request listings.
type State uint8

const (
	StateOpen State = iota // the default of both listings
	StateClosed
	StateAll
)

// ParseState maps "open", "closed" and "all" (any case); anything else is StateOpen and ok false.
func ParseState(s string) (State, bool) {
	switch {
	case s == "open" || strings.EqualFold(s, "open"):
		return StateOpen, true
	case s == "closed" || strings.EqualFold(s, "closed"):
		return StateClosed, true
	case s == "all" || strings.EqualFold(s, "all"):
		return StateAll, true
	}
	return StateOpen, false
}

// Sort keys for listings. Creation order is number order, so SortCreated never compares timestamps.
type Sort uint8

const (
	SortCreated Sort = iota
	SortUpdated
	SortComments
)

// IssueQuery selects and orders issues.
type IssueQuery struct {
	State  State
	Labels []string // every listed label must be on the issue (names compared without case)
	Sort   Sort
	Asc    bool // false = newest (or largest) first, the REST default
	Offset int  // items to skip after filtering and sorting
	Limit  int  // maximum items to return; <= 0 = no limit
}

func (q *IssueQuery) match(is *Issue) bool {
	if (q.State == StateOpen && is.Closed) || (q.State == StateClosed && !is.Closed) {
		return false
	}
next:
	for _, want := range q.Labels {
		for _, l := range is.Labels {
			if strings.EqualFold(l.Name, want) {
				continue next
			}
		}
		return false
	}
	return true
}

// ListIssues appends the selected page of issues to dst and returns it with the number of issues
// that matched before paging. Pull requests are never included. Ties under SortUpdated and
// SortComments break by number in the direction of the sort.
func (r *Repo) ListIssues(q IssueQuery, dst []*Issue) (page []*Issue, total int) {
	base := len(dst)
	if q.Sort == SortCreated {
		// Walk the creation-ordered slice in the requested direction; no sorting, no copying.
		n := len(r.Issues)
		for i := 0; i < n; i++ {
			is := r.Issues[i]
			if !q.Asc {
				is = r.Issues[n-1-i]
			}
			if !q.match(is) {
				continue
			}
			if total >= q.Offset && (q.Limit <= 0 || len(dst)-base < q.Limit) {
				dst = append(dst, is)
			}
			total++
		}
		return dst, total
	}
	for _, is := range r.Issues {
		if q.match(is) {
			dst = append(dst, is)
		}
	}
	all := dst[base:]
	total = len(all)
	key := func(is *Issue) int64 {
		if q.Sort == SortComments {
			return int64(len(is.Comments))
		}
		return is.UpdatedAt
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if ka, kb := key(a), key(b); ka != kb {
			return (ka < kb) == q.Asc
		}
		return (a.Number < b.Number) == q.Asc
	})
	lo := min(q.Offset, total)
	hi := total
	if q.Limit > 0 && lo+q.Limit < hi {
		hi = lo + q.Limit
	}
	return append(dst[:base], all[lo:hi]...), total
}

// AddComment is POST /issues/{n}/comments; number may name an issue or a pull request. Errors: 404
// for an unknown number, 422 Validation Failed (missing_field body) for an empty body.
func (r *Repo) AddComment(number int64, body string) (*Comment, error) {
	if number < 1 || number > int64(len(r.items)) {
		return nil, ErrNotFound
	}
	if body == "" {
		return nil, errMissing("IssueComment", "body")
	}
	s := r.store
	now := s.Tick()
	c := &Comment{Number: number, Body: body, CreatedAt: now, UpdatedAt: now, Repo: r}
	s.commentIDs = append(s.commentIDs, c)
	c.ID = int64(len(s.commentIDs))
	if it := r.items[number-1]; it.issue != nil {
		it.issue.Comments = append(it.issue.Comments, c)
		it.issue.touch(now)
	} else {
		it.pull.Comments = append(it.pull.Comments, c)
		it.pull.UpdatedAt = now
	}
	return c, nil
}

// Comments returns the comments of an issue or pull request in creation order. ok is false for an
// unknown number. The slice is the store's own: do not modify it.
func (r *Repo) Comments(number int64) (comments []*Comment, ok bool) {
	if number < 1 || number > int64(len(r.items)) {
		return nil, false
	}
	if it := r.items[number-1]; it.issue != nil {
		return it.issue.Comments, true
	} else {
		return it.pull.Comments, true
	}
}
