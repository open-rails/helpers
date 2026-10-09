// Package smtp sends email through any SMTP submission server, so the email
// provider (SendGrid, ZeptoMail, a self-hosted server, …) is configuration:
// host, port, username, password and the sender.
//
// Port 465 is TLS from the first byte; any other port upgrades with STARTTLS
// when the server offers it, and must when credentials are sent to a host
// other than loopback. Each Send opens its own connection, so a Sender is safe
// for concurrent use. No error carries the password.
package smtp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the submission port used when Config.Port is 0.
const DefaultPort = 587

// DefaultTimeout bounds one Send or CheckHealth when Config.Timeout is 0.
const DefaultTimeout = 30 * time.Second

// Config is an SMTP submission server and the default sender.
type Config struct {
	Host string
	// Port is 465 for implicit TLS, else STARTTLS; 0 is DefaultPort.
	Port int
	// Username and Password authenticate (PLAIN, else LOGIN); an empty
	// Username sends without authenticating.
	Username string
	Password string
	// From is the default sender, one RFC 5322 mailbox ("Name <a@b.example>"
	// or "a@b.example"); a Message's From replaces it.
	From string
	// Timeout bounds one Send or CheckHealth; 0 is DefaultTimeout.
	Timeout time.Duration
	// TLS configures the TLS client (a private CA, a test server); nil uses
	// the system roots. Its ServerName defaults to Host.
	TLS *tls.Config
}

// Message is one email to one recipient. It needs a subject and a text or
// HTML body (both send multipart/alternative).
type Message struct {
	// From replaces Config.From for this message.
	From *mail.Address
	// To is one RFC 5322 mailbox.
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers Messages through one SMTP server.
type Sender struct {
	host     string
	addr     string
	implicit bool
	username string
	password string
	from     *mail.Address
	timeout  time.Duration
	tls      *tls.Config
}

// New validates cfg and returns its Sender. It does not connect.
func New(cfg Config) (*Sender, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return nil, errors.New("smtp: host is required")
	}
	port := cfg.Port
	if port == 0 {
		port = DefaultPort
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("smtp: invalid port %d", cfg.Port)
	}
	username := strings.TrimSpace(cfg.Username)
	if username == "" && cfg.Password != "" {
		return nil, errors.New("smtp: a password needs a username")
	}
	if username != "" && cfg.Password == "" {
		return nil, errors.New("smtp: a username needs a password")
	}
	s := &Sender{
		host:     host,
		addr:     net.JoinHostPort(host, strconv.Itoa(port)),
		implicit: port == 465,
		username: username,
		password: cfg.Password,
		timeout:  cfg.Timeout,
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	if from := strings.TrimSpace(cfg.From); from != "" {
		a, err := ParseMailbox(from)
		if err != nil {
			return nil, fmt.Errorf("smtp: from: %w", err)
		}
		s.from = a
	}
	if cfg.TLS != nil {
		s.tls = cfg.TLS.Clone()
	} else {
		s.tls = &tls.Config{}
	}
	if s.tls.ServerName == "" {
		s.tls.ServerName = host
	}
	if s.tls.MinVersion == 0 {
		s.tls.MinVersion = tls.VersionTLS12
	}
	return s, nil
}

// ParseMailbox parses one RFC 5322 mailbox, refusing line breaks.
func ParseMailbox(s string) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("mailbox contains a line break")
	}
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("invalid mailbox %q: %w", s, err)
	}
	return a, nil
}

// From is the default sender, or nil when Config.From was empty.
func (s *Sender) From() *mail.Address {
	if s.from == nil {
		return nil
	}
	a := *s.from
	return &a
}

// Send delivers m. An error from the server wraps its *textproto.Error, whose
// Code tells a temporary (4xx) refusal from a permanent (5xx) one.
func (s *Sender) Send(ctx context.Context, m Message) error {
	from := m.From
	if from == nil {
		from = s.from
	}
	if from == nil {
		return errors.New("smtp: the message has no sender and Config.From is empty")
	}
	if strings.ContainsAny(from.Name, "\r\n") {
		return errors.New("smtp: from contains a line break")
	}
	if _, err := ParseMailbox(from.Address); err != nil {
		return fmt.Errorf("smtp: from: %w", err)
	}
	to, err := ParseMailbox(m.To)
	if err != nil {
		return fmt.Errorf("smtp: to: %w", err)
	}
	body, err := compose(from, to, m, time.Now())
	if err != nil {
		return err
	}
	return s.session(ctx, func(c *smtp.Client) error {
		if err := c.Mail(from.Address); err != nil {
			return fmt.Errorf("smtp: MAIL FROM: %w", err)
		}
		if err := c.Rcpt(to.Address); err != nil {
			return fmt.Errorf("smtp: RCPT TO: %w", err)
		}
		w, err := c.Data()
		if err != nil {
			return fmt.Errorf("smtp: DATA: %w", err)
		}
		if _, err := w.Write(body); err != nil {
			_ = w.Close()
			return fmt.Errorf("smtp: DATA: %w", err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("smtp: DATA: %w", err)
		}
		return nil
	})
}

// CheckHealth connects, negotiates TLS and authenticates, then quits without
// sending: nil when the server would accept this Sender's mail.
func (s *Sender) CheckHealth(ctx context.Context) error {
	return s.session(ctx, func(*smtp.Client) error { return nil })
}

// session dials, secures and authenticates a connection, runs do on it and
// quits, all within the timeout and ctx.
func (s *Sender) session(ctx context.Context, do func(*smtp.Client) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if s.implicit {
		tc := tls.Client(conn, s.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			return s.fail(ctx, "TLS", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return s.fail(ctx, "greeting", err)
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return s.fail(ctx, "EHLO", err)
	}
	secure := s.implicit
	if !secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(s.tls); err != nil {
				return s.fail(ctx, "STARTTLS", err)
			}
			secure = true
		}
	}
	if s.username != "" {
		if !secure && !loopback(s.host) {
			return fmt.Errorf("smtp: %s offers no STARTTLS; refusing to send credentials in cleartext (use port 465 or a server with STARTTLS)", s.addr)
		}
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return fmt.Errorf("smtp: %s offers no AUTH", s.addr)
		}
		auth, err := s.auth(mechs)
		if err != nil {
			return err
		}
		if err := c.Auth(auth); err != nil {
			return s.fail(ctx, "AUTH", err)
		}
	}
	if err := do(c); err != nil {
		return s.cause(ctx, err)
	}
	_ = c.Quit()
	return nil
}

// fail names the failed step, preferring the context's error when the
// timeout or the caller ended the exchange.
func (s *Sender) fail(ctx context.Context, step string, err error) error {
	return s.cause(ctx, fmt.Errorf("smtp: %s: %w", step, err))
}

func (s *Sender) cause(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w (%w)", err, ctx.Err())
	}
	return err
}

func (s *Sender) auth(mechs string) (smtp.Auth, error) {
	offered := strings.Fields(strings.ToUpper(mechs))
	for _, m := range offered {
		if m == "PLAIN" {
			return plainAuth{s.username, s.password}, nil
		}
	}
	for _, m := range offered {
		if m == "LOGIN" {
			return &loginAuth{username: s.username, password: s.password}, nil
		}
	}
	return nil, fmt.Errorf("smtp: %s offers no supported AUTH mechanism (PLAIN or LOGIN), only %q", s.addr, mechs)
}

// plainAuth is RFC 4616 PLAIN; session decides when sending it is safe.
type plainAuth struct{ username, password string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

// loginAuth is the LOGIN mechanism: the username, then the password.
type loginAuth struct {
	username, password string
	step               int
}

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	}
	return nil, errors.New("unexpected server challenge")
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// compose renders m as an RFC 5322 message with CRLF line endings.
func compose(from, to *mail.Address, m Message, now time.Time) ([]byte, error) {
	subject := strings.TrimSpace(m.Subject)
	if subject == "" {
		return nil, errors.New("smtp: subject is required")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return nil, errors.New("smtp: subject contains a line break")
	}
	hasText, hasHTML := strings.TrimSpace(m.Text) != "", strings.TrimSpace(m.HTML) != ""
	if !hasText && !hasHTML {
		return nil, errors.New("smtp: a text or HTML body is required")
	}
	id, err := messageID(from.Address)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	header := func(name, value string) {
		b.WriteString(fold(name + ": " + value))
		b.WriteString("\r\n")
	}
	header("From", from.String())
	header("To", to.String())
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", id)
	header("MIME-Version", "1.0")

	part := func(w *bytes.Buffer, body string) error {
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(body)); err != nil {
			return err
		}
		return qp.Close()
	}
	if !hasText || !hasHTML {
		kind, body := "text/plain", m.Text
		if hasHTML {
			kind, body = "text/html", m.HTML
		}
		header("Content-Type", kind+"; charset=utf-8")
		header("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := part(&b, body); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	var parts bytes.Buffer
	mw := multipart.NewWriter(&parts)
	for _, p := range []struct{ kind, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {p.kind + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		var enc bytes.Buffer
		if err := part(&enc, p.body); err != nil {
			return nil, err
		}
		if _, err := w.Write(enc.Bytes()); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	header("Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": mw.Boundary()}))
	b.WriteString("\r\n")
	b.Write(parts.Bytes())
	return b.Bytes(), nil
}

func messageID(from string) (string, error) {
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	domain := "localhost"
	if at := strings.LastIndexByte(from, '@'); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}
	return "<" + hex.EncodeToString(r[:]) + "@" + domain + ">", nil
}

// fold breaks a header line longer than 78 characters at spaces (RFC 5322
// §2.2.3); a run without spaces stays whole.
func fold(line string) string {
	const limit = 78
	if len(line) <= limit {
		return line
	}
	var b strings.Builder
	for len(line) > limit {
		i := strings.LastIndexByte(line[:limit], ' ')
		if i <= 0 {
			if i = strings.IndexByte(line[limit:], ' '); i < 0 {
				break
			}
			i += limit
		}
		b.WriteString(line[:i])
		b.WriteString("\r\n")
		line = line[i:]
	}
	b.WriteString(line)
	return b.String()
}
