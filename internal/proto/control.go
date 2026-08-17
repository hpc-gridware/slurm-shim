package proto

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"
)

// helloTimeout bounds how long the server waits for a dialer's HELLO before
// dropping it, so a stray connection cannot occupy an accept slot (SI-52).
const helloTimeout = 10 * time.Second

// NewToken returns a 32-byte cryptographically random hex token for a step
// (REQ-CHN-002).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Conn is one authenticated control connection. Send is safe for concurrent
// use; Recv is single-consumer.
type Conn struct {
	Host   string
	nc     net.Conn
	writer *FrameWriter
	reader *FrameReader
}

// Send writes a frame.
func (c *Conn) Send(f Frame) error { return c.writer.Write(f) }

// Recv reads the next frame.
func (c *Conn) Recv() (Frame, error) { return c.reader.Read() }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.nc.Close() }

// Server is srun's control listener. Steppers dial in and authenticate; each
// authenticated connection is delivered from Accept with its claimed host.
type Server struct {
	ln       net.Listener
	token    string
	accepted chan *Conn
	errc     chan error
}

// Listen opens a control listener bound to addr (use "127.0.0.1:0" for a local
// ephemeral port) authenticating with token, and starts accepting.
func Listen(addr, token string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		token:    token,
		accepted: make(chan *Conn),
		errc:     make(chan error, 1),
	}
	go s.acceptLoop()
	return s, nil
}

// Addr is the listener's address, including the chosen port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) acceptLoop() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			// The listener was closed; stop quietly.
			return
		}
		go s.authenticate(nc)
	}
}

// authenticate reads the HELLO, verifies the token in constant time, and
// delivers the connection. An unauthenticated or malformed dialer is dropped
// without affecting other steppers (REQ-CHN-002).
func (s *Server) authenticate(nc net.Conn) {
	_ = nc.SetReadDeadline(time.Now().Add(helloTimeout))
	fr := NewFrameReader(nc)
	hello, err := fr.Read()
	if err != nil || hello.Type != FrameHello {
		_ = nc.Close()
		return
	}
	token, host, ok := splitHello(hello.Payload)
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		_ = nc.Close()
		return
	}
	_ = nc.SetReadDeadline(time.Time{}) // clear the deadline for the session
	c := &Conn{Host: host, nc: nc, writer: NewFrameWriter(nc), reader: fr}
	select {
	case s.accepted <- c:
	case <-time.After(helloTimeout):
		_ = nc.Close()
	}
}

// Accept returns the next authenticated connection, or the context error.
func (s *Server) Accept(ctx context.Context) (*Conn, error) {
	select {
	case c := <-s.accepted:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops the listener.
func (s *Server) Close() error { return s.ln.Close() }

// Dial connects to srun's control channel and authenticates as host. The
// returned Conn is ready to receive the StepSpec.
func Dial(addr, token, host string) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, helloTimeout)
	if err != nil {
		return nil, err
	}
	c := &Conn{Host: host, nc: nc, writer: NewFrameWriter(nc), reader: NewFrameReader(nc)}
	if err := c.Send(Frame{Type: FrameHello, Payload: []byte(joinHello(token, host))}); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("proto: sending HELLO: %w", err)
	}
	return c, nil
}

// HELLO payload is "<token>\x00<host>" so the host may contain any character.
func joinHello(token, host string) string { return token + "\x00" + host }

func splitHello(payload []byte) (token, host string, ok bool) {
	s := string(payload)
	i := strings.IndexByte(s, 0)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
