package smtp

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"time"
)

const (
	// DefaultTimeout for SMTP operations
	DefaultTimeout = 30 * time.Second
)

// Auth represents authentication credentials
type Auth interface {
	// Method returns the SASL method name (e.g., "PLAIN", "XOAUTH2")
	Method() string
	// Credentials returns the auth credentials for the given host
	Credentials(host string) ([]byte, error)
}

// PasswordAuth implements username/password authentication (PLAIN)
type PasswordAuth struct {
	Username string
	Password string
}

func (a *PasswordAuth) Method() string {
	return "PLAIN"
}

func (a *PasswordAuth) Credentials(host string) ([]byte, error) {
	// PLAIN auth: \0username\0password
	return []byte("\x00" + a.Username + "\x00" + a.Password), nil
}

// OAuth2Auth implements XOAUTH2 authentication
type OAuth2Auth struct {
	Email       string
	AccessToken string
}

func (a *OAuth2Auth) Method() string {
	return "XOAUTH2"
}

func (a *OAuth2Auth) Credentials(host string) ([]byte, error) {
	// XOAUTH2 format: user=<email>\x01auth=Bearer <token>\x01\x01
	authStr := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.Email, a.AccessToken)
	return []byte(authStr), nil
}

// xoauth2Auth wraps OAuth2Auth to implement smtp.Auth interface
type xoauth2Auth struct {
	email       string
	accessToken string
}

func (a *xoauth2Auth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	authStr := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.email, a.accessToken)
	return "XOAUTH2", []byte(authStr), nil
}

func (a *xoauth2Auth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		// Server wants more data - send empty response to get error details
		return []byte{}, nil
	}
	return nil, nil
}

// Client represents an SMTP client
type Client struct {
	Host    string
	Port    int
	Auth    Auth
	Timeout time.Duration
}

// NewClient creates a new SMTP client
func NewClient(host string, port int, auth Auth) *Client {
	return &Client{
		Host:    host,
		Port:    port,
		Auth:    auth,
		Timeout: DefaultTimeout,
	}
}

// Send sends an email message
func (c *Client) Send(msg *Message) error {
	addr := fmt.Sprintf("%s:%d", c.Host, c.Port)

	// Connect with timeout — try IPv4 first, fall back to IPv6
	conn, err := net.DialTimeout("tcp4", addr, c.Timeout)
	if err != nil {
		conn, err = net.DialTimeout("tcp6", addr, c.Timeout)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", addr, err)
		}
	}
	defer conn.Close()

	// Set read/write deadline
	conn.SetDeadline(time.Now().Add(c.Timeout))

	var client *smtp.Client

	if c.Port == 465 {
		// Port 465: implicit TLS — wrap connection in TLS before SMTP handshake
		slog.Debug("Using implicit TLS", "module", "SMTP", "host", c.Host, "port", c.Port)
		tlsConn := tls.Client(conn, &tls.Config{ServerName: c.Host})
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("TLS handshake failed: %w", err)
		}
		client, err = smtp.NewClient(tlsConn, c.Host)
	} else {
		client, err = smtp.NewClient(conn, c.Host)
	}
	if err != nil {
		return fmt.Errorf("failed to create SMTP client: %w", err)
	}
	defer client.Close()

	// Say hello with sender's domain (some providers like GMX reject "localhost")
	ehloHost := "localhost"
	if from, err := ParseAddress(msg.From); err == nil {
		if at := strings.LastIndex(from, "@"); at != -1 {
			ehloHost = from[at+1:]
		}
	}
	if err := client.Hello(ehloHost); err != nil {
		return fmt.Errorf("HELO failed: %w", err)
	}

	// Require STARTTLS for all non-implicit-TLS connections
	if c.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			config := &tls.Config{
				ServerName: c.Host,
			}
			if err := client.StartTLS(config); err != nil {
				return fmt.Errorf("STARTTLS failed: %w", err)
			}
			slog.Debug("STARTTLS negotiated", "module", "SMTP", "host", c.Host, "port", c.Port)
		} else {
			return fmt.Errorf("server does not support STARTTLS (port %d)", c.Port)
		}
	}

	// Authenticate
	if c.Auth != nil {
		slog.Debug("Authenticating", "module", "SMTP", "method", c.Auth.Method())
		var smtpAuth smtp.Auth

		switch a := c.Auth.(type) {
		case *OAuth2Auth:
			smtpAuth = &xoauth2Auth{
				email:       a.Email,
				accessToken: a.AccessToken,
			}
		case *PasswordAuth:
			smtpAuth = smtp.PlainAuth("", a.Username, a.Password, c.Host)
		default:
			return fmt.Errorf("unsupported auth type")
		}

		if err := client.Auth(smtpAuth); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
	}

	// Parse From address
	from, err := ParseAddress(msg.From)
	if err != nil {
		return fmt.Errorf("invalid From address: %w", err)
	}

	// Set sender. Bypass smtp.Client.Mail() because Go's stdlib unconditionally
	// appends "BODY=8BITMIME" and "SMTPUTF8" to MAIL FROM when the server
	// advertises those extensions in EHLO. Microsoft 365's SMTP service has
	// shipped (mid-2026) tenant-scoped routing where some submission backends
	// advertise both extensions in EHLO but then reject MAIL FROM with the
	// parameters present, returning '502 5.3.3 Command not implemented'. The
	// same session, same auth, sending a bare 'MAIL FROM:<email>' is accepted.
	// Apple Mail and other clients that don't auto-append these extension
	// parameters are unaffected. Our message bodies are quoted-printable or
	// base64 (always 7-bit clean) and our addresses are ASCII, so dropping
	// both extension declarations is a no-op for content semantics.
	if err := mailFromPlain(client, from); err != nil {
		return fmt.Errorf("MAIL FROM failed: %w", err)
	}

	// Set recipients (extract bare email from "Name <email>" format)
	for _, to := range msg.Recipients() {
		rcpt, err := ParseAddress(to)
		if err != nil {
			return fmt.Errorf("invalid recipient address %q: %w", to, err)
		}
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO failed for %s: %w", rcpt, err)
		}
	}

	// Send message data
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA failed: %w", err)
	}

	// Build and write message
	data, err := msg.Build()
	if err != nil {
		wc.Close()
		return fmt.Errorf("failed to build message: %w", err)
	}

	if _, err := wc.Write(data); err != nil {
		wc.Close()
		return fmt.Errorf("failed to write message: %w", err)
	}

	if err := wc.Close(); err != nil {
		return fmt.Errorf("failed to complete message: %w", err)
	}

	// Quit gracefully
	client.Quit()

	return nil
}

// Send is a convenience function to send an email
func Send(host string, port int, auth Auth, msg *Message) error {
	client := NewClient(host, port, auth)
	return client.Send(msg)
}

// buildXOAuth2String builds the XOAUTH2 authentication string
func buildXOAuth2String(email, accessToken string) string {
	return base64.StdEncoding.EncodeToString(
		[]byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", email, accessToken)),
	)
}

// mailFromPlain sends 'MAIL FROM:<email>' without 8BITMIME or SMTPUTF8 extension
// parameters via the client's exported textproto.Conn. See the call site for
// the M365 motivation; this helper does the minimal SMTP roundtrip and asserts
// a 250 response, mirroring what smtp.Client.Mail does without the extension
// auto-append.
func mailFromPlain(c *smtp.Client, from string) error {
	if strings.ContainsAny(from, "\r\n") {
		return fmt.Errorf("MAIL FROM address contains CR or LF")
	}
	id, err := c.Text.Cmd("MAIL FROM:<%s>", from)
	if err != nil {
		return err
	}
	c.Text.StartResponse(id)
	defer c.Text.EndResponse(id)
	_, _, err = c.Text.ReadResponse(250)
	return err
}

// SMTPError represents an SMTP error with code
type SMTPError struct {
	Code    int
	Message string
}

func (e *SMTPError) Error() string {
	return fmt.Sprintf("SMTP error %d: %s", e.Code, e.Message)
}

// IsTemporary returns true if the error is temporary (4xx)
func (e *SMTPError) IsTemporary() bool {
	return e.Code >= 400 && e.Code < 500
}

// IsPermanent returns true if the error is permanent (5xx)
func (e *SMTPError) IsPermanent() bool {
	return e.Code >= 500 && e.Code < 600
}

// ParseSMTPError parses an SMTP error response
func ParseSMTPError(err error) *SMTPError {
	if err == nil {
		return nil
	}

	msg := err.Error()

	// Try to extract code from error message
	var code int
	if len(msg) >= 3 {
		fmt.Sscanf(msg[:3], "%d", &code)
	}

	if code == 0 {
		// Check for common error patterns
		if strings.Contains(msg, "authentication") || strings.Contains(msg, "AUTH") {
			code = 535 // Auth failed
		} else if strings.Contains(msg, "connection") {
			code = 421 // Service not available
		} else {
			code = 500 // Generic error
		}
	}

	return &SMTPError{
		Code:    code,
		Message: msg,
	}
}
