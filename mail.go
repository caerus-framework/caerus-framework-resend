package cf_resend

import (
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"

	"github.com/resend/resend-go/v2"
)

// Mail is the Caerus send DTO (SES-simple shape). Apps pass this to Send
// without importing resend-go. Attachments, Cc/Bcc, headers, and scheduled
// send stay on Client() for callers that opt into the SDK.
type Mail struct {
	// From overrides from_address when non-empty. Empty (or whitespace)
	// uses the configured soft default. The resolved value must parse as
	// an RFC 5322 address (`net/mail.ParseAddress`).
	From    string
	To      []string
	Subject string
	HTML    string
	Text    string
	ReplyTo string
	// Tags are Resend metadata (name → value). Empty names are skipped.
	Tags map[string]string
	// IdempotencyKey is sent as the Idempotency-Key header when non-empty.
	IdempotencyKey string
}

func resolveFrom(mailFrom, defaultFrom string) (string, error) {
	from := strings.TrimSpace(mailFrom)
	if from == "" {
		from = strings.TrimSpace(defaultFrom)
	}
	if from == "" {
		return "", errors.New("cf_resend: no From (set from_address / WithFromAddress, or Mail.From)")
	}
	if err := validateMailbox("From", from); err != nil {
		return "", err
	}
	return from, nil
}

func validateTo(to []string) ([]string, error) {
	if len(to) == 0 {
		return nil, errors.New("cf_resend: Mail.To is empty")
	}
	out := make([]string, 0, len(to))
	for i, raw := range to {
		addr := strings.TrimSpace(raw)
		if addr == "" {
			return nil, fmt.Errorf("cf_resend: Mail.To[%d] is empty", i)
		}
		if err := validateMailbox("To", addr); err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, nil
}

func validateMailbox(field, addr string) error {
	if _, err := mail.ParseAddress(addr); err != nil {
		return fmt.Errorf("cf_resend: invalid %s %q: %w", field, addr, err)
	}
	return nil
}

func (m Mail) toSDK(from string) *resend.SendEmailRequest {
	req := &resend.SendEmailRequest{
		From:    from,
		To:      append([]string(nil), m.To...),
		Subject: m.Subject,
		Html:    m.HTML,
		Text:    m.Text,
		ReplyTo: m.ReplyTo,
	}
	if len(m.Tags) == 0 {
		return req
	}
	names := make([]string, 0, len(m.Tags))
	for name := range m.Tags {
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	req.Tags = make([]resend.Tag, 0, len(names))
	for _, name := range names {
		req.Tags = append(req.Tags, resend.Tag{Name: name, Value: m.Tags[name]})
	}
	return req
}
