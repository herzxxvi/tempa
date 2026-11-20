package rest

import (
	"encoding/json"
	"math/rand"
	"testing"
)

// viaJSON is what encoding/json makes of an issue-creation body: the reference scanIssue must agree
// with whenever it accepts an input. plain is false when the body uses anything scanIssue leaves to
// the general decoder (another key, a non-string value).
func viaJSON(b []byte) (title, body string, plain, valid bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || fields == nil {
		return "", "", false, false
	}
	plain = true
	for key, raw := range fields {
		if (key != "title" && key != "body") || len(raw) == 0 || raw[0] != '"' {
			plain = false
		}
	}
	var in struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if json.Unmarshal(b, &in) != nil {
		return "", "", false, true
	}
	if in.Title != nil {
		title = *in.Title
	}
	if in.Body != nil {
		body = *in.Body
	}
	return title, body, plain, true
}

func TestScanIssue(t *testing.T) {
	accepted := map[string][2]string{
		`{"title":"Incorrect GraphQL node ids [bench 1]","body":"A minimal case is attached."}`: {"Incorrect GraphQL node ids [bench 1]", "A minimal case is attached."},
		`{"body":"b","title":"t"}`: {"t", "b"},
		`{"title":"t"}`:            {"t", ""},
		`{"body":"b"}`:             {"", "b"},
		`{}`:                       {"", ""},
		" \t\r\n{ \"title\" : \"t\" , \"body\" : \"b\" } \n": {"t", "b"},
		`{"title":"first","title":"last"}`:                   {"last", ""},
		`{"title":"héllo 世界 😀","body":"<b>&</b> 'x'"}`:       {"héllo 世界 😀", "<b>&</b> 'x'"},
		`{"title":"","body":""}`:                             {"", ""},
		"{\"title\":\"del \x7f ok\"}":                        {"del \x7f ok", ""},
	}
	for in, want := range accepted {
		title, body, ok := scanIssue([]byte(in))
		if !ok || string(title) != want[0] || string(body) != want[1] {
			t.Errorf("scanIssue(%q) = %q, %q, %v", in, title, body, ok)
		}
		if jt, jb, plain, valid := viaJSON([]byte(in)); !valid || !plain || jt != want[0] || jb != want[1] {
			t.Errorf("encoding/json disagrees on %q: %q, %q", in, jt, jb)
		}
	}
	refused := []string{
		``, ` `, `{`, `}`, `[]`, `null`, `"title"`, `{"title"}`, `{"title":}`, `{"title":"t"`, `{"title":"t",}`, `{,"title":"t"}`,
		`{"title":"t"} x`, `{"title":"t"}{}`, `{"title":"t" "body":"b"}`, `{"title":"unterminated}`, `{title:"t"}`,
		`{"title":"a\nb"}`, `{"title":"a\"b"}`, `{"title":"a\\b"}`, `{"title":"\u0041"}`, "{\"title\":\"raw\nnewline\"}", "{\"title\":\"tab\there\"}",
		`{"title":null}`, `{"title":17}`, `{"title":true}`, `{"title":["t"]}`, `{"title":{"x":"y"}}`,
		`{"Title":"t"}`, `{"TITLE":"t"}`, `{"title":"t","labels":["bug"]}`, `{"title":"t","assignees":[]}`, `{"title":"t","labels":[]}`,
		"{\"title\":\"bad \xff utf8\"}", "{\"title\":\"cut \xe4\xb8\"}", "{\"title\":\"t\"}\x00", "\xef\xbb\xbf{\"title\":\"t\"}",
	}
	for _, in := range refused {
		if title, body, ok := scanIssue([]byte(in)); ok {
			t.Errorf("scanIssue(%q) accepted: %q, %q", in, title, body)
		}
	}
}

// TestScanIssueAgainstJSON feeds scanIssue bodies made by mutating valid ones and checks the one
// promise it makes: whatever it accepts, encoding/json accepts with the same result.
func TestScanIssueAgainstJSON(t *testing.T) {
	seeds := []string{
		`{"title":"Incorrect GraphQL node ids [bench 1]","body":"A minimal case is attached but the second request fails."}`,
		`{"title":"t","body":"b"}`,
		` { "body" : "héllo 世界" , "title" : "x" } `,
		`{"title":"t","labels":["bug"],"body":null}`,
		`{"title":"a\"b","body":"c\\d\u00e9"}`,
	}
	alphabet := []byte("{}[]:,\"\\ \n\ttilebodynu0x\x00\x1f\x7f\x80\xc3\xa9\xff")
	rng := rand.New(rand.NewSource(1))
	accepted := 0
	for i := 0; i < 200000; i++ {
		b := []byte(seeds[rng.Intn(len(seeds))])
		for n := rng.Intn(4); n > 0 && len(b) > 0; n-- {
			at := rng.Intn(len(b))
			switch rng.Intn(3) {
			case 0:
				b[at] = alphabet[rng.Intn(len(alphabet))]
			case 1:
				b = append(b[:at], b[at+1:]...)
			default:
				b = append(b[:at], append([]byte{alphabet[rng.Intn(len(alphabet))]}, b[at:]...)...)
			}
		}
		title, body, ok := scanIssue(b)
		if !ok {
			continue
		}
		accepted++
		jt, jb, plain, valid := viaJSON(b)
		if !valid || !plain || jt != string(title) || jb != string(body) {
			t.Fatalf("scanIssue(%q) = %q, %q but encoding/json gives %q, %q (valid %v, plain %v)", b, title, body, jt, jb, valid, plain)
		}
	}
	if accepted < 1000 {
		t.Errorf("only %d mutated bodies were accepted: the test is not exercising the scanner", accepted)
	}
}
