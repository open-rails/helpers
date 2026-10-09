package smtp_test

import (
	"context"
	"errors"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/helpers/smtp"
	"github.com/open-rails/helpers/smtp/smtptest"
)

const password = "s3cret-api-key"

func sender(t *testing.T, srv *smtptest.Server, cfg smtp.Config) *smtp.Sender {
	t.Helper()
	cfg.Host, cfg.Port, cfg.TLS = srv.Host, srv.Port, srv.ClientTLS()
	if cfg.From == "" {
		cfg.From = "Café Billing <billing@shop.example>"
	}
	s, err := smtp.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSendDeliversOverEachTransport(t *testing.T) {
	cases := map[string]struct {
		opts smtptest.Options
		user string
		tls  bool
	}{
		"STARTTLS with credentials": {smtptest.Options{Username: "apikey", Password: password, STARTTLS: true}, "apikey", true},
		"LOGIN only":                {smtptest.Options{Username: "apikey", Password: password, STARTTLS: true, Mechanisms: []string{"LOGIN"}}, "apikey", true},
		"implicit TLS":              {smtptest.Options{Username: "apikey", Password: password, ImplicitTLS: true}, "apikey", true},
		"loopback without TLS":      {smtptest.Options{Username: "apikey", Password: password}, "apikey", false},
		"relay without credentials": {smtptest.Options{}, "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := smtptest.Start(t, tc.opts)
			cfg := smtp.Config{Username: tc.user}
			if tc.user != "" {
				cfg.Password = password
			}
			s := sender(t, srv, cfg)
			if tc.opts.ImplicitTLS {
				s = implicit(t, srv, cfg)
			}
			if err := s.CheckHealth(context.Background()); err != nil {
				t.Fatalf("health: %v", err)
			}
			err := s.Send(context.Background(), smtp.Message{
				To: "Zoë <zoe@example.com>", Subject: "Votre reçu — order 42",
				Text: "Total: 23 USD\n.leading dot\n", HTML: "<p>Total: <b>23 USD</b></p>",
			})
			if err != nil {
				t.Fatal(err)
			}
			m := srv.Wait(t, 1, 5*time.Second)[0]
			if m.Username != tc.user || m.TLS != tc.tls {
				t.Fatalf("session: user %q tls %v", m.Username, m.TLS)
			}
			if m.From != "billing@shop.example" || len(m.To) != 1 || m.To[0] != "zoe@example.com" {
				t.Fatalf("envelope: %q -> %q", m.From, m.To)
			}
			from, err := m.Header.AddressList("From")
			if err != nil || from[0].Name != "Café Billing" {
				t.Fatalf("From header %q: %v", m.Header.Get("From"), err)
			}
			if m.Subject != "Votre reçu — order 42" {
				t.Fatalf("subject %q", m.Subject)
			}
			if m.Text != "Total: 23 USD\n.leading dot\n" || m.HTML != "<p>Total: <b>23 USD</b></p>" {
				t.Fatalf("bodies %q / %q", m.Text, m.HTML)
			}
			if id := m.Header.Get("Message-Id"); !strings.HasSuffix(id, "@shop.example>") {
				t.Fatalf("Message-ID %q", id)
			}
		})
	}
}

// implicit is the sender as if srv listened on port 465.
func implicit(t *testing.T, srv *smtptest.Server, cfg smtp.Config) *smtp.Sender {
	t.Helper()
	cfg.Host, cfg.Port, cfg.TLS, cfg.From = srv.Host, srv.Port, srv.ClientTLS(), "Café Billing <billing@shop.example>"
	s, err := smtp.NewImplicitTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSingleBodyAndMessageFrom(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{})
	s := sender(t, srv, smtp.Config{})
	ctx := context.Background()
	if err := s.Send(ctx, smtp.Message{To: "a@example.com", Subject: "text", Text: "only text"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, smtp.Message{From: &mail.Address{Name: "Merchant", Address: "hi@merchant.example"}, To: "a@example.com", Subject: strings.Repeat("long subject ", 20), HTML: "<p>only html</p>"}); err != nil {
		t.Fatal(err)
	}
	got := srv.Wait(t, 2, 5*time.Second)
	if got[0].Text != "only text\n" || got[0].HTML != "" || got[0].Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("text-only: %q %q %+v", got[0].Text, got[0].HTML, got[0].Header)
	}
	if got[1].HTML != "<p>only html</p>\n" || got[1].Text != "" || got[1].From != "hi@merchant.example" {
		t.Fatalf("html-only from merchant: %q %q", got[1].From, got[1].HTML)
	}
	if got[1].Subject != strings.TrimSpace(strings.Repeat("long subject ", 20)) {
		t.Fatalf("folded subject %q", got[1].Subject)
	}
	for _, line := range strings.Split(string(got[1].Raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line of %d characters", len(line))
		}
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("wrong password names no secret", func(t *testing.T) {
		srv := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: password, STARTTLS: true})
		s := sender(t, srv, smtp.Config{Username: "apikey", Password: "wrong-" + password})
		for _, err := range []error{s.CheckHealth(ctx), s.Send(ctx, smtp.Message{To: "a@example.com", Subject: "s", Text: "t"})} {
			var tp *textproto.Error
			if !errors.As(err, &tp) || tp.Code != 535 || strings.Contains(err.Error(), password) {
				t.Fatalf("got %v", err)
			}
		}
		if n := len(srv.Messages()); n != 0 {
			t.Fatalf("%d messages delivered", n)
		}
	})
	t.Run("refused recipient keeps its code", func(t *testing.T) {
		srv := smtptest.Start(t, smtptest.Options{RefuseRecipient: func(string) string { return "550 5.1.1 no such user" }})
		err := sender(t, srv, smtp.Config{}).Send(ctx, smtp.Message{To: "a@example.com", Subject: "s", Text: "t"})
		var tp *textproto.Error
		if !errors.As(err, &tp) || tp.Code != 550 {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no credentials without TLS off loopback", func(t *testing.T) {
		ip := nonLoopback(t)
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
		if err != nil {
			t.Skipf("listen on %s: %v", ip, err)
		}
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			tc := textproto.NewConn(c)
			_ = tc.PrintfLine("220 plain")
			for {
				line, err := tc.ReadLine()
				if err != nil {
					return
				}
				if strings.HasPrefix(strings.ToUpper(line), "EHLO") {
					_ = tc.PrintfLine("250-plain")
					_ = tc.PrintfLine("250 AUTH PLAIN LOGIN")
					continue
				}
				if strings.HasPrefix(strings.ToUpper(line), "AUTH") {
					t.Errorf("credentials sent in cleartext: %q", line)
				}
				_ = tc.PrintfLine("221 bye")
				return
			}
		}()
		t.Cleanup(func() { _ = ln.Close() })
		port := ln.Addr().(*net.TCPAddr).Port
		s, err := smtp.New(smtp.Config{Host: ip, Port: port, Username: "apikey", Password: password, From: "a@shop.example"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CheckHealth(ctx); err == nil || !strings.Contains(err.Error(), "cleartext") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("outage fails health until it ends", func(t *testing.T) {
		srv := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: password, STARTTLS: true})
		s := sender(t, srv, smtp.Config{Username: "apikey", Password: password})
		srv.Outage("421 4.3.2 service unavailable")
		var tp *textproto.Error
		if err := s.CheckHealth(ctx); !errors.As(err, &tp) || tp.Code != 421 {
			t.Fatalf("got %v", err)
		}
		srv.Outage("")
		if err := s.CheckHealth(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("header injection", func(t *testing.T) {
		srv := smtptest.Start(t, smtptest.Options{})
		s := sender(t, srv, smtp.Config{})
		for _, m := range []smtp.Message{
			{To: "a@example.com", Subject: "hi\r\nBcc: x@evil.example", Text: "t"},
			{To: "a@example.com\r\nBcc: x@evil.example", Subject: "hi", Text: "t"},
			{From: &mail.Address{Name: "x\r\nBcc: y", Address: "a@shop.example"}, To: "a@example.com", Subject: "hi", Text: "t"},
			{From: &mail.Address{Name: "No address"}, To: "a@example.com", Subject: "hi", Text: "t"},
			{To: "a@example.com", Subject: "", Text: "t"},
			{To: "a@example.com", Subject: "no body"},
		} {
			if err := s.Send(ctx, m); err == nil {
				t.Fatalf("sent %+v", m)
			}
		}
		if n := len(srv.Messages()); n != 0 {
			t.Fatalf("%d messages delivered", n)
		}
	})
}

func TestHungServerIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	s, err := smtp.New(smtp.Config{Host: "127.0.0.1", Port: port, From: "a@shop.example", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Send(context.Background(), smtp.Message{To: "b@example.com", Subject: "s", Text: "t"}); err == nil {
		t.Fatal("sent to a server that never answered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.CheckHealth(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("health: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s", took)
	}
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []smtp.Config{
		{},
		{Host: "mail.example", Port: 70000},
		{Host: "mail.example", Username: "u"},
		{Host: "mail.example", Password: "p"},
		{Host: "mail.example", From: "not a mailbox"},
		{Host: "mail.example", From: "a@b.example\r\nBcc: c@d.example"},
	} {
		if _, err := smtp.New(cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	s, err := smtp.New(smtp.Config{Host: "smtp.sendgrid.net", Username: "apikey", Password: "SG.x", From: "Shop <noreply@shop.example>"})
	if err != nil {
		t.Fatal(err)
	}
	if f := s.From(); f.Name != "Shop" || f.Address != "noreply@shop.example" {
		t.Fatalf("from %+v", f)
	}
}

func nonLoopback(t *testing.T) string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return ""
}
