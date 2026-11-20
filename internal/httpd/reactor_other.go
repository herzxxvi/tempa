//go:build !linux

package httpd

import "net"

// EpollSupported reports whether this build has the epoll loop. It is Linux only; everywhere else
// the server uses the netpoll loop whatever Server.IO says.
const EpollSupported = false

func (s *Server) serveEpoll(ln *net.TCPListener) error { return s.serveNetpoll(ln) }
