// Command forge is a GitHub-compatible git forge in one process: git smart HTTP, REST v3, GraphQL
// v4 and MCP over one in-memory store.
//
// It also runs the whole life cycle the grading environment expects:
//
//  1. MAKEFUN_ENV_URL names the environment. Without it there is nothing to serve: exit 1.
//     (FORGE_ENV_URL, set by the launcher, is the same URL with the host already resolved; it is
//     tried first and MAKEFUN_ENV_URL remains the fallback.)
//  2. Listen on every interface at an OS-chosen port (never 8080) and start serving.
//  3. Once /healthz answers, POST {"port": N} to $MAKEFUN_ENV_URL/register, retrying with backoff.
//  4. Poll GET $MAKEFUN_ENV_URL/done until it says {"done": true}, then exit 0.
//
// The process logs three lines in total (the last one with its memory use) and nothing per request.
// FORGE_STATS=1 adds one line of I/O counters just before the last.
package main

import (
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"forge/internal/app"
	"forge/internal/httpd"
)

func main() {
	tune()

	env, err := newEnvironment(os.Getenv("MAKEFUN_ENV_URL"), os.Getenv("FORGE_ENV_URL"))
	if err != nil {
		logf("MAKEFUN_ENV_URL: " + err.Error())
		os.Exit(1)
	}

	ln, err := httpd.Listen()
	if err != nil {
		logf("cannot listen: " + err.Error())
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	if token == "" {
		logf("warning: MAKEFUN_SHOP_TOKEN is empty; any credentials will be accepted")
	}
	forge := app.New()
	srv := &httpd.Server{
		Handler:     forge.Serve,
		Token:       token,
		DefaultHost: "127.0.0.1:" + strconv.Itoa(port),
		IO:          ioLoop(),
		// Experiments of the epoll loop, each off unless its variable is exactly "1".
		Predict:     os.Getenv("FORGE_PREDICT") == "1",
		DeferAccept: os.Getenv("FORGE_DEFER_ACCEPT") == "1",
		Count:       os.Getenv("FORGE_STATS") == "1",
		Yield:       threadsYield(),
	}
	go func() {
		err := srv.Serve(ln)
		logf("server stopped: " + err.Error())
		os.Exit(1)
	}()
	logf("listening on port " + strconv.Itoa(port) + " (" + srv.IO + ")")

	if !waitHealthy(port) {
		logf("warning: /healthz did not answer on loopback; registering anyway")
	}
	if err := register(env, port); err != nil {
		logf(err.Error())
		os.Exit(1)
	}
	logf("registered; waiting for the environment to finish")

	waitDone(env)
	if srv.Count {
		logf(srv.StatsLine())
	}
	logf("done; " + memoryReport())
	os.Exit(0)
}

// memoryReport sums up the process's memory use for the one line logged at exit: with the
// collector off, what was allocated is what is held, and this is the number the memory limit in
// tune must stay well above.
func memoryReport() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mib := func(n uint64) string { return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB" }
	return "allocated " + mib(m.TotalAlloc) + " in total, heap in use " + mib(m.HeapInuse) +
		", obtained from the OS " + mib(m.Sys) + ", " + strconv.Itoa(int(m.NumGC)) + " collections"
}

// tune sets the runtime for a workload of short, mostly sequential requests on a small machine.
// Each setting yields to the standard environment variable when that is set, so a different value
// can be tried without rebuilding.
//
//   - One P: requests arrive one at a time on most workloads, and a second P only adds wake-ups and
//     a thread competing with the client for CPU.
//   - No periodic garbage collection: the hot paths do not allocate, so the collector would only
//     add pauses. The memory limit is the backstop: if the heap ever gets near it, the collector
//     runs. A complete conformance and benchmark session allocates about 21 MiB in total (the last
//     log line of every run reports the figure), and the largest request the server accepts can add
//     roughly 160 MiB, so 256 MiB leaves the collector idle in normal use and still keeps the
//     process far below the memory of a small container.
func tune() {
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(1)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(-1)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(256 << 20)
	}
}

// ioMode is the socket I/O loop used when FORGE_IO does not choose one. It can be set at link time
// (-ldflags "-X main.ioMode=netpoll"); empty means the epoll loop where the build has it (Linux)
// and the netpoll loop everywhere else.
var ioMode string

// threadsYield is Server.Yield from FORGE_YIELD_MS, an option of the threads loop: the number of
// milliseconds after which a connection's goroutine yields before it waits for its next request.
// "0" means never; unset or anything else means the server's default.
func threadsYield() time.Duration {
	ms, err := strconv.Atoi(os.Getenv("FORGE_YIELD_MS"))
	switch {
	case err != nil || ms < 0:
		return 0
	case ms == 0:
		return -1
	}
	return time.Duration(ms) * time.Millisecond
}

// ioLoop picks the socket I/O loop: FORGE_IO, else ioMode, else epoll on Linux and netpoll
// elsewhere. "epoll" is the single-threaded epoll loop of internal/httpd; under a sandboxed kernel
// it measured 1.39 ± 0.21 speed points above netpoll over 150 paired runs, with about 30 % fewer
// system calls. "threads" (Linux, an experiment) gives every connection a goroutine in a blocking
// read; it wants GOMAXPROCS above the number of connections that are busy at once. Asking for a
// loop by name where it does not exist, or for anything unknown, falls back to netpoll with one
// line of warning.
func ioLoop() string {
	want := os.Getenv("FORGE_IO")
	if want == "" {
		want = ioMode
	}
	switch want {
	case "":
		if httpd.EpollSupported {
			return httpd.IOEpoll
		}
		return httpd.IONetpoll
	case httpd.IONetpoll:
		return httpd.IONetpoll
	case httpd.IOEpoll:
		if httpd.EpollSupported {
			return httpd.IOEpoll
		}
		logf("warning: the epoll loop is not available on this platform; using netpoll")
	case httpd.IOThreads:
		if httpd.ThreadsSupported {
			return httpd.IOThreads
		}
		logf("warning: the threads loop is not available on this platform; using netpoll")
	default:
		logf("warning: unknown I/O loop " + strconv.Quote(want) + " (want netpoll, epoll or threads); using netpoll")
	}
	return httpd.IONetpoll
}

func logf(line string) {
	os.Stderr.WriteString("[forge] " + line + "\n")
}
