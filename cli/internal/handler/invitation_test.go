package handler

import (
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/store"
)

const answeredInvite = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
	"UID:planning@example.com\r\nDTSTART:20261020T080000Z\r\nDTEND:20261020T090000Z\r\nSUMMARY:Quarterly planning\r\n" +
	"ORGANIZER:mailto:mara@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:me@work.example\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

func TestShowThread_Invitation(t *testing.T) {
	db := newTestStore(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Unix()
	invite := seedThreadMessage(t, db, &store.Message{
		MessageID: "invite@test", Subject: "Invitation: Quarterly planning", FromAddr: "mara@example.com",
		Date: at, CreatedAt: at, BodyText: "Planning", Mailbox: "INBOX", Account: "work",
	})
	broken := seedThreadMessage(t, db, &store.Message{
		MessageID: "broken@test", Subject: "Re: Invitation", FromAddr: "mara@example.com", InReplyTo: "<invite@test>", Refs: "<invite@test>",
		Date: at + 60, CreatedAt: at, BodyText: "Updated", Mailbox: "INBOX", Account: "work",
	})
	if err := db.SetInvitation(invite.ID, answeredInvite); err != nil {
		t.Fatal(err)
	}
	if err := db.SetInvitation(broken.ID, "BEGIN:VCALENDAR\r\nnot really"); err != nil {
		t.Fatal(err)
	}
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "work", Email: "me@work.example"}}})

	resp := h.ShowThread(invite.ThreadID)
	if !resp.OK || len(resp.Thread.Messages) != 2 {
		t.Fatalf("ShowThread = %+v", resp)
	}
	byID := map[string]*calendar.Invitation{}
	for _, m := range resp.Thread.Messages {
		byID[m.MessageID] = m.Invitation
	}
	inv := byID["invite@test"]
	if inv == nil {
		t.Fatal("thread view has no invitation for the invitation mail")
	}
	if inv.Method != "REQUEST" || inv.Event.Subject != "Quarterly planning" || inv.Event.Account != "work" {
		t.Errorf("invitation = %+v", inv)
	}
	if inv.Event.MyResponse != string(calendar.OwnerRespAccepted) {
		t.Errorf("my response = %q, want %q: read for the account the mail reached", inv.Event.MyResponse, calendar.OwnerRespAccepted)
	}
	if byID["broken@test"] != nil {
		t.Errorf("unreadable invitation shown as %+v, want none", byID["broken@test"])
	}

	// search results stay small: no invitations there
	msgs, err := db.GetByThread(invite.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	light := h.convertThread(invite.ThreadID, msgs, true, nil, nil)
	for _, m := range light.Messages {
		if m.Invitation != nil {
			t.Errorf("light view of %s carries an invitation", m.MessageID)
		}
	}
}
