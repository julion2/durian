package imap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/store"
)

// ExecInput is the JSON sent to an exec rule's stdin.
type ExecInput struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	CC            string   `json:"cc,omitempty"`
	Subject       string   `json:"subject"`
	Body          string   `json:"body"`
	HTML          string   `json:"html,omitempty"`
	Account       string   `json:"account"`
	Tags          []string `json:"tags"`
	HasAttachment bool     `json:"has_attachment"`
}

// ExecOutput is the JSON expected from an exec rule's stdout.
type ExecOutput struct {
	AddTags    []string `json:"add_tags"`
	RemoveTags []string `json:"remove_tags"`
}

const defaultExecTimeout = 10 // seconds

// filterAllowedTags returns only tags that are in the allowed list.
// Rejected tags are logged as warnings.
func filterAllowedTags(tags, allowed []string, ruleName string) []string {
	if len(tags) == 0 {
		return tags
	}
	set := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		set[t] = true
	}
	var filtered []string
	for _, t := range tags {
		if set[t] {
			filtered = append(filtered, t)
		} else {
			slog.Warn("Exec rule returned disallowed tag", "module", "RULES", "rule", ruleName, "tag", t)
		}
	}
	return filtered
}

// RunExecRule runs an external command for a matched rule and returns tag operations.
// Returns nil, nil if the rule has no exec command.
func RunExecRule(rule config.RuleConfig, msg *store.Message, tags []string, account string) (*ExecOutput, error) {
	if rule.Exec == "" {
		return nil, nil
	}

	timeout := rule.ExecTimeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}

	input := ExecInput{
		From:          msg.FromAddr,
		To:            msg.ToAddrs,
		CC:            msg.CCAddrs,
		Subject:       msg.Subject,
		Body:          msg.BodyText,
		HTML:          msg.BodyHTML,
		Account:       account,
		Tags:          tags,
		HasAttachment: false, // caller can set this
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal exec input: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, rule.Exec)
	cmd.Stdin = bytes.NewReader(inputJSON)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Debug("Running exec rule", "module", "RULES", "rule", rule.Name, "cmd", rule.Exec, "timeout", timeout)

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("exec rule %q timed out after %ds", rule.Name, timeout)
		}
		stderrStr := stderr.String()
		if stderrStr != "" {
			slog.Warn("Exec rule stderr", "module", "RULES", "rule", rule.Name, "stderr", stderrStr)
		}
		return nil, fmt.Errorf("exec rule %q failed: %w", rule.Name, err)
	}

	if stderr.Len() > 0 {
		slog.Debug("Exec rule stderr", "module", "RULES", "rule", rule.Name, "stderr", stderr.String())
	}

	// Empty stdout = no action
	if stdout.Len() == 0 || bytes.TrimSpace(stdout.Bytes()) == nil {
		return &ExecOutput{}, nil
	}

	var output ExecOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return nil, fmt.Errorf("exec rule %q: invalid JSON output: %w", rule.Name, err)
	}

	slog.Debug("Exec rule result", "module", "RULES", "rule", rule.Name, "add", output.AddTags, "remove", output.RemoveTags)
	return &output, nil
}
