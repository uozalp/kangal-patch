// Package slack announces Talos releases in Slack and records the decisions of approvers.
package slack

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var userIDPattern = regexp.MustCompile(`^[UW][A-Z0-9]+$`)

// Config is the operator-wide Slack setup.
type Config struct {
	BotToken  string // xoxb-
	AppToken  string // xapp-, scope connections:write
	ChannelID string
	Approvers []string // Slack user IDs
	Required  int      // approvals needed
}

// Set reports whether any setting other than Required (which has a default) is present; otherwise
// Slack is simply off.
func (c Config) Set() bool {
	return c.BotToken != "" || c.AppToken != "" || c.ChannelID != "" || len(c.Approvers) > 0
}

// ParseApprovers splits a comma-separated list of user IDs, dropping blanks and duplicates.
func ParseApprovers(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range strings.Split(s, ",") {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Validate checks that Slack is fully and consistently configured. Errors never include token values.
func (c Config) Validate() error {
	var errs []error
	if c.BotToken == "" {
		errs = append(errs, errors.New("bot token is not set"))
	}
	if c.AppToken == "" {
		errs = append(errs, errors.New("app token is not set"))
	}
	if c.ChannelID == "" {
		errs = append(errs, errors.New("channel ID is not set"))
	}
	for _, id := range c.Approvers {
		if !userIDPattern.MatchString(id) {
			errs = append(errs, fmt.Errorf("approver %q is not a Slack user ID", id))
		}
	}
	if len(c.Approvers) == 0 {
		errs = append(errs, errors.New("no approvers are set"))
	}
	if c.Required < 1 || c.Required > len(c.Approvers) {
		errs = append(errs, fmt.Errorf("required approvals must be between 1 and the number of approvers (%d), got %d", len(c.Approvers), c.Required))
	}
	return errors.Join(errs...)
}
