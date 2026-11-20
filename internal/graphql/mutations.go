package graphql

import (
	"strings"

	"forge/internal/store"
)

// Mutations. Each one resolves its global ids, calls the store operation the matching REST route
// calls and returns the live object, so what a mutation made reads back identically over REST.
// A store error becomes the field's error with the store's message.

// target resolves the global id in field of an input to an object of type T, or fails with the
// NOT_FOUND error GitHub gives for an id that names no such object.
func target[T any](q *query, in args, field string) (T, error) {
	if obj, ok := q.node(in.str(field)).(T); ok {
		return obj, nil
	}
	var none T
	return none, notFound(in.str(field))
}

func branchName(s string) string { return strings.TrimPrefix(s, "refs/heads/") }

func setIssueState(q *query, a args, state string) (any, error) {
	in := a.object("input")
	is, err := target[*store.Issue](q, in, "issueId")
	if err != nil {
		return nil, err
	}
	patch := store.IssuePatch{State: &state}
	if in.has("stateReason") {
		reason := strings.ToLower(in.str("stateReason"))
		patch.StateReason = &reason
	}
	if err := is.Repo.UpdateIssue(is, patch); err != nil {
		return nil, err
	}
	return &payload{mutationID: in["clientMutationId"], issue: is}, nil
}

func mutations(s *Schema) {
	call(s, "Mutation", callers[root]{
		"createIssue": func(q *query, _ root, a args) (any, error) {
			in := a.object("input")
			r, err := target[*store.Repo](q, in, "repositoryId")
			if err != nil {
				return nil, err
			}
			// Labels are given by id; the store takes names. An id that is no label of this
			// repository is skipped.
			var names []string
			for _, id := range in.strings("labelIds") {
				if l, ok := q.st.Node(id).(*store.Label); ok && l.Repo == r {
					names = append(names, l.Name)
				}
			}
			is, err := r.CreateIssue(store.NewIssue{Title: in.str("title"), Body: in.str("body"), Labels: names})
			if err != nil {
				return nil, err
			}
			return &payload{mutationID: in["clientMutationId"], issue: is}, nil
		},
		"closeIssue":  func(q *query, _ root, a args) (any, error) { return setIssueState(q, a, "closed") },
		"reopenIssue": func(q *query, _ root, a args) (any, error) { return setIssueState(q, a, "open") },
		"addComment": func(q *query, _ root, a args) (any, error) {
			in := a.object("input")
			var r *store.Repo
			var number int64
			subject := q.node(in.str("subjectId"))
			switch x := subject.(type) {
			case *store.Issue:
				r, number = x.Repo, x.Number
			case *store.Pull:
				r, number = x.Repo, x.Number
			default:
				return nil, notFound(in.str("subjectId"))
			}
			c, err := r.AddComment(number, in.str("body"))
			if err != nil {
				return nil, err
			}
			return &payload{
				mutationID: in["clientMutationId"],
				edge:       &edge{cursor: cursorOf(idKey(c.ID)), node: c},
				subject:    subject,
			}, nil
		},
		"createPullRequest": func(q *query, _ root, a args) (any, error) {
			in := a.object("input")
			r, err := target[*store.Repo](q, in, "repositoryId")
			if err != nil {
				return nil, err
			}
			p, err := r.CreatePull(store.NewPull{
				Title: in.str("title"),
				Body:  in.str("body"),
				Head:  branchName(in.str("headRefName")),
				Base:  branchName(in.str("baseRefName")),
				Draft: in.flag("draft"),
			})
			if err != nil {
				return nil, err
			}
			return &payload{mutationID: in["clientMutationId"], pull: p}, nil
		},
		"mergePullRequest": func(q *query, _ root, a args) (any, error) {
			in := a.object("input")
			p, err := target[*store.Pull](q, in, "pullRequestId")
			if err != nil {
				return nil, err
			}
			// The enum values are the store's method names in capitals: MERGE, SQUASH, REBASE.
			_, err = p.Repo.MergePull(p, store.MergeOptions{
				Method:        strings.ToLower(in.str("mergeMethod")),
				CommitTitle:   in.str("commitHeadline"),
				CommitMessage: in.str("commitBody"),
				SHA:           in.str("expectedHeadOid"),
			})
			if err != nil {
				return nil, err
			}
			return &payload{mutationID: in["clientMutationId"], pull: p}, nil
		},
		"closePullRequest": func(q *query, _ root, a args) (any, error) {
			in := a.object("input")
			p, err := target[*store.Pull](q, in, "pullRequestId")
			if err != nil {
				return nil, err
			}
			state := "closed"
			if err := p.Repo.UpdatePull(p, store.PullPatch{State: &state}); err != nil {
				return nil, err
			}
			return &payload{mutationID: in["clientMutationId"], pull: p}, nil
		},
	})

	issue := func(_ *query, p *payload) any { return p.issue }
	pull := func(_ *query, p *payload) any { return p.pull }
	for _, def := range []struct {
		name   string
		fields getters[*payload]
	}{
		{"CreateIssuePayload", getters[*payload]{"issue": issue}},
		{"CloseIssuePayload", getters[*payload]{"issue": issue}},
		{"ReopenIssuePayload", getters[*payload]{"issue": issue}},
		{"CreatePullRequestPayload", getters[*payload]{"pullRequest": pull}},
		{"MergePullRequestPayload", getters[*payload]{"pullRequest": pull}},
		{"ClosePullRequestPayload", getters[*payload]{"pullRequest": pull}},
		{"AddCommentPayload", getters[*payload]{
			"commentEdge": func(_ *query, p *payload) any { return p.edge },
			"subject":     func(_ *query, p *payload) any { return p.subject },
		}},
	} {
		def.fields["clientMutationId"] = func(_ *query, p *payload) any { return p.mutationID }
		get(s, def.name, def.fields)
	}
}
