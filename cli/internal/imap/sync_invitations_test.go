package imap

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/julion2/durian/cli/internal/config"
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

// mimePart describes a message as a tree. The test renders it as raw MIME for
// the parser and as the BODYSTRUCTURE and sections an IMAP server would serve
// for it, so both ways of finding the calendar part see the same message.
type mimePart struct {
	typ, sub   string
	params     map[string]string
	disp       string
	dispParams map[string]string
	enc        string // as the server reports it, e.g. BASE64
	body       string // decoded
	parts      []*mimePart
}

func (p *mimePart) multipart() bool { return p.typ == "multipart" }

func (p *mimePart) encoded() string {
	switch strings.ToLower(p.enc) {
	case "base64":
		s := base64.StdEncoding.EncodeToString([]byte(p.body))
		var lines []string
		for len(s) > 76 {
			lines, s = append(lines, s[:76]), s[76:]
		}
		return strings.Join(append(lines, s), "\r\n")
	case "quoted-printable":
		var b bytes.Buffer
		w := quotedprintable.NewWriter(&b)
		_, _ = w.Write([]byte(p.body))
		_ = w.Close()
		return b.String()
	default:
		return p.body
	}
}

func headerParams(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "; %s=%q", k, m[k])
	}
	return b.String()
}

// render writes the part's headers, a blank line and its body.
func (p *mimePart) render(boundaries *int) string {
	params := map[string]string{}
	for k, v := range p.params {
		params[k] = v
	}
	boundary := ""
	if p.multipart() {
		*boundaries++
		boundary = fmt.Sprintf("b%d", *boundaries)
		params["boundary"] = boundary
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Content-Type: %s/%s%s\r\n", p.typ, p.sub, headerParams(params))
	if p.disp != "" {
		fmt.Fprintf(&b, "Content-Disposition: %s%s\r\n", p.disp, headerParams(p.dispParams))
	}
	if p.enc != "" {
		fmt.Fprintf(&b, "Content-Transfer-Encoding: %s\r\n", p.enc)
	}
	b.WriteString("\r\n")
	if !p.multipart() {
		b.WriteString(p.encoded())
		return b.String()
	}
	for _, child := range p.parts {
		fmt.Fprintf(&b, "--%s\r\n%s\r\n", boundary, child.render(boundaries))
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.String()
}

func (p *mimePart) message() string {
	n := 0
	return "From: a@example.com\r\nTo: b@example.com\r\nSubject: Planning\r\nMIME-Version: 1.0\r\n" + p.render(&n)
}

// structure is the BODYSTRUCTURE a server reports, in its upper case.
func (p *mimePart) structure() *goimap.BodyStructure {
	bs := &goimap.BodyStructure{
		MIMEType:          strings.ToUpper(p.typ),
		MIMESubType:       strings.ToUpper(p.sub),
		Params:            p.params,
		Disposition:       p.disp,
		DispositionParams: p.dispParams,
		Encoding:          p.enc,
	}
	for _, child := range p.parts {
		bs.Parts = append(bs.Parts, child.structure())
	}
	return bs
}

// section returns the transfer-encoded body of an IMAP section.
func (p *mimePart) section(path []int) ([]byte, error) {
	part := p
	if !p.multipart() {
		if len(path) != 1 || path[0] != 1 {
			return nil, fmt.Errorf("no section %v", path)
		}
		return []byte(p.encoded()), nil
	}
	for _, i := range path {
		if i < 1 || i > len(part.parts) {
			return nil, fmt.Errorf("no section %v", path)
		}
		part = part.parts[i-1]
	}
	return []byte(part.encoded()), nil
}

func leaf(typ, sub, body string) *mimePart {
	return &mimePart{typ: typ, sub: sub, body: body}
}

func multi(sub string, parts ...*mimePart) *mimePart {
	return &mimePart{typ: "multipart", sub: sub, parts: parts}
}

func ics(summary string) string {
	return "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:" + summary + "@example.com\r\nSUMMARY:" + summary + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func calendar(body, enc string) *mimePart {
	return &mimePart{typ: "text", sub: "calendar", params: map[string]string{"method": "REQUEST", "charset": "utf-8"}, enc: enc, body: body}
}

func attachedICS(typ, sub, body, enc string) *mimePart {
	return &mimePart{typ: typ, sub: sub, params: map[string]string{"name": "invite.ics"}, disp: "attachment",
		dispParams: map[string]string{"filename": "invite.ics"}, enc: enc, body: body}
}

func invitationCases() []struct {
	name string
	msg  *mimePart
	want string
} {
	oversized := strings.Repeat("X", durianmail.MaxCalendarBytes+1)
	latin9 := calendar("", "QUOTED-PRINTABLE")
	latin9.params = map[string]string{"method": "REQUEST", "charset": "iso-8859-15"}
	latin9.body = "BEGIN:VCALENDAR\r\nSUMMARY:5 \xa4\r\nEND:VCALENDAR\r\n"
	inlineICS := attachedICS("application", "ics", ics("inline-named"), "BASE64")
	inlineICS.disp = "inline"
	forwarded := leaf("message", "rfc822", "Content-Type: text/calendar\r\n\r\n"+ics("forwarded"))
	attachedMultipart := multi("mixed", calendar(ics("inside-attachment"), "BASE64"))
	attachedMultipart.disp = "attachment"
	upperAttachment := attachedICS("text", "calendar", ics("attached-first"), "BASE64")
	upperAttachment.disp = "ATTACHMENT"
	return []struct {
		name string
		msg  *mimePart
		want string
	}{
		{"outlook: alternative with text/calendar",
			multi("alternative", leaf("text", "plain", "Planning"), leaf("text", "html", "<p>Planning</p>"), calendar(ics("outlook"), "BASE64")),
			ics("outlook")},
		{"gmail: inline part wins over the attached copy",
			multi("mixed",
				multi("alternative", leaf("text", "plain", "Planning"), calendar(ics("gmail-inline"), "7BIT")),
				attachedICS("application", "ics", ics("gmail-attached"), "BASE64")),
			ics("gmail-inline")},
		{"attached text/calendar only",
			multi("mixed", leaf("text", "plain", "See attached"), attachedICS("text", "calendar", ics("attached"), "QUOTED-PRINTABLE")),
			ics("attached")},
		{"inline application/ics with a filename counts as attached",
			multi("mixed", leaf("text", "plain", "x"), inlineICS, multi("alternative", leaf("text", "plain", "x"), calendar(ics("real-inline"), "BASE64"))),
			ics("real-inline")},
		{"attached parts in document order",
			multi("mixed", leaf("text", "plain", "x"), inlineICS, attachedICS("text", "calendar", ics("second"), "BASE64")),
			ics("inline-named")},
		{"nested alternative inside related inside mixed",
			multi("mixed", multi("related", multi("alternative", leaf("text", "html", "<p>x</p>"), calendar(ics("nested"), "BASE64"))), attachedICS("application", "pdf", "%PDF", "BASE64")),
			ics("nested")},
		{"single-part text/calendar", calendar(ics("single"), "QUOTED-PRINTABLE"), ics("single")},
		{"single-part attached .ics", attachedICS("application", "ics", ics("single-attached"), "BASE64"), ics("single-attached")},
		{"oversized inline part falls back to the attached one",
			multi("mixed", multi("alternative", leaf("text", "plain", "x"), calendar(oversized, "BASE64")), attachedICS("application", "ics", ics("fallback"), "BASE64")),
			ics("fallback")},
		{"charset is decoded", latin9, "BEGIN:VCALENDAR\r\nSUMMARY:5 €\r\nEND:VCALENDAR\r\n"},
		{"mentioning text/calendar is no invitation", multi("alternative", leaf("text", "plain", "Send it as text/calendar please")), ""},
		{"a forwarded message's invitation isn't this one's", multi("mixed", leaf("text", "plain", "fwd"), forwarded), ""},
		{"an attached multipart is not this message's invitation", multi("mixed", attachedMultipart), ""},
		{"disposition is case insensitive", multi("mixed", upperAttachment, calendar(ics("actual-inline"), "BASE64")), ics("actual-inline")},
	}
}

// The IMAP path picks the calendar part from the BODYSTRUCTURE and fetches it
// alone; it must keep what the parser keeps from the whole message.
func TestFetchCalendarPart_KeepsWhatTheParserKeeps(t *testing.T) {
	for _, c := range invitationCases() {
		t.Run(c.name, func(t *testing.T) {
			parsed, err := mail.ReadMessage(strings.NewReader(c.msg.message()))
			if err != nil {
				t.Fatal(err)
			}
			fromParser := durianmail.NewParser().Parse(parsed).Calendar
			fromIMAP, err := fetchCalendarPart(c.msg.structure(), c.msg.section)
			if err != nil {
				t.Fatal(err)
			}
			if fromParser != c.want {
				t.Errorf("parser kept %.80q, want %.80q", fromParser, c.want)
			}
			if fromIMAP != c.want {
				t.Errorf("IMAP section fetch kept %.80q, want %.80q", fromIMAP, c.want)
			}
		})
	}
}

func TestFetchCalendarPart_FetchError(t *testing.T) {
	msg := multi("alternative", leaf("text", "plain", "x"), calendar(ics("x"), "BASE64"))
	failing := func([]int) ([]byte, error) { return nil, errors.New("connection reset") }
	if _, err := fetchCalendarPart(msg.structure(), failing); err == nil {
		t.Fatal("a failed section fetch must be reported, not stored as no invitation")
	}
}

func TestFillInvitations_StalledServerAndSkippedModes(t *testing.T) {
	for _, mode := range []string{"deadline", "upload-only", "dry-run", "backlog"} {
		t.Run(mode, func(t *testing.T) {
			db := newFlagTestDB(t)
			count := 1
			if mode == "backlog" {
				count = 50
			}
			for i := 1; i <= count; i++ {
				m := &store.Message{MessageID: fmt.Sprintf("old%d@example.com", i), Account: "work", Mailbox: "INBOX", UID: uint32(i), Date: 1}
				if err := db.InsertMessage(m); err != nil {
					t.Fatal(err)
				}
				if err := db.AddTag(m.ID, store.CalendarTag); err != nil {
					t.Fatal(err)
				}
			}
			local, remote := net.Pipe()
			t.Cleanup(func() { local.Close(); remote.Close() })
			command := make(chan string, 1)
			go func() {
				fmt.Fprint(remote, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
				reader := bufio.NewReader(remote)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if mode != "backlog" {
						command <- line // deliberately never answer SELECT
						return
					}
					// Responsive commands, but more work than fits in one run.
					time.Sleep(15 * time.Millisecond)
					fields := strings.Fields(line)
					if fields[1] == "SELECT" {
						fmt.Fprint(remote, "* 50 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n")
					} else if fields[1] == "UID" && fields[2] == "FETCH" {
						fmt.Fprintf(remote, "* 1 FETCH (UID %s BODYSTRUCTURE (\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 1 1))\r\n", fields[3])
					}
					fmt.Fprintf(remote, "%s OK completed\r\n", fields[0])
				}
			}()
			conn, err := client.New(local)
			if err != nil {
				t.Fatal(err)
			}
			s := NewSyncer(&config.AccountConfig{Name: "work"}, &SyncOptions{Store: db, Quiet: true})
			s.client.conn = conn
			s.options.DryRun = mode == "dry-run"
			if mode == "upload-only" {
				s.options.Mode = SyncUploadOnly
			}
			done := make(chan struct{})
			go func() { s.fillInvitations(100 * time.Millisecond); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				local.Close()
				<-done
				t.Fatal("catch-up remained blocked beyond its budget")
			}
			switch mode {
			case "deadline":
				if line := <-command; !strings.Contains(line, "SELECT") {
					t.Fatalf("expected a stalled SELECT, got %q", line)
				}
			case "backlog":
				if err := conn.Noop(); err != nil {
					t.Fatalf("budget expiry killed the responsive caller-owned connection: %v", err)
				}
			default:
				select {
				case line := <-command:
					t.Fatalf("skipped catch-up sent %q", line)
				default:
				}
			}
			missing, err := db.MissingInvitations("work", store.ByUID, nil, count)
			if err != nil || len(missing) == 0 || (mode == "backlog" && len(missing) >= count) {
				t.Fatalf("message must remain retryable: %v, %v", missing, err)
			}
		})
	}
}

// Ingest keeps what the catch-up would fetch, for each case, and records
// mail tagged as calendar mail without one as looked at.
func TestStoreInsertMessage_KeepsTheInvitation(t *testing.T) {
	db := newFlagTestDB(t)
	syncer := NewSyncer(&config.AccountConfig{Name: "work"}, &SyncOptions{Store: db, Quiet: true})
	date := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)
	for i, c := range invitationCases() {
		t.Run(c.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf("Message-ID: <case%d@example.com>\r\n", i) + c.msg.message())
			id, err := syncer.storeInsertMessage("INBOX", 1, nil, &goimap.Message{Uid: uint32(i + 1), InternalDate: date}, raw)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := db.GetByMessageID(id)
			if err != nil || stored == nil {
				t.Fatalf("stored message: %v", err)
			}
			invs, err := db.InvitationsByMessages([]int64{stored.ID})
			if err != nil {
				t.Fatal(err)
			}
			if invs[stored.ID] != c.want {
				t.Errorf("kept %.60q, want %.60q", invs[stored.ID], c.want)
			}
			tags, _ := db.GetMessageTags(stored.ID)
			tagged := slices.Contains(tags, store.CalendarTag)
			if wantTag := c.want != "" || bytes.Contains(raw, []byte("text/calendar")); tagged != wantTag {
				t.Errorf("tagged %q: %v, want %v", store.CalendarTag, tagged, wantTag)
			}
			missing, err := db.MissingInvitations("work", store.ByUID, nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range missing {
				if m.ID == stored.ID {
					t.Error("left for the catch-up to fetch, want it looked at on ingest")
				}
			}
		})
	}
}
