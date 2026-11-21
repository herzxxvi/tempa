//go:build !linux

package httpd

import "net"

// HybridSupported reports whether this build has the hybrid loop. It is Linux only; everywhere
// else the server uses the netpoll loop whatever Server.IO says.
const HybridSupported = false

// hybrid is the hybrid loop's shared state where there is one.
type hybrid struct{}

func (h *hybrid) halt() {}

func (h *hybrid) statsTail() string { return "" }

func (s *Server) serveHybrid(ln *net.TCPListener) error { return s.serveNetpoll(ln) }
