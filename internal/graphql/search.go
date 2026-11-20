package graphql

import (
	"sort"
	"strings"

	"forge/internal/store"
)

// Query.search: a small version of GitHub's search over the store. The query string is words and
// "quoted phrases" that must all occur in the searched text, plus these qualifiers:
//
//	repo:owner/name          only that repository (several: any of them)
//	user: org: owner: author:  the viewer's login (or @me) matches everything, another login nothing
//	is: type: state:         issue, pr, open, closed, merged, unmerged, public, private
//	in:                      title, body, comments (issues); name, description (repositories)
//	label:name               issues carrying the label
//	sort:field-direction     created, updated, comments; asc or desc (default created-desc)
//
// Other qualifiers are ignored.

type searchQuery struct {
	words  []string // lowercased
	repos  []string // lowercased "owner/name"
	labels []string
	in     map[string]bool
	is     map[string]bool
	nobody bool // a user qualifier named someone other than the viewer
	by     store.Sort
	asc    bool
}

func parseSearch(text string) *searchQuery {
	sq := &searchQuery{in: map[string]bool{}, is: map[string]bool{}}
	for text = strings.TrimSpace(text); text != ""; text = strings.TrimSpace(text) {
		// One term: up to the next space, except that a quoted part may contain spaces.
		end, quoted := 0, false
		for end < len(text) && (quoted || text[end] != ' ') {
			if text[end] == '"' {
				quoted = !quoted
			}
			end++
		}
		term := strings.ReplaceAll(text[:end], `"`, "")
		text = text[end:]
		key, value, qualified := strings.Cut(term, ":")
		value = strings.ToLower(value)
		switch {
		case !qualified || key == "" || value == "":
			if term != "" {
				sq.words = append(sq.words, strings.ToLower(term))
			}
		case key == "repo":
			sq.repos = append(sq.repos, value)
		case key == "user" || key == "org" || key == "owner" || key == "author":
			sq.nobody = sq.nobody || (value != store.ViewerLogin && value != "@me")
		case key == "is" || key == "type" || key == "state":
			sq.is[value] = true
		case key == "in":
			for _, part := range strings.Split(value, ",") {
				sq.in[part] = true
			}
		case key == "label":
			sq.labels = append(sq.labels, value)
		case key == "sort":
			field, direction, _ := strings.Cut(value, "-")
			switch field {
			case "updated":
				sq.by = store.SortUpdated
			case "comments":
				sq.by = store.SortComments
			}
			sq.asc = direction == "asc"
		}
	}
	return sq
}

// matches reports whether every word occurs in one of the texts.
func (sq *searchQuery) matches(texts ...string) bool {
	for _, w := range sq.words {
		found := false
		for _, t := range texts {
			found = found || strings.Contains(strings.ToLower(t), w)
		}
		if !found {
			return false
		}
	}
	return true
}

func (sq *searchQuery) repo(r *store.Repo) bool {
	if len(sq.repos) == 0 {
		return true
	}
	for _, name := range sq.repos {
		if name == strings.ToLower(r.FullName()) {
			return true
		}
	}
	return false
}

// texts returns the parts of an issue or pull request the query searches in.
func (sq *searchQuery) texts(title, body string, comments []*store.Comment) []string {
	var out []string
	everywhere := !sq.in["title"] && !sq.in["body"] && !sq.in["comments"]
	if everywhere || sq.in["title"] {
		out = append(out, title)
	}
	if everywhere || sq.in["body"] {
		out = append(out, body)
	}
	if sq.in["comments"] {
		for _, c := range comments {
			out = append(out, c.Body)
		}
	}
	return out
}

func (sq *searchQuery) issue(is *store.Issue) bool {
	if sq.is["pr"] || sq.is["merged"] || sq.is["unmerged"] || (sq.is["open"] && is.Closed) || (sq.is["closed"] && !is.Closed) {
		return false
	}
next:
	for _, want := range sq.labels {
		for _, l := range is.Labels {
			if strings.ToLower(l.Name) == want {
				continue next
			}
		}
		return false
	}
	return sq.matches(sq.texts(is.Title, is.Body, is.Comments)...)
}

func (sq *searchQuery) pull(p *store.Pull) bool {
	if sq.is["issue"] || len(sq.labels) > 0 || (sq.is["open"] && p.Closed) || (sq.is["closed"] && !p.Closed) ||
		(sq.is["merged"] && !p.Merged) || (sq.is["unmerged"] && p.Merged) {
		return false
	}
	return sq.matches(sq.texts(p.Title, p.Body, p.Comments)...)
}

func (q *query) search(a args) (any, error) {
	sq := parseSearch(a.str("query"))
	var list []any
	var counts [3]int
	switch kind := a.str("type"); {
	case sq.nobody:
	case kind == "ISSUE" || kind == "ISSUE_ADVANCED":
		for _, r := range q.st.Repos() {
			if !sq.repo(r) {
				continue
			}
			for _, is := range r.Issues {
				if sq.issue(is) {
					list = append(list, is)
				}
			}
			for _, p := range r.Pulls {
				if sq.pull(p) {
					list = append(list, p)
				}
			}
		}
		// Newest first unless the query says otherwise; equal keys keep repository order.
		created := func(v any) int64 {
			if is, ok := v.(*store.Issue); ok {
				return is.CreatedAt
			}
			return v.(*store.Pull).CreatedAt
		}
		rank := compare(sq.by, sq.asc)
		sort.SliceStable(list, func(i, j int) bool {
			if sq.by != store.SortCreated {
				return rank(list[i], list[j]) < 0
			}
			ci, cj := created(list[i]), created(list[j])
			return ci != cj && (ci < cj) == sq.asc
		})
		counts[0] = len(list)
	case kind == "REPOSITORY":
		for _, r := range q.st.Repos() {
			texts := []string{r.Name, r.Description}
			if sq.in["name"] && !sq.in["description"] {
				texts = texts[:1]
			} else if sq.in["description"] && !sq.in["name"] {
				texts = texts[1:]
			}
			if sq.repo(r) && !(sq.is["public"] && r.Private) && !(sq.is["private"] && !r.Private) && sq.matches(texts...) {
				list = append(list, r)
			}
		}
		counts[1] = len(list)
	case kind == "USER":
		if sq.matches(store.ViewerLogin, store.ViewerName) {
			list = append(list, store.Viewer{})
		}
		counts[2] = len(list)
	}
	c, err := paginate(identified(list, func(v any) string {
		switch x := v.(type) {
		case *store.Issue:
			return "I" + idKey(x.ID)
		case *store.Pull:
			return "PR" + idKey(x.ID)
		case *store.Repo:
			return "R" + idKey(x.ID)
		}
		return "U"
	}), a)
	if err != nil {
		return nil, err
	}
	c.counts = counts
	return c, nil
}
