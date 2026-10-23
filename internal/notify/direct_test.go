package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is a minimal in-process SMTP server: enough of RFC 5321 for
// net/smtp.SendMail without auth or STARTTLS. It records each delivered
// message's envelope and data.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []fakeSMTPMessage
}

type fakeSMTPMessage struct {
	from string
	rcpt []string
	data string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{ln: ln}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort() (string, string) {
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	return host, port
}

func (f *fakeSMTP) messages() []fakeSMTPMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSMTPMessage(nil), f.msgs...)
}

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	var cur fakeSMTPMessage
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			w("250 fake")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			cur = fakeSMTPMessage{from: strings.Trim(cmd[len("MAIL FROM:"):], "<> ")}
			w("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			cur.rcpt = append(cur.rcpt, strings.Trim(cmd[len("RCPT TO:"):], "<> "))
			w("250 ok")
		case upper == "DATA":
			w("354 go ahead")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(l)
			}
			cur.data = data.String()
			f.mu.Lock()
			f.msgs = append(f.msgs, cur)
			f.mu.Unlock()
			w("250 queued")
		case upper == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

// TestSendDirectEmail_AddressesOnlyTheRecipient: a direct email goes to the
// one named recipient — never the deployment-wide EmailTo list — ignores the
// On filter, and carries both body parts under the given subject.
func TestSendDirectEmail_AddressesOnlyTheRecipient(t *testing.T) {
	srv := newFakeSMTP(t)
	host, port := srv.hostPort()
	n := New(Config{
		SMTPHost: host, SMTPPort: port, SMTPFrom: "fleet@example.com",
		EmailTo: []string{"ops@example.com"}, On: []string{"failure"}, Retries: -1,
	})
	err := n.SendDirectEmail(context.Background(), " owner@example.com ", "Fleet: 2 scheduled tasks need attention\r\nBcc: x@evil", "plain body", "<p>html body</p>")
	if err != nil {
		t.Fatalf("SendDirectEmail: %v", err)
	}
	msgs := srv.messages()
	if len(msgs) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.from != "fleet@example.com" || len(m.rcpt) != 1 || m.rcpt[0] != "owner@example.com" {
		t.Fatalf("envelope from=%q rcpt=%q, want the owner only", m.from, m.rcpt)
	}
	for _, want := range []string{
		"To: owner@example.com\r\n",
		"Subject: Fleet: 2 scheduled tasks need attention  Bcc: x@evil\r\n",
		"plain body", "<p>html body</p>", "multipart/alternative",
	} {
		if !strings.Contains(m.data, want) {
			t.Errorf("message missing %q:\n%s", want, m.data)
		}
	}
	if strings.Contains(m.data, "ops@example.com") {
		t.Error("a direct email must not name the deployment-wide recipients")
	}
}

// TestSendDirectEmail_NotConfigured: without an SMTP host or From address the
// send is refused with ErrDirectEmailNotConfigured (the owner digest's
// logged no-op), and a recipient carrying CR/LF is refused outright.
func TestSendDirectEmail_NotConfigured(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{SMTPHost: "smtp.example.com"},
		{SMTPFrom: "fleet@example.com"},
	} {
		if err := New(cfg).SendDirectEmail(context.Background(), "o@example.com", "s", "t", "h"); !errors.Is(err, ErrDirectEmailNotConfigured) {
			t.Errorf("cfg %+v: err = %v, want ErrDirectEmailNotConfigured", cfg, err)
		}
		if cfg.DirectEmailConfigured() {
			t.Errorf("cfg %+v reports DirectEmailConfigured", cfg)
		}
	}
	var nilNotifier *Notifier
	if err := nilNotifier.SendDirectEmail(context.Background(), "o@example.com", "s", "t", "h"); !errors.Is(err, ErrDirectEmailNotConfigured) {
		t.Errorf("nil notifier: %v", err)
	}
	ok := New(Config{SMTPHost: "127.0.0.1", SMTPPort: "1", SMTPFrom: "fleet@example.com"})
	if !ok.Config().DirectEmailConfigured() {
		t.Fatal("host + from must count as configured")
	}
	if err := ok.SendDirectEmail(context.Background(), "o@example.com\r\nRCPT TO:<x@evil>", "s", "t", "h"); err == nil || errors.Is(err, ErrDirectEmailNotConfigured) {
		t.Errorf("CR/LF recipient: err = %v, want a refusal", err)
	}
}
