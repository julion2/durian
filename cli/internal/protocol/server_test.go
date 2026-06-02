package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/mail"
)

// mockHandler implements CommandHandler for testing
type mockHandler struct {
	response    Response
	lastCommand Command
	callCount   int
}

func (m *mockHandler) Handle(cmd Command) Response {
	m.lastCommand = cmd
	m.callCount++
	return m.response
}

func TestServerValidCommand(t *testing.T) {
	handler := &mockHandler{
		response: SuccessWithResults([]mail.Mail{
			{ThreadID: "123", Subject: "Test"},
		}),
	}

	input := `{"cmd":"search","query":"*","limit":10}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	server := NewServer(handler, reader, &output)
	server.Run()

	// Verify handler was called
	if handler.callCount != 1 {
		t.Errorf("Handler should be called once, got %d", handler.callCount)
	}

	// Verify command was parsed correctly
	if handler.lastCommand.Cmd != "search" {
		t.Errorf("Command.Cmd = %q, want %q", handler.lastCommand.Cmd, "search")
	}
	if handler.lastCommand.Query != "*" {
		t.Errorf("Command.Query = %q, want %q", handler.lastCommand.Query, "*")
	}
	if handler.lastCommand.Limit != 10 {
		t.Errorf("Command.Limit = %d, want %d", handler.lastCommand.Limit, 10)
	}

	// Verify response was written
	var resp Response
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if !resp.OK {
		t.Error("Response should be OK")
	}
	if len(resp.Results) != 1 {
		t.Errorf("Response should have 1 result, got %d", len(resp.Results))
	}
}

func TestServerInvalidJSON(t *testing.T) {
	handler := &mockHandler{response: Success()}

	input := `{invalid json}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	server := NewServer(handler, reader, &output)
	server.Run()

	// Handler should not be called for invalid JSON
	if handler.callCount != 0 {
		t.Errorf("Handler should not be called for invalid JSON, got %d calls", handler.callCount)
	}

	// Verify error response
	var resp Response
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if resp.OK {
		t.Error("Response should not be OK for invalid JSON")
	}
	if resp.ErrorCode != ErrInvalidJSON {
		t.Errorf("ErrorCode = %q, want %q", resp.ErrorCode, ErrInvalidJSON)
	}
	if !strings.Contains(resp.Error, "invalid json") {
		t.Errorf("Error should contain 'invalid json', got %q", resp.Error)
	}
}

func TestServerMultipleCommands(t *testing.T) {
	handler := &mockHandler{response: Success()}

	input := `{"cmd":"search","query":"*"}` + "\n" +
		`{"cmd":"tag","query":"thread:123","tags":"+read"}` + "\n" +
		`{"cmd":"show","thread":"456"}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	server := NewServer(handler, reader, &output)
	server.Run()

	// Handler should be called 3 times
	if handler.callCount != 3 {
		t.Errorf("Handler should be called 3 times, got %d", handler.callCount)
	}

	// Verify 3 responses were written
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Errorf("Should have 3 response lines, got %d", len(lines))
	}
}

func TestServerEmptyInput(t *testing.T) {
	handler := &mockHandler{response: Success()}

	reader := strings.NewReader("")
	var output bytes.Buffer

	server := NewServer(handler, reader, &output)
	server.Run()

	// Handler should not be called
	if handler.callCount != 0 {
		t.Errorf("Handler should not be called for empty input, got %d calls", handler.callCount)
	}

	// No output expected
	if output.Len() != 0 {
		t.Errorf("Output should be empty, got %q", output.String())
	}
}

func TestServerShowCommand(t *testing.T) {
	handler := &mockHandler{
		response: SuccessWithMail(&mail.MailContent{
			From:    "test@example.com",
			Subject: "Test Subject",
			Body:    "Test body",
		}),
	}

	input := `{"cmd":"show","thread":"abc123"}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	server := NewServer(handler, reader, &output)
	server.Run()

	// Verify command parsing
	if handler.lastCommand.Cmd != "show" {
		t.Errorf("Command.Cmd = %q, want %q", handler.lastCommand.Cmd, "show")
	}
	if handler.lastCommand.Thread != "abc123" {
		t.Errorf("Command.Thread = %q, want %q", handler.lastCommand.Thread, "abc123")
	}
}

func TestServerTagCommand(t *testing.T) {
	handler := &mockHandler{response: Success()}

	input := `{"cmd":"tag","query":"thread:123","tags":"+read -unread"}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	server := NewServer(handler, reader, &output)
	server.Run()

	// Verify command parsing
	if handler.lastCommand.Cmd != "tag" {
		t.Errorf("Command.Cmd = %q, want %q", handler.lastCommand.Cmd, "tag")
	}
	if handler.lastCommand.Query != "thread:123" {
		t.Errorf("Command.Query = %q, want %q", handler.lastCommand.Query, "thread:123")
	}
	if handler.lastCommand.Tags != "+read -unread" {
		t.Errorf("Command.Tags = %q, want %q", handler.lastCommand.Tags, "+read -unread")
	}
}

func TestServerMixedValidAndInvalid(t *testing.T) {
	handler := &mockHandler{response: Success()}

	input := `{"cmd":"search","query":"*"}` + "\n" +
		`{invalid}` + "\n" +
		`{"cmd":"tag","query":"*","tags":"+test"}` + "\n"
	reader := strings.NewReader(input)
	var output bytes.Buffer

	server := NewServer(handler, reader, &output)
	server.Run()

	// Handler should be called twice (valid commands only)
	if handler.callCount != 2 {
		t.Errorf("Handler should be called 2 times, got %d", handler.callCount)
	}

	// Verify 3 responses (2 success, 1 error)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Errorf("Should have 3 response lines, got %d", len(lines))
	}

	// Second line should be error
	var errResp Response
	if err := json.Unmarshal([]byte(lines[1]), &errResp); err != nil {
		t.Fatalf("Failed to parse error response: %v", err)
	}
	if errResp.OK {
		t.Error("Second response should be error")
	}
	if errResp.ErrorCode != ErrInvalidJSON {
		t.Errorf("Second response ErrorCode = %q, want %q", errResp.ErrorCode, ErrInvalidJSON)
	}
}
