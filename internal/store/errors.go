package store

// Error is the failure of a domain operation, already shaped the way the REST API reports it:
// an HTTP status, the top-level "message" and at most one entry of the "errors" array. Adapters
// render it with render.AppendError (REST, MCP) or map Status to their own vocabulary (GraphQL), so
// the same failure reads the same everywhere.
type Error struct {
	Status  int    // 404, 405, 409, 410 or 422
	Message string // REST "message"

	// The entry of the REST "errors" array, when Code is set.
	Resource string // "Repository", "Issue", "Label", "IssueComment", "PullRequest", ...
	Field    string
	Code     string // "missing_field", "invalid", "already_exists", "custom"
	Value    string // offending value, for "invalid"
	Detail   string // human text, for "custom"

	DocURL string // overrides the default documentation_url when set
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Message + ": " + e.Detail
	}
	return e.Message
}

// StatusOf returns the HTTP status of an error from this package (500 for anything else).
func StatusOf(err error) int {
	if e, ok := err.(*Error); ok {
		return e.Status
	}
	return 500
}

// ErrNotFound is the plain 404 every missing object produces.
var ErrNotFound = &Error{Status: 404, Message: "Not Found"}

func errStatus(status int, message string) *Error {
	return &Error{Status: status, Message: message}
}

// errValidation is a 422 "Validation Failed" with one errors[] entry.
func errValidation(resource, field, code, value, detail string) *Error {
	return &Error{Status: 422, Message: "Validation Failed", Resource: resource, Field: field, Code: code, Value: value, Detail: detail}
}

func errMissing(resource, field string) *Error {
	return errValidation(resource, field, "missing_field", "", "")
}

const repoCreateDoc = "https://docs.github.com/rest/repos/repos#create-a-repository-for-the-authenticated-user"

func errRepoCreate(code, detail string) *Error {
	return &Error{Status: 422, Message: "Repository creation failed.", Resource: "Repository", Field: "name", Code: code, Detail: detail, DocURL: repoCreateDoc}
}

// errRepoExists is the duplicate-name error. Git clients and fixtures look for "already exists" in
// its body, so the wording is fixed.
func errRepoExists() *Error {
	return errRepoCreate("custom", "name already exists on this account")
}

// Messages of the fixed-text errors, shared so adapters and tests can refer to them.
const (
	MsgNotMergeable = "Pull Request is not mergeable"
	MsgEmptyRepo    = "This repository is empty."
	MsgEmptyGit     = "Git Repository is empty."
	MsgHeadModified = "Head branch was modified. Review and try the merge again."
	MsgMerged       = "Pull Request successfully merged"
)
