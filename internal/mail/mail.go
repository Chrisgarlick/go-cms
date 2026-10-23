// Package mail sends plain-text email via SMTP, or logs it in development.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
	// ReplyTo is optional (e.g. the sender of a form submission).
	ReplyTo string
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

// clean rejects header injection: no CR/LF in header values, valid addresses.
func clean(m Message, from string) (string, error) {
	for _, v := range []string{m.To, m.Subject, m.ReplyTo, from} {
		if strings.ContainsAny(v, "\r\n") {
			return "", errors.New("mail: header contains a line break")
		}
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return "", fmt.Errorf("mail: bad recipient: %w", err)
	}
	if m.ReplyTo != "" {
		if _, err := mail.ParseAddress(m.ReplyTo); err != nil {
			return "", fmt.Errorf("mail: bad reply-to: %w", err)
		}
	}
	return to.Address, nil
}

func build(from, to, replyTo, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	if replyTo != "" {
		if a, err := mail.ParseAddress(replyTo); err == nil {
			b.WriteString("Reply-To: " + a.String() + "\r\n")
		}
	}
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}

// SMTP sends through a relay. STARTTLS is required unless the relay is on loopback.
type SMTP struct {
	Addr     string
	From     string
	Username string
	Password string
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	to, err := clean(m, s.From)
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	loopback := host == "localhost" || net.ParseIP(host).IsLoopback()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	} else if !loopback {
		return errors.New("mail: relay does not offer STARTTLS")
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return err
		}
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("mail: bad from address: %w", err)
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(build(s.From, to, m.ReplyTo, m.Subject, m.Text)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Log writes messages to the logger instead of sending (development, tests).
type Log struct {
	Logger *slog.Logger
	mu     sync.Mutex
	Sent   []Message
}

func (l *Log) Send(_ context.Context, m Message) error {
	if _, err := clean(m, "dev@localhost"); err != nil {
		return err
	}
	l.mu.Lock()
	l.Sent = append(l.Sent, m)
	l.mu.Unlock()
	if l.Logger != nil {
		l.Logger.Info("email (not sent: no SMTP configured)", "to", m.To, "subject", m.Subject, "body", m.Text)
	}
	return nil
}

func (l *Log) Last() (Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.Sent) == 0 {
		return Message{}, false
	}
	return l.Sent[len(l.Sent)-1], true
}
