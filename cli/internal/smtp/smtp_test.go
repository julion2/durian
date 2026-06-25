package smtp

import (
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestMessageBuild(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "Test Subject",
		Body:    "Hello, World!",
	}

	data, err := msg.Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	content := string(data)

	// Check required headers
	requiredHeaders := []string{
		"From: sender@example.com",
		"To: recipient@example.com",
		"Subject: Test Subject",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
	}

	for _, header := range requiredHeaders {
		if !strings.Contains(content, header) {
			t.Errorf("Missing header: %s\nContent:\n%s", header, content)
		}
	}

	// Check Message-ID present
	if !strings.Contains(content, "Message-ID: <") {
		t.Error("Missing Message-ID header")
	}

	// Check Date present
	if !strings.Contains(content, "Date: ") {
		t.Error("Missing Date header")
	}

	// Check body present
	if !strings.Contains(content, "Hello") {
		t.Error("Body not found in message")
	}
}

func TestMessageBuildWithAttachment(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "Test with Attachment",
		Body:    "See attachment.",
		Attachments: []Attachment{
			{
				Filename: "test.txt",
				Data:     []byte("Hello from attachment!"),
				MIMEType: "text/plain",
			},
		},
	}

	data, err := msg.Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	content := string(data)

	// Check multipart header
	if !strings.Contains(content, "Content-Type: multipart/mixed; boundary=") {
		t.Error("Missing multipart/mixed Content-Type")
	}

	// Check attachment present
	if !strings.Contains(content, "Content-Disposition: attachment; filename=\"test.txt\"") {
		t.Error("Missing attachment Content-Disposition")
	}
}

func TestMessageBuildHTML(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "HTML Newsletter",
		Body:    "<b>Hello!</b> Test content here.",
		IsHTML:  true,
	}

	data, err := msg.Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	content := string(data)

	// Check for HTML content type
	if !strings.Contains(content, "Content-Type: text/html; charset=UTF-8") {
		t.Error("Missing text/html Content-Type")
	}

	// Should NOT have text/plain
	if strings.Contains(content, "Content-Type: text/plain") {
		t.Error("HTML message should not have text/plain Content-Type")
	}

	// Check body present (strip QP soft breaks for substring matching)
	decoded := strings.ReplaceAll(content, "=\r\n", "")
	if !strings.Contains(decoded, "Hello!") || !strings.Contains(decoded, "Test content here.") {
		t.Error("HTML body content not found in message")
	}
}

func TestMessageBuildHTMLWithAttachment(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "HTML with Attachment",
		Body:    "<p>See attached file.</p>",
		IsHTML:  true,
		Attachments: []Attachment{
			{
				Filename: "doc.pdf",
				Data:     []byte("fake pdf content"),
				MIMEType: "application/pdf",
			},
		},
	}

	data, err := msg.Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	content := string(data)

	// Check multipart header
	if !strings.Contains(content, "Content-Type: multipart/mixed; boundary=") {
		t.Error("Missing multipart/mixed Content-Type")
	}

	// Check HTML content type for body part
	if !strings.Contains(content, "Content-Type: text/html; charset=UTF-8") {
		t.Error("Missing text/html Content-Type for body part")
	}

	// Check attachment
	if !strings.Contains(content, "Content-Disposition: attachment; filename=\"doc.pdf\"") {
		t.Error("Missing attachment Content-Disposition")
	}
}

func TestMessageBuildUTF8Subject(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "Test mit Umlauten: äöü",
		Body:    "Hello!",
	}

	data, err := msg.Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	content := string(data)

	// Subject should be encoded (RFC 2047)
	if strings.Contains(content, "Subject: Test mit Umlauten: äöü\r\n") {
		t.Error("UTF-8 subject should be encoded")
	}
	if !strings.Contains(content, "Subject: =?UTF-8?") {
		t.Error("Subject should be RFC 2047 encoded")
	}
}

func TestParseAddress(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"test@example.com", "test@example.com", false},
		{"  test@example.com  ", "test@example.com", false},
		{"Test User <test@example.com>", "test@example.com", false},
		{"\"Test User\" <test@example.com>", "test@example.com", false},
		{"", "", true},
		{"not-an-email", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseAddress(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseAddress(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseAddress(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseAddressList(t *testing.T) {
	tests := []struct {
		input   string
		want    []string
		wantErr bool
	}{
		{"a@example.com", []string{"a@example.com"}, false},
		{"a@example.com,b@example.com", []string{"a@example.com", "b@example.com"}, false},
		{"a@example.com, b@example.com , c@example.com", []string{"a@example.com", "b@example.com", "c@example.com"}, false},
		{"", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseAddressList(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseAddressList(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("ParseAddressList(%q) = %v, want %v", tt.input, got, tt.want)
					return
				}
				for i, addr := range got {
					if addr != tt.want[i] {
						t.Errorf("ParseAddressList(%q)[%d] = %q, want %q", tt.input, i, addr, tt.want[i])
					}
				}
			}
		})
	}
}

func TestReadBody(t *testing.T) {
	input := `Hello World!

This is a test message.

# This is a comment
# And another comment

End of message.`

	body, err := ReadBody(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ReadBody() error: %v", err)
	}

	// Should not contain comment lines
	if strings.Contains(body, "# This is a comment") {
		t.Error("Body should not contain comment lines")
	}

	// Should contain actual content
	if !strings.Contains(body, "Hello World!") {
		t.Error("Body missing 'Hello World!'")
	}
	if !strings.Contains(body, "End of message.") {
		t.Error("Body missing 'End of message.'")
	}
}

func TestLoadAttachment(t *testing.T) {
	// Test loading non-existent file
	_, err := LoadAttachment("/nonexistent/file.pdf")
	if err == nil {
		t.Error("LoadAttachment() should fail for non-existent file")
	}
}

func TestOAuth2AuthCredentials(t *testing.T) {
	auth := &OAuth2Auth{
		Email:       "test@example.com",
		AccessToken: "token123",
	}

	if auth.Method() != "XOAUTH2" {
		t.Errorf("Method() = %q, want %q", auth.Method(), "XOAUTH2")
	}

	creds, err := auth.Credentials("")
	if err != nil {
		t.Fatalf("Credentials() error: %v", err)
	}

	expected := "user=test@example.com\x01auth=Bearer token123\x01\x01"
	if string(creds) != expected {
		t.Errorf("Credentials() = %q, want %q", string(creds), expected)
	}
}

func TestPasswordAuthCredentials(t *testing.T) {
	auth := &PasswordAuth{
		Username: "user",
		Password: "pass",
	}

	if auth.Method() != "PLAIN" {
		t.Errorf("Method() = %q, want %q", auth.Method(), "PLAIN")
	}

	creds, err := auth.Credentials("")
	if err != nil {
		t.Fatalf("Credentials() error: %v", err)
	}

	expected := "\x00user\x00pass"
	if string(creds) != expected {
		t.Errorf("Credentials() = %q, want %q", string(creds), expected)
	}
}

func TestRecipients(t *testing.T) {
	msg := &Message{
		From: "sender@example.com",
		To:   []string{"a@example.com", "b@example.com"},
	}

	recipients := msg.Recipients()
	if len(recipients) != 2 {
		t.Errorf("Recipients() length = %d, want 2", len(recipients))
	}
}

func TestBuild_MessageIDCached(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"recipient@example.com"},
		Subject: "Test",
		Body:    "Hello",
	}

	data1, err := msg.Build()
	if err != nil {
		t.Fatalf("first Build(): %v", err)
	}
	if msg.GeneratedMessageID == "" {
		t.Fatal("GeneratedMessageID not set after Build()")
	}
	firstID := msg.GeneratedMessageID

	data2, err := msg.Build()
	if err != nil {
		t.Fatalf("second Build(): %v", err)
	}
	if msg.GeneratedMessageID != firstID {
		t.Errorf("Message-ID changed: %q -> %q", firstID, msg.GeneratedMessageID)
	}

	// Both builds should contain the same Message-ID header
	id1 := extractHeader(string(data1), "Message-ID")
	id2 := extractHeader(string(data2), "Message-ID")
	if id1 != id2 {
		t.Errorf("Message-ID header differs: %q vs %q", id1, id2)
	}
}

func TestBuild_MessageIDPreset(t *testing.T) {
	msg := &Message{
		From:               "sender@example.com",
		To:                 []string{"recipient@example.com"},
		Subject:            "Test",
		Body:               "Hello",
		GeneratedMessageID: "<preset@example.com>",
	}

	data, err := msg.Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if msg.GeneratedMessageID != "<preset@example.com>" {
		t.Errorf("preset ID was overwritten: %q", msg.GeneratedMessageID)
	}

	content := string(data)
	if !strings.Contains(content, "Message-ID: <preset@example.com>") {
		t.Error("preset Message-ID not in headers")
	}
}

func TestNormalizeParagraphs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bare p", "<p>text</p>", `<p style="margin:0">text</p>`},
		{"p with style", `<p style="color:red">text</p>`, `<p style="margin:0; color:red">text</p>`},
		{"p with class and style (WebKit editor)", `<p class="isSelectedEnd" style="caret-color: rgb(0, 0, 0);">text</p>`,
			`<p class="isSelectedEnd" style="margin:0; caret-color: rgb(0, 0, 0);">text</p>`},
		{"p with class only", `<p class="MsoNormal">text</p>`, `<p style="margin:0" class="MsoNormal">text</p>`},
		{"p with existing margin", `<p style="margin:10px">text</p>`, `<p style="margin:10px">text</p>`},
		{"uppercase P", `<P>text</P>`, `<P style="margin:0">text</P>`},
		{"multiple p tags", `<p>one</p><p class="x">two</p><p style="color:blue">three</p>`,
			`<p style="margin:0">one</p><p style="margin:0" class="x">two</p><p style="margin:0; color:blue">three</p>`},
		{"no p tags", "<div>text</div>", "<div>text</div>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeParagraphs(tt.input)
			if got != tt.want {
				t.Errorf("normalizeParagraphs(%q)\ngot:  %q\nwant: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeListStyles(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bare ul", "<ul><li>a</li></ul>", `<ul style="padding-left:1.5em;margin:0.3em 0"><li>a</li></ul>`},
		{"bare ol", "<ol><li>a</li></ol>", `<ol style="padding-left:1.5em;margin:0.3em 0"><li>a</li></ol>`},
		{"ul with style", `<ul style="color:red"><li>a</li></ul>`, `<ul style="padding-left:1.5em;margin:0.3em 0; color:red"><li>a</li></ul>`},
		{"ul with existing margin", `<ul style="margin:10px"><li>a</li></ul>`, `<ul style="margin:10px"><li>a</li></ul>`},
		{"ul with existing padding-left", `<ul style="padding-left:2em"><li>a</li></ul>`, `<ul style="padding-left:2em"><li>a</li></ul>`},
		{"ul with class", `<ul class="list"><li>a</li></ul>`, `<ul style="padding-left:1.5em;margin:0.3em 0" class="list"><li>a</li></ul>`},
		{"uppercase UL", `<UL><li>a</li></UL>`, `<UL style="padding-left:1.5em;margin:0.3em 0"><li>a</li></UL>`},
		{"nested lists", `<ul><li>a<ol><li>b</li></ol></li></ul>`,
			`<ul style="padding-left:1.5em;margin:0.3em 0"><li>a<ol style="padding-left:1.5em;margin:0.3em 0"><li>b</li></ol></li></ul>`},
		{"no lists", "<p>text</p>", "<p>text</p>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeListStyles(tt.input)
			if got != tt.want {
				t.Errorf("normalizeListStyles(%q)\ngot:  %q\nwant: %q", tt.input, got, tt.want)
			}
		})
	}
}

func extractHeader(content, name string) string {
	for _, line := range strings.Split(content, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	return ""
}

// TestMailFromPlain_NoExtensionParams pins the contract that mailFromPlain
// sends a bare 'MAIL FROM:<email>' even when the server advertises 8BITMIME
// and SMTPUTF8. If someone replaces mailFromPlain with smtp.Client.Mail()
// in the future, this test will catch it — and the failure message will
// remind them why we bypass: M365 backends reject MAIL FROM with the
// auto-appended parameters returning '502 5.3.3 Command not implemented'.
func TestMailFromPlain_NoExtensionParams(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	var capturedMailFrom string
	serverDone := make(chan struct{})

	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))

		text := textproto.NewConn(conn)
		_ = text.PrintfLine("220 fake.example.com ESMTP test")
		for {
			line, err := text.ReadLine()
			if err != nil {
				return
			}
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO"):
				// Advertise both extensions so stdlib Mail() WOULD append
				// BODY=8BITMIME and SMTPUTF8. mailFromPlain must NOT.
				_ = text.PrintfLine("250-fake.example.com")
				_ = text.PrintfLine("250-8BITMIME")
				_ = text.PrintfLine("250 SMTPUTF8")
			case strings.HasPrefix(upper, "MAIL FROM:"):
				capturedMailFrom = line
				_ = text.PrintfLine("250 2.1.0 OK")
			case strings.HasPrefix(upper, "QUIT"):
				_ = text.PrintfLine("221 2.0.0 bye")
				return
			default:
				_ = text.PrintfLine("502 5.5.1 unimplemented in test")
			}
		}
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client, err := smtp.NewClient(conn, "fake.example.com")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Hello("client.example.com"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}

	if err := mailFromPlain(client, "sender@example.com"); err != nil {
		t.Fatalf("mailFromPlain: %v", err)
	}
	_ = client.Quit()
	<-serverDone

	wantPrefix := "MAIL FROM:<sender@example.com>"
	if capturedMailFrom != wantPrefix {
		t.Fatalf("MAIL FROM line mismatch — got %q, want exactly %q "+
			"(this guards against re-introducing BODY=8BITMIME / SMTPUTF8 "+
			"auto-append which Microsoft 365 rejects with 502 5.3.3)",
			capturedMailFrom, wantPrefix)
	}
}
