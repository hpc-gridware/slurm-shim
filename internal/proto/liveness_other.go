//go:build !linux

package proto

import (
	"net"
	"time"
)

// setUserTimeout is Linux-only; elsewhere keepalive alone detects a dead peer
// (exec hosts are Linux; this keeps development platforms building).
func setUserTimeout(*net.TCPConn, time.Duration) error { return nil }
