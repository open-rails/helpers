// Package smtptest is an in-process SMTP server that captures what it is
// sent, so a test drives a real sender (helpers/smtp, or anything speaking
// SMTP) end to end and reads the delivered message back.
package smtptest

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options shape the server. The zero value accepts any mail on a plain
// connection without authentication.
type Options struct {
	// Username and Password, when set, are the only credentials accepted, and
	// mail is refused until a client authenticates (PLAIN or LOGIN).
	Username string
	Password string
	// Mechanisms are the AUTH mechanisms offered: PLAIN and LOGIN when empty.
	Mechanisms []string
	// STARTTLS offers the upgrade and withholds AUTH until it is done.
	STARTTLS bool
	// ImplicitTLS speaks TLS from the first byte, like port 465.
	ImplicitTLS bool
	// Refuse, when it returns a non-empty reply ("550 5.1.1 no such user"),
	// refuses that recipient with it.
	Refuse func(recipient string) string
}

// Message is one delivered email, decoded.
type Message struct {
	// From and To are the envelope (MAIL FROM, RCPT TO).
	From string
	To   []string
	// Username is who authenticated, empty for none; TLS reports whether the
	// connection was encrypted.
	Username string
	TLS      bool
	// Raw is the message exactly as received (after dot-unstuffing).
	Raw []byte
	// Header is Raw's header; Subject is decoded.
	Header  mail.Header
	Subject string
	// Text and HTML are the decoded text/plain and text/html bodies; a
	// single-part body keeps the final line break SMTP framing adds.
	Text string
	HTML string
}

// Server is a running SMTP server.
type Server struct {
	// Host and Port are where it listens (127.0.0.1).
	Host string
	Port int

	opts     Options
	ln       net.Listener
	tls      *tls.Config
	roots    *x509.CertPool
	mu       sync.Mutex
	messages []Message
	arrived  chan struct{}
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

// Start runs a server until t ends.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	s, err := Listen(opts)
	if err != nil {
		t.Fatalf("smtptest: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// Listen runs a server on 127.0.0.1 until Close; Start is the usual entry.
func Listen(opts Options) (*Server, error) {
	s := &Server{opts: opts, arrived: make(chan struct{}, 1), conns: map[net.Conn]struct{}{}}
	if opts.STARTTLS || opts.ImplicitTLS {
		cfg, roots, err := selfSigned()
		if err != nil {
			return nil, err
		}
		s.tls, s.roots = cfg, roots
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if opts.ImplicitTLS {
		ln = tls.NewListener(ln, s.tls)
	}
	s.ln = ln
	addr := ln.Addr().(*net.TCPAddr)
	s.Host, s.Port = addr.IP.String(), addr.Port
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr is host:port.
func (s *Server) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// ClientTLS is a TLS client configuration that trusts this server's
// certificate; nil when it speaks no TLS.
func (s *Server) ClientTLS() *tls.Config {
	if s.roots == nil {
		return nil
	}
	return &tls.Config{RootCAs: s.roots, MinVersion: tls.VersionTLS12}
}

// Messages are the messages delivered so far, oldest first.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

// Wait returns the first n messages once they have arrived, failing t after
// timeout.
func (s *Server) Wait(t testing.TB, n int, timeout time.Duration) []Message {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if got := s.Messages(); len(got) >= n {
			return got[:n]
		}
		select {
		case <-s.arrived:
		case <-deadline.C:
			t.Fatalf("smtptest: %d of %d messages arrived within %s", len(s.Messages()), n, timeout)
		}
	}
}

// Close stops the server and ends its connections.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.serve(c)
		}()
	}
}

type session struct {
	s        *Server
	conn     net.Conn
	text     *textproto.Conn
	tls      bool
	hello    bool
	username string
	from     string
	to       []string
}

func (s *Server) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(time.Minute))
	ss := &session{s: s, conn: c, text: textproto.NewConn(c), tls: s.opts.ImplicitTLS}
	ss.reply("220 smtptest ESMTP")
	for {
		line, err := ss.text.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		if !ss.handle(strings.ToUpper(verb), strings.TrimSpace(arg)) {
			return
		}
	}
}

func (ss *session) reply(format string, args ...any) { _ = ss.text.PrintfLine(format, args...) }

func (ss *session) authRequired() bool { return ss.s.opts.Username != "" }

// handle answers one command; false ends the connection.
func (ss *session) handle(verb, arg string) bool {
	switch verb {
	case "EHLO", "HELO":
		ss.hello, ss.from, ss.to = true, "", nil
		lines := []string{"smtptest"}
		if ss.s.opts.STARTTLS && !ss.tls {
			lines = append(lines, "STARTTLS")
		}
		if ss.authRequired() && (ss.tls || !ss.s.opts.STARTTLS) {
			lines = append(lines, "AUTH "+strings.Join(ss.mechanisms(), " "))
		}
		lines = append(lines, "8BITMIME")
		for i, l := range lines {
			sep := "-"
			if i == len(lines)-1 {
				sep = " "
			}
			ss.reply("250%s%s", sep, l)
		}
	case "STARTTLS":
		if !ss.s.opts.STARTTLS || ss.tls {
			ss.reply("502 5.5.1 STARTTLS not offered")
			return true
		}
		ss.reply("220 2.0.0 ready")
		tc := tls.Server(ss.conn, ss.s.tls)
		if err := tc.Handshake(); err != nil {
			return false
		}
		ss.conn, ss.text, ss.tls, ss.hello = tc, textproto.NewConn(tc), true, false
	case "AUTH":
		ss.auth(arg)
	case "MAIL":
		switch {
		case !ss.hello:
			ss.reply("503 5.5.1 EHLO first")
		case ss.authRequired() && ss.username == "":
			ss.reply("530 5.7.0 authentication required")
		default:
			ss.from, ss.to = path(arg, "FROM:"), nil
			ss.reply("250 2.1.0 ok")
		}
	case "RCPT":
		if ss.from == "" {
			ss.reply("503 5.5.1 MAIL first")
			return true
		}
		to := path(arg, "TO:")
		if ss.s.opts.Refuse != nil {
			if r := ss.s.opts.Refuse(to); r != "" {
				ss.reply("%s", r)
				return true
			}
		}
		ss.to = append(ss.to, to)
		ss.reply("250 2.1.5 ok")
	case "DATA":
		if len(ss.to) == 0 {
			ss.reply("503 5.5.1 RCPT first")
			return true
		}
		ss.reply("354 end with <CRLF>.<CRLF>")
		raw, err := ss.text.ReadDotBytes()
		if err != nil {
			return false
		}
		m, err := decode(raw)
		if err != nil {
			ss.reply("554 5.6.0 %s", strings.ReplaceAll(err.Error(), "\n", " "))
			return true
		}
		m.From, m.To, m.Username, m.TLS = ss.from, ss.to, ss.username, ss.tls
		ss.s.deliver(m)
		ss.from, ss.to = "", nil
		ss.reply("250 2.0.0 queued")
	case "RSET":
		ss.from, ss.to = "", nil
		ss.reply("250 2.0.0 ok")
	case "NOOP":
		ss.reply("250 2.0.0 ok")
	case "QUIT":
		ss.reply("221 2.0.0 bye")
		return false
	default:
		ss.reply("502 5.5.2 unknown command")
	}
	return true
}

func (ss *session) mechanisms() []string {
	if m := ss.s.opts.Mechanisms; len(m) > 0 {
		return m
	}
	return []string{"PLAIN", "LOGIN"}
}

func (ss *session) auth(arg string) {
	if !ss.authRequired() || (ss.s.opts.STARTTLS && !ss.tls) {
		ss.reply("503 5.5.1 AUTH not offered")
		return
	}
	mech, initial, _ := strings.Cut(arg, " ")
	mech = strings.ToUpper(mech)
	if !slices.Contains(ss.mechanisms(), mech) {
		ss.reply("504 5.5.4 unsupported mechanism")
		return
	}
	var user, pass string
	switch mech {
	case "PLAIN":
		if initial == "" {
			ss.reply("334 ")
			initial, _ = ss.text.ReadLine()
		}
		b, err := base64.StdEncoding.DecodeString(initial)
		parts := strings.Split(string(b), "\x00")
		if err != nil || len(parts) != 3 {
			ss.reply("501 5.5.2 malformed PLAIN response")
			return
		}
		user, pass = parts[1], parts[2]
	case "LOGIN":
		var ok bool
		if user, ok = ss.challenge("Username:"); !ok {
			return
		}
		if pass, ok = ss.challenge("Password:"); !ok {
			return
		}
	default:
		ss.reply("504 5.5.4 unsupported mechanism")
		return
	}
	if user != ss.s.opts.Username || pass != ss.s.opts.Password {
		ss.reply("535 5.7.8 authentication credentials invalid")
		return
	}
	ss.username = user
	ss.reply("235 2.7.0 authenticated")
}

func (ss *session) challenge(prompt string) (string, bool) {
	ss.reply("334 %s", base64.StdEncoding.EncodeToString([]byte(prompt)))
	line, err := ss.text.ReadLine()
	if err != nil {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		ss.reply("501 5.5.2 malformed LOGIN response")
		return "", false
	}
	return string(b), true
}

func (s *Server) deliver(m Message) {
	s.mu.Lock()
	s.messages = append(s.messages, m)
	s.mu.Unlock()
	select {
	case s.arrived <- struct{}{}:
	default:
	}
}

// path is the address of "FROM:<a@b> SIZE=1".
func path(arg, prefix string) string {
	if len(arg) >= len(prefix) && strings.EqualFold(arg[:len(prefix)], prefix) {
		arg = arg[len(prefix):]
	}
	arg, _, _ = strings.Cut(strings.TrimSpace(arg), " ")
	return strings.TrimSuffix(strings.TrimPrefix(arg, "<"), ">")
}

// decode parses raw strictly enough that a malformed message fails the
// sender's test instead of passing through.
func decode(raw []byte) (Message, error) {
	m := Message{Raw: raw}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return m, fmt.Errorf("header: %w", err)
	}
	m.Header = msg.Header
	for _, h := range []string{"From", "To", "Subject", "Date", "Message-Id"} {
		if msg.Header.Get(h) == "" {
			return m, fmt.Errorf("missing %s header", h)
		}
	}
	if _, err := msg.Header.Date(); err != nil {
		return m, fmt.Errorf("date: %w", err)
	}
	if m.Subject, err = new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); err != nil {
		return m, fmt.Errorf("subject: %w", err)
	}
	err = m.body(textproto.MIMEHeader(msg.Header), msg.Body)
	return m, err
}

func (m *Message) body(h textproto.MIMEHeader, r io.Reader) error {
	kind, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("content type: %w", err)
	}
	if strings.HasPrefix(kind, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("multipart: %w", err)
			}
			// NextPart already decoded a quoted-printable part.
			if err := m.body(p.Header, p); err != nil {
				return err
			}
		}
	}
	switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	}
	b, err := io.ReadAll(bufio.NewReader(r))
	if err != nil {
		return fmt.Errorf("%s body: %w", kind, err)
	}
	switch kind {
	case "text/plain":
		m.Text = string(b)
	case "text/html":
		m.HTML = string(b)
	}
	return nil
}

func selfSigned() (*tls.Config, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "smtptest"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}},
		MinVersion:   tls.VersionTLS12,
	}, roots, nil
}
