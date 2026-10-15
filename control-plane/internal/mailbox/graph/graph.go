// Package graph implements the Microsoft 365 mailbox [mailbox.Source] over
// Microsoft Graph (HOR-358, DES-HOR-358-02).
//
// Every request runs inside the tool gateway with an app-only access token for
// the mailbox's credential slot. The token, the client secret and any
// provider response text never reach logs, errors or the database: failures
// surface as typed, customer-safe errors.
package graph

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
)

// DefaultBaseURL is the Microsoft Graph v1.0 endpoint.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

const (
	pageSize = 50
	// Graph allows at most 4230 minutes for Outlook message subscriptions.
	subscriptionLifetime = 48 * time.Hour
	renewBefore          = 24 * time.Hour
	maxResponseBytes     = 8 << 20
	messageSelect        = "id,internetMessageId,conversationId,subject,from,receivedDateTime,hasAttachments,isDraft"
)

// TokenSource returns a current app-only Graph access token for a credential
// slot. Failures must be *mailbox.CredentialError with a customer-safe reason.
type TokenSource interface {
	Token(ctx context.Context, slot string) (string, error)
}

// Client is the Microsoft Graph mailbox source.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Tokens  TokenSource
}

var _ mailbox.Source = (*Client)(nil)

// New returns a Client with a bounded HTTP timeout that never follows a
// redirect, so a bearer token is only ever sent to the Graph endpoint.
func New(tokens TokenSource) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		BaseURL: DefaultBaseURL,
		Tokens:  tokens,
	}
}

type graphMessage struct {
	ID                string `json:"id"`
	InternetMessageID string `json:"internetMessageId"`
	ConversationID    string `json:"conversationId"`
	Subject           string `json:"subject"`
	From              *struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
	ReceivedDateTime time.Time       `json:"receivedDateTime"`
	HasAttachments   bool            `json:"hasAttachments"`
	IsDraft          bool            `json:"isDraft"`
	Removed          json.RawMessage `json:"@removed"`
}

// Changes runs one Graph delta page sequence step for the folder. An empty
// cursor starts a delta sync filtered to mail received at or after since, so
// pre-existing mail is history and never an event.
func (c *Client) Changes(ctx context.Context, mb mailbox.Mailbox, folder, cursor string, since time.Time) (mailbox.Changes, error) {
	target := cursor
	if target == "" {
		q := url.Values{}
		q.Set("$select", messageSelect)
		q.Set("$filter", "receivedDateTime ge "+since.UTC().Format(time.RFC3339))
		target = c.base() + "/users/" + url.PathEscape(mb.Address) + "/mailFolders/" + url.PathEscape(folder) +
			"/messages/delta?" + q.Encode()
	} else if !c.owned(target) {
		// A cursor is only ever a Graph-issued link; anything else is reset
		// rather than receiving the bearer token.
		return mailbox.Changes{}, mailbox.ErrCursorExpired
	}
	var page struct {
		Value     []graphMessage `json:"value"`
		NextLink  string         `json:"@odata.nextLink"`
		DeltaLink string         `json:"@odata.deltaLink"`
	}
	if err := c.do(ctx, mb, http.MethodGet, target, nil, &page); err != nil {
		return mailbox.Changes{}, err
	}
	out := mailbox.Changes{}
	for _, m := range page.Value {
		if len(m.Removed) > 0 || m.IsDraft || m.ID == "" {
			continue
		}
		msg := mailbox.Message{
			ProviderMessageID: m.ID,
			Folder:            folder,
			InternetMessageID: m.InternetMessageID,
			ConversationID:    m.ConversationID,
			Subject:           m.Subject,
			HasAttachments:    m.HasAttachments,
			ReceivedAt:        m.ReceivedDateTime,
		}
		if m.From != nil {
			msg.SenderAddress = m.From.EmailAddress.Address
			msg.SenderName = m.From.EmailAddress.Name
		}
		out.Messages = append(out.Messages, msg)
	}
	switch {
	case page.NextLink != "":
		out.Cursor, out.More = page.NextLink, true
	case page.DeltaLink != "":
		out.Cursor = page.DeltaLink
	default:
		return mailbox.Changes{}, fmt.Errorf("graph delta for %s: response has no next or delta link", mb.Address)
	}
	return out, nil
}

// ReplyHeaders reads In-Reply-To and References from the message headers.
// Delta cannot select internetMessageHeaders, so it is fetched per new message.
func (c *Client) ReplyHeaders(ctx context.Context, mb mailbox.Mailbox, msg *mailbox.Message) error {
	target := c.base() + "/users/" + url.PathEscape(mb.Address) + "/messages/" + url.PathEscape(msg.ProviderMessageID) +
		"?$select=internetMessageHeaders"
	var body struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"internetMessageHeaders"`
	}
	if err := c.do(ctx, mb, http.MethodGet, target, nil, &body); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil // moved or deleted since delta; identity is enough
		}
		return err
	}
	for _, h := range body.Headers {
		switch strings.ToLower(h.Name) {
		case "in-reply-to":
			msg.InReplyTo = strings.TrimSpace(h.Value)
		case "references":
			msg.References = strings.Fields(h.Value)
		}
	}
	return nil
}

// Subscribe creates the folder's "created" subscription, renews it when it is
// within a day of expiry, and recreates it when Graph no longer knows it.
func (c *Client) Subscribe(ctx context.Context, mb mailbox.Mailbox, folder string, current *mailbox.Subscription, now time.Time) (*mailbox.Subscription, error) {
	if current != nil {
		if current.ExpiresAt.Sub(now) > renewBefore {
			return current, nil
		}
		expires := now.Add(subscriptionLifetime).UTC()
		body := map[string]string{"expirationDateTime": expires.Format(time.RFC3339)}
		err := c.do(ctx, mb, http.MethodPatch, c.base()+"/subscriptions/"+url.PathEscape(current.ProviderID), body, nil)
		if err == nil {
			renewed := *current
			renewed.ExpiresAt = expires
			return &renewed, nil
		}
		if !isStatus(err, http.StatusNotFound) {
			return nil, err
		}
	}
	state := make([]byte, 32)
	if _, err := rand.Read(state); err != nil {
		return nil, fmt.Errorf("subscription client state: %w", err)
	}
	clientState := hex.EncodeToString(state)
	expires := now.Add(subscriptionLifetime).UTC()
	req := map[string]string{
		"changeType":                "created",
		"notificationUrl":           mb.NotificationURL,
		"resource":                  "users/" + mb.Address + "/mailFolders('" + folder + "')/messages",
		"expirationDateTime":        expires.Format(time.RFC3339),
		"clientState":               clientState,
		"latestSupportedTlsVersion": "v1_2",
	}
	var created struct {
		ID                 string    `json:"id"`
		ExpirationDateTime time.Time `json:"expirationDateTime"`
	}
	if err := c.do(ctx, mb, http.MethodPost, c.base()+"/subscriptions", req, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, fmt.Errorf("graph subscription for %s: response has no id", mb.Address)
	}
	if !created.ExpirationDateTime.IsZero() {
		expires = created.ExpirationDateTime
	}
	sum := sha256.Sum256([]byte(clientState))
	return &mailbox.Subscription{
		ProviderID:        created.ID,
		NotificationURL:   mb.NotificationURL,
		ClientStateSHA256: hex.EncodeToString(sum[:]),
		ExpiresAt:         expires,
	}, nil
}

// Unsubscribe deletes a subscription; a missing one is already gone.
func (c *Client) Unsubscribe(ctx context.Context, mb mailbox.Mailbox, sub mailbox.Subscription) error {
	err := c.do(ctx, mb, http.MethodDelete, c.base()+"/subscriptions/"+url.PathEscape(sub.ProviderID), nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// statusError is a non-success Graph response. Only the status and the
// closed-set Graph error code are kept; the message text may echo request
// content and is dropped.
type statusError struct {
	status int
	code   string
	op     string
}

func (e *statusError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("graph %s: HTTP %d (%s)", e.op, e.status, e.code)
	}
	return fmt.Sprintf("graph %s: HTTP %d", e.op, e.status)
}

func isStatus(err error, status int) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == status
}

func (c *Client) do(ctx context.Context, mb mailbox.Mailbox, method, target string, body, out any) error {
	req, err := c.request(ctx, mb, method, target, body)
	if err != nil {
		return err
	}
	op := method + " " + operation(target)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &mailbox.RetryAfterError{After: time.Minute}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &mailbox.RetryAfterError{After: time.Minute}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("graph %s: unparseable response", op)
		}
		return nil
	}
	return classify(resp, errorCode(raw), mb.Address, op)
}

// request builds an authenticated Graph request for the mailbox's slot.
func (c *Client) request(ctx context.Context, mb mailbox.Mailbox, method, target string, body any) (*http.Request, error) {
	token, err := c.Tokens.Token(ctx, mb.CredentialSlot)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("graph request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	// Immutable ids keep a message's identity stable across folder moves, so
	// deduplication by provider id holds.
	req.Header.Set("Prefer", `IdType="ImmutableId", odata.maxpagesize=`+strconv.Itoa(pageSize))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// classify maps a non-success Graph response to a typed mailbox error.
func classify(resp *http.Response, code, address, op string) error {
	status := resp.StatusCode
	switch {
	case status == http.StatusGone || strings.EqualFold(code, "syncStateNotFound") ||
		strings.EqualFold(code, "resyncRequired") || strings.EqualFold(code, "syncStateInvalid"):
		return mailbox.ErrCursorExpired
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout || status == http.StatusBadGateway:
		return &mailbox.RetryAfterError{After: retryAfter(resp.Header.Get("Retry-After"))}
	case status == http.StatusUnauthorized:
		return &mailbox.CredentialError{Reason: "Microsoft 365 rejected the access token for " + address}
	case status == http.StatusForbidden || strings.EqualFold(code, "ErrorAccessDenied"):
		return &mailbox.CredentialError{Reason: "Microsoft 365 denied access to " + address}
	case status == http.StatusNotFound && strings.EqualFold(code, "MailboxNotEnabledForRESTAPI"):
		return &mailbox.CredentialError{Reason: "Microsoft 365 has no mailbox for " + address}
	}
	return &statusError{status: status, code: code, op: op}
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// owned reports whether target is under the configured Graph base URL.
func (c *Client) owned(target string) bool {
	return strings.HasPrefix(target, c.base()+"/")
}

// operation is a log-safe request shape: the path without ids or query.
func operation(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return "request"
	}
	switch {
	case strings.Contains(u.Path, "/delta"):
		return "delta"
	case strings.Contains(u.Path, "/subscriptions"):
		return "subscriptions"
	case strings.Contains(u.Path, "/messages/"):
		return "message"
	}
	return "request"
}

// errorCode extracts the Graph error code. Codes are identifier-shaped; any
// other value is dropped so no provider text is echoed.
func errorCode(raw []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	code := e.Error.Code
	if len(code) == 0 || len(code) > 64 {
		return ""
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '.' {
			return ""
		}
	}
	return code
}

func retryAfter(header string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs > 0 {
		return min(time.Duration(secs)*time.Second, 10*time.Minute)
	}
	return time.Minute
}
