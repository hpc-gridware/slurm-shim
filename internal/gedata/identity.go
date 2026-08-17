package gedata

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Identity resolves host, user, and network identity. It is a seam because a
// static (CGO-disabled) binary's os/user and pure-Go resolver ignore NSS, so
// LDAP/SSSD users and hosts fail; the production implementation falls back to
// getent through the Runner, which is NSS-aware (SI-34, D-4). Tests inject a
// deterministic double so golden env maps do not depend on the host.
type Identity interface {
	Hostname() (string, error)
	User() (name string, uid, gid int, err error)
	LookupIP(ctx context.Context, host string) (string, error)
	InterfaceAddr(iface string) (string, error)
}

// RealIdentity is the production Identity. Its getent fallbacks go through the
// Runner so they are recorded and mockable like every other GE call.
type RealIdentity struct {
	Runner Runner
}

// Hostname returns the local short hostname.
func (RealIdentity) Hostname() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h, nil
}

// User resolves the job user. uid/gid come from syscalls (always correct); the
// name comes from $USER, then $LOGNAME, then getent passwd (NSS-aware), then
// $SGE_O_LOGNAME. An unknown name returns empty, not an error.
func (id RealIdentity) User() (string, int, int, error) {
	uid, gid := os.Getuid(), os.Getgid()
	if u := os.Getenv("USER"); u != "" {
		return u, uid, gid, nil
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u, uid, gid, nil
	}
	if name := id.getentUser(uid); name != "" {
		return name, uid, gid, nil
	}
	if u := os.Getenv("SGE_O_LOGNAME"); u != "" {
		return u, uid, gid, nil
	}
	return "", uid, gid, nil
}

func (id RealIdentity) getentUser(uid int) string {
	if id.Runner == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _, exit, err := id.Runner.Run(ctx, "getent", "passwd", strconv.Itoa(uid))
	if err != nil || exit != 0 {
		return ""
	}
	// getent passwd: name:passwd:uid:gid:gecos:dir:shell
	return firstColonField(string(out))
}

// LookupIP returns the first IPv4 for host, trying the Go resolver first and
// falling back to getent hosts (NSS-aware) under a static build. An unresolved
// host returns an empty string and no error, so callers warn-and-continue
// (REQ-FAB-004).
func (id RealIdentity) LookupIP(ctx context.Context, host string) (string, error) {
	if ips, err := net.LookupIP(host); err == nil {
		if v4 := firstV4(ips); v4 != "" {
			return v4, nil
		}
	}
	if id.Runner != nil {
		out, _, exit, err := id.Runner.Run(ctx, "getent", "hosts", host)
		if err == nil && exit == 0 {
			// getent hosts: "<ip> <name> [aliases...]"
			if fields := strings.Fields(string(out)); len(fields) > 0 {
				return fields[0], nil
			}
		}
	}
	return "", nil
}

// InterfaceAddr returns the first IPv4 address of the named interface, or of the
// first non-loopback up interface when iface is empty.
func (RealIdentity) InterfaceAddr(iface string) (string, error) {
	if iface != "" {
		ifi, err := net.InterfaceByName(iface)
		if err != nil {
			return "", err
		}
		return firstV4Addr(ifi)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagUp == 0 {
			continue
		}
		if addr, _ := firstV4Addr(&ifi); addr != "" {
			return addr, nil
		}
	}
	return "", nil
}

func firstV4Addr(ifi *net.Interface) (string, error) {
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4.String(), nil
			}
		}
	}
	return "", nil
}

func firstV4(ips []net.IP) string {
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

func firstColonField(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i]
	}
	return s
}
