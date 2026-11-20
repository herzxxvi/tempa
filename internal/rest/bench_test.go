package rest

import (
	"bytes"
	"strconv"
	"testing"

	"forge/internal/store"
)

// The two requests the benchmark times, byte for byte as the suite's client (Node's fetch) sends
// them, through the real engine and this surface's real handlers.
//
//	go test ./internal/rest/ -run xxx -bench . -benchmem
//
// Targets: GET issue 0 allocs/op; POST issue only what it stores (the issue, its text, its
// rendered reply).

const clientHeaders = "host: 127.0.0.1:43210\r\nconnection: keep-alive\r\naccept: application/vnd.github+json\r\n" +
	"authorization: Bearer " + token + "\r\nuser-agent: node\r\naccept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n"

const benchBody = "A minimal case is attached but the second request fails. The same flow works over the API and the server answers with a JSON error. Steps to reproduce are below but the second request fails."

func postIssueRequest(repo, body string) []byte {
	return []byte("POST /api/v3/repos/adaiversen/" + repo + "/issues HTTP/1.1\r\n" + clientHeaders +
		"content-type: application/json\r\ncontent-length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
}

// BenchmarkGetIssue is the read workload: GET /api/v3/repos/{owner}/{repo}/issues/{number}.
func BenchmarkGetIssue(b *testing.B) {
	cl := newClient(b)
	r, _ := cl.st.CreateRepo(store.NewRepo{Name: "bench-read", Private: true})
	r.CreateIssue(store.NewIssue{Title: "Incorrect GraphQL node ids (bench setup)", Body: benchBody})
	req := []byte("GET /api/v3/repos/adaiversen/bench-read/issues/1 HTTP/1.1\r\n" + clientHeaders + "\r\n")
	res := cl.tester.Do(req)
	if !bytes.HasPrefix(res, []byte("HTTP/1.1 200 OK\r\n")) {
		b.Fatalf("unexpected reply: %s", res)
	}
	b.Logf("response %d bytes, body %d bytes", len(res), len(res)-bytes.Index(res, []byte("\r\n\r\n"))-4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if res := cl.tester.Do(req); len(res) < 200 || res[9] != '2' {
			b.Fatal("bad reply")
		}
	}
}

// BenchmarkPostIssue is the write workload: POST /api/v3/repos/{owner}/{repo}/issues with a title
// and a body. The repository is replaced every few thousand issues so a long run does not measure
// a store of millions.
func BenchmarkPostIssue(b *testing.B) {
	cl := newClient(b)
	r, _ := cl.st.CreateRepo(store.NewRepo{Name: "bench-write", Private: true})
	req := postIssueRequest("bench-write", `{"title":"Incorrect GraphQL node ids [bench 1]","body":"`+benchBody+`"}`)
	if res := cl.tester.Do(req); !bytes.HasPrefix(res, []byte("HTTP/1.1 201 Created\r\n")) {
		b.Fatalf("unexpected reply: %s", res)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i&8191 == 8191 {
			cl.st.DeleteRepo(r)
			r, _ = cl.st.CreateRepo(store.NewRepo{Name: "bench-write", Private: true})
		}
		if res := cl.tester.Do(req); len(res) < 200 || res[9] != '2' {
			b.Fatal("bad reply")
		}
	}
}

// BenchmarkPostIssueLabels is an issue creation that takes the general decoder (labels present),
// for comparison with the scanned form above.
func BenchmarkPostIssueLabels(b *testing.B) {
	cl := newClient(b)
	r, _ := cl.st.CreateRepo(store.NewRepo{Name: "bench-write", Private: true})
	r.CreateLabel("bug", "", "", false)
	req := postIssueRequest("bench-write", `{"title":"Incorrect GraphQL node ids [bench 1]","body":"`+benchBody+`","labels":["bug"]}`)
	if res := cl.tester.Do(req); !bytes.HasPrefix(res, []byte("HTTP/1.1 201 Created\r\n")) {
		b.Fatalf("unexpected reply: %s", res)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i&8191 == 8191 {
			cl.st.DeleteRepo(r)
			r, _ = cl.st.CreateRepo(store.NewRepo{Name: "bench-write", Private: true})
			r.CreateLabel("bug", "", "", false)
		}
		if res := cl.tester.Do(req); len(res) < 200 || res[9] != '2' {
			b.Fatal("bad reply")
		}
	}
}

// TestHotPathAllocations pins the allocation budget of the two timed routes, so a change that makes
// the read path allocate (or the write path allocate more) fails the tests, not only a benchmark
// someone has to remember to run.
func TestHotPathAllocations(t *testing.T) {
	cl := newClient(t)
	r, _ := cl.st.CreateRepo(store.NewRepo{Name: "bench", Private: true})
	r.CreateIssue(store.NewIssue{Title: "Incorrect GraphQL node ids (bench setup)", Body: benchBody})
	get := []byte("GET /api/v3/repos/adaiversen/bench/issues/1 HTTP/1.1\r\n" + clientHeaders + "\r\n")
	post := postIssueRequest("bench", `{"title":"Incorrect GraphQL node ids [bench 1]","body":"`+benchBody+`"}`)
	cl.tester.Do(get)
	cl.tester.Do(post)
	if n := testing.AllocsPerRun(500, func() { cl.tester.Do(get) }); n != 0 {
		t.Errorf("GET issue allocates %v times per request, want 0", n)
	}
	// The issue, its text and its rendered reply; slices that grow add a fraction.
	if n := testing.AllocsPerRun(500, func() { cl.tester.Do(post) }); n > 4 {
		t.Errorf("POST issue allocates %v times per request, want at most 4", n)
	}
}
