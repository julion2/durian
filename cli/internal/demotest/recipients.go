package demotest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/mail"
	"strings"
)

const maxMetadata = 1 << 20
const maxPayload = 12 << 20 // small attachments only; all reads are bounded

func bareAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && a.Name == "" && !strings.ContainsAny(s, "\r\n")
}

func (g *mailGuard) address(value string) bool {
	if strings.ContainsAny(value, "\r\n") {
		return false
	}
	a, err := mail.ParseAddress(value)
	return err == nil && g.allowed[a.Address]
}

func (g *mailGuard) envelope(groups ...[]string) bool {
	count := 0
	for _, group := range groups {
		for _, value := range group {
			if !g.address(value) {
				return false
			}
			count++
		}
	}
	return count > 0
}

func (g *mailGuard) mime(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maxPayload {
		return false
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	count := 0
	for name, values := range m.Header {
		lower := strings.ToLower(name)
		// Resent recipients have provider-specific envelope semantics. Fail
		// closed instead of trying to predict which block the provider uses.
		if strings.HasPrefix(lower, "resent-") {
			return false
		}
		if lower != "to" && lower != "cc" && lower != "bcc" {
			continue
		}
		for _, value := range values {
			addrs, err := mail.ParseAddressList(value)
			if err != nil || len(addrs) == 0 {
				return false
			}
			for _, a := range addrs {
				if !g.allowed[a.Address] {
					return false
				}
				count++
			}
		}
	}
	return count > 0
}

// object rejects duplicate keys (including case variants), trailing JSON and
// nonobjects. Only documented mutation shapes are subsequently whitelisted.
func object(data []byte) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, false
	}
	out := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, false
		}
		k, ok := key.(string)
		if !ok || seen[strings.ToLower(k)] {
			return nil, false
		}
		seen[strings.ToLower(k)] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		out[k] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, false
	}
	return out, true
}

func keys(obj map[string]json.RawMessage, allowed ...string) bool {
	for k := range obj {
		found := false
		for _, a := range allowed {
			found = found || k == a
		}
		if !found {
			return false
		}
	}
	return true
}

func textValue(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func (g *mailGuard) gmailRaw(data []byte, draft bool) bool {
	o, ok := object(data)
	if draft {
		if !ok || !keys(o, "message") {
			return false
		}
		o, ok = object(o["message"])
	}
	if !ok || !keys(o, "raw", "threadId") {
		return false
	}
	encoded := textValue(o["raw"])
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(encoded)
	}
	return err == nil && g.mime(raw)
}

func (g *mailGuard) graphRecipients(o map[string]json.RawMessage) bool {
	count := 0
	for _, key := range []string{"toRecipients", "ccRecipients", "bccRecipients"} {
		if len(o[key]) == 0 {
			continue
		}
		var recipients []json.RawMessage
		if json.Unmarshal(o[key], &recipients) != nil {
			return false
		}
		for _, raw := range recipients {
			r, ok := object(raw)
			if !ok || !keys(r, "emailAddress") {
				return false
			}
			a, ok := object(r["emailAddress"])
			if !ok || !keys(a, "address", "name") || !g.allowed[textValue(a["address"])] {
				return false
			}
			count++
		}
	}
	return count > 0
}

func fileAttachment(raw []byte) bool {
	a, ok := object(raw)
	if !ok || !keys(a, "@odata.type", "name", "contentType", "contentBytes") ||
		textValue(a["@odata.type"]) != "#microsoft.graph.fileAttachment" {
		return false
	}
	data, err := base64.StdEncoding.DecodeString(textValue(a["contentBytes"]))
	return err == nil && len(data) < 3<<20
}

func (g *mailGuard) graphDraft(data []byte) bool {
	o, ok := object(data)
	if !ok || !keys(o, "subject", "body", "toRecipients", "ccRecipients", "bccRecipients", "attachments") || !g.graphRecipients(o) {
		return false
	}
	if len(o["body"]) != 0 {
		b, ok := object(o["body"])
		if !ok || !keys(b, "contentType", "content") {
			return false
		}
	}
	if len(o["attachments"]) != 0 {
		var attachments []json.RawMessage
		if json.Unmarshal(o["attachments"], &attachments) != nil {
			return false
		}
		for _, a := range attachments {
			if !fileAttachment(a) {
				return false
			}
		}
	}
	return true
}
