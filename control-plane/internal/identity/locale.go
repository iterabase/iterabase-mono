package identity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Product locales (DES-HOR-670-02). Stored codes stay "en"/"pt"; the UI maps
// "pt" to pt-PT for its catalogs and Intl formatting.
const (
	LocaleEN = "en"
	LocalePT = "pt"
	// FallbackLocale is the last resort when neither the person, the
	// organisation nor (for web requests) the browser decides.
	FallbackLocale = LocaleEN
)

// EventOrganizationSettingsUpdated records an Admin changing organisation
// settings such as the default language.
const EventOrganizationSettingsUpdated = "organization_settings_updated"

// ErrInvalidLocale is returned for any locale outside exactly en|pt.
var ErrInvalidLocale = errors.New("identity: invalid locale")

// ErrOrganizationSettingsConflict is returned when an organisation settings
// update is based on a stale version.
var ErrOrganizationSettingsConflict = errors.New("identity: organization settings changed since they were loaded")

// ValidLocale reports whether locale is a supported product locale.
func ValidLocale(locale string) bool {
	return locale == LocaleEN || locale == LocalePT
}

// OrganizationSettings is the deployment-wide settings row. DefaultLocale is
// empty until an Admin chooses one.
type OrganizationSettings struct {
	DefaultLocale       string
	UpdatedByIdentityID string
	UpdatedAt           time.Time
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GetOrganizationSettings returns the deployment-wide settings.
func (s *Store) GetOrganizationSettings(ctx context.Context) (OrganizationSettings, error) {
	return getOrganizationSettings(ctx, s.pool)
}

func getOrganizationSettings(ctx context.Context, q rowQuerier) (OrganizationSettings, error) {
	var settings OrganizationSettings
	err := q.QueryRow(ctx, `
		SELECT COALESCE(default_locale, ''), COALESCE(updated_by_identity_id::text, ''), updated_at
		FROM identity.organization_settings WHERE id`).Scan(
		&settings.DefaultLocale, &settings.UpdatedByIdentityID, &settings.UpdatedAt)
	if err != nil {
		return OrganizationSettings{}, fmt.Errorf("get organization settings: %w", err)
	}
	return settings, nil
}

// UpdateOrganizationDefaultLocale sets the organisation default language. It
// is an Admin action logged with the actor (HOR-459 "Settings changes are
// logged with the person's name"). A non-nil expected version must match the
// stored one, so a stale tab cannot overwrite a newer change.
func (s *Store) UpdateOrganizationDefaultLocale(ctx context.Context, actorID, locale string, expected *time.Time, now time.Time) (OrganizationSettings, error) {
	if !ValidLocale(locale) {
		return OrganizationSettings{}, ErrInvalidLocale
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OrganizationSettings{}, fmt.Errorf("begin organization settings update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	previous, err := getOrganizationSettings(ctx, tx)
	if err != nil {
		return OrganizationSettings{}, err
	}
	if expected != nil && !previous.UpdatedAt.Equal(*expected) {
		return OrganizationSettings{}, ErrOrganizationSettingsConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.organization_settings
		SET default_locale = $1, updated_by_identity_id = $2
		WHERE id`, locale, actorID); err != nil {
		return OrganizationSettings{}, fmt.Errorf("update organization settings: %w", err)
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:                     EventOrganizationSettingsUpdated,
		Outcome:                   OutcomeSuccess,
		InitiatingHumanIdentityID: actorID,
		RequestActorIdentityID:    actorID,
		CredentialKind:            CredentialBrowser,
		Detail: map[string]any{
			"defaultLocale":         locale,
			"previousDefaultLocale": previous.DefaultLocale,
		},
		CreatedAt: now,
	}); err != nil {
		return OrganizationSettings{}, err
	}
	updated, err := getOrganizationSettings(ctx, tx)
	if err != nil {
		return OrganizationSettings{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OrganizationSettings{}, fmt.Errorf("commit organization settings update: %w", err)
	}
	return updated, nil
}

// ResolveLocale returns the language to use for a recipient when no browser is
// involved: the person's choice, then the organisation default, then English.
// It is the contract Sofia, push and Teams notifications call
// (DES-HOR-670-02/03). Outbound customer emails never use it: they keep the
// thread or template language.
func (s *Store) ResolveLocale(ctx context.Context, identityID string) (string, error) {
	return resolveLocale(ctx, s.pool, identityID)
}

func resolveLocale(ctx context.Context, q rowQuerier, identityID string) (string, error) {
	var locale string
	err := q.QueryRow(ctx, `
		SELECT COALESCE(lu.locale,
		                (SELECT default_locale FROM identity.organization_settings WHERE id),
		                $2)
		FROM identity.local_users lu
		WHERE lu.identity_id = $1`, identityID, FallbackLocale).Scan(&locale)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve locale: %w", err)
	}
	return locale, nil
}

// ResolveWebLocale resolves the effective UI language for a web request: the
// person's choice, then the organisation default, then the browser's
// Accept-Language, then English. Empty values mean "not chosen".
func ResolveWebLocale(preference, organizationDefault, acceptLanguage string) string {
	if ValidLocale(preference) {
		return preference
	}
	if ValidLocale(organizationDefault) {
		return organizationDefault
	}
	if locale := LocaleFromAcceptLanguage(acceptLanguage); locale != "" {
		return locale
	}
	return FallbackLocale
}

// LocaleFromAcceptLanguage returns the supported locale the browser prefers
// most, or "" when it names none. Any Portuguese variant maps to pt (pt-PT is
// the only Portuguese the product ships).
func LocaleFromAcceptLanguage(header string) string {
	type candidate struct {
		locale string
		q      float64
	}
	var candidates []candidate
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		tag := strings.ToLower(strings.TrimSpace(fields[0]))
		primary, _, _ := strings.Cut(tag, "-")
		if !ValidLocale(primary) {
			continue
		}
		q := 1.0
		for _, param := range fields[1:] {
			name, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(name, "q") {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil {
					q = parsed
				}
			}
		}
		if q <= 0 {
			continue
		}
		candidates = append(candidates, candidate{locale: primary, q: q})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		return candidates[a].q > candidates[b].q
	})
	return candidates[0].locale
}
