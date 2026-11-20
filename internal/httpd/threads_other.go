//go:build !linux

package httpd

import "net"

// ThreadsSupported reports whether this build has the threads loop. It is Linux only; everywhere
// else the server uses the netpoll loop whatever Server.IO says.
const ThreadsSupported = false

// threads is the threads loop's state where there is one.
type threads struct{}

func (t *threads) halt() {}

func (s *Server) serveThreads(ln *net.TCPListener) error { return s.serveNetpoll(ln) }
