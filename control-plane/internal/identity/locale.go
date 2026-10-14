package identity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// ErrInvalidOrganizationName is returned for an installation name longer than
// OrganizationNameLimit characters after trimming.
var ErrInvalidOrganizationName = errors.New("identity: invalid organization name")

// ErrEmptyOrganizationSettingsUpdate is returned when an update names no
// setting to change.
var ErrEmptyOrganizationSettingsUpdate = errors.New("identity: organization settings update changes nothing")

// OrganizationNameLimit bounds the installation name (DES-HOR-459-08).
const OrganizationNameLimit = 80

// NormalizeOrganizationName trims an installation name and validates its
// length. Empty is allowed and means unset.
func NormalizeOrganizationName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > OrganizationNameLimit {
		return "", ErrInvalidOrganizationName
	}
	return name, nil
}

// ValidLocale reports whether locale is a supported product locale.
func ValidLocale(locale string) bool {
	return locale == LocaleEN || locale == LocalePT
}

// OrganizationSettings is the deployment-wide settings row. DefaultLocale is
// empty until an Admin chooses one; Name is empty until seeded or set.
type OrganizationSettings struct {
	DefaultLocale       string
	Name                string
	UpdatedByIdentityID string
	UpdatedAt           time.Time
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GetOrganizationSettings returns the deployment-wide settings.
func (s *Store) GetOrganizationSettings(ctx context.Context) (OrganizationSettings, error) {
	return getOrganizationSettings(ctx, s.pool, "")
}

const organizationSettingsQuery = `
	SELECT COALESCE(default_locale, ''), name, COALESCE(updated_by_identity_id::text, ''), updated_at
	FROM identity.organization_settings WHERE id`

// getOrganizationSettings reads the settings row; lock is appended to the
// query (for example " FOR UPDATE").
func getOrganizationSettings(ctx context.Context, q rowQuerier, lock string) (OrganizationSettings, error) {
	var settings OrganizationSettings
	err := q.QueryRow(ctx, organizationSettingsQuery+lock).Scan(
		&settings.DefaultLocale, &settings.Name, &settings.UpdatedByIdentityID, &settings.UpdatedAt)
	if err != nil {
		return OrganizationSettings{}, fmt.Errorf("get organization settings: %w", err)
	}
	return settings, nil
}

// OrganizationSettingsUpdate names the settings an Admin changes; nil fields
// are left as they are.
type OrganizationSettingsUpdate struct {
	DefaultLocale *string
	Name          *string
}

// normalized validates the update and trims the name.
func (u OrganizationSettingsUpdate) normalized() (OrganizationSettingsUpdate, error) {
	if u.DefaultLocale == nil && u.Name == nil {
		return u, ErrEmptyOrganizationSettingsUpdate
	}
	if u.DefaultLocale != nil && !ValidLocale(*u.DefaultLocale) {
		return u, ErrInvalidLocale
	}
	if u.Name != nil {
		name, err := NormalizeOrganizationName(*u.Name)
		if err != nil {
			return u, err
		}
		u.Name = &name
	}
	return u, nil
}

// auditDetail records each changed setting with its previous value.
func (u OrganizationSettingsUpdate) auditDetail(previous OrganizationSettings) map[string]any {
	detail := map[string]any{}
	if u.DefaultLocale != nil {
		detail["defaultLocale"] = *u.DefaultLocale
		detail["previousDefaultLocale"] = previous.DefaultLocale
	}
	if u.Name != nil {
		detail["name"] = *u.Name
		detail["previousName"] = previous.Name
	}
	return detail
}

// UpdateOrganizationSettings changes the organisation default language and/or
// the installation name. It is an Admin action logged with the actor (HOR-459
// "Settings changes are logged with the person's name"). A non-nil expected
// version must match the stored one, so a stale tab cannot overwrite a newer
// change.
func (s *Store) UpdateOrganizationSettings(ctx context.Context, actorID string, update OrganizationSettingsUpdate, expected *time.Time, now time.Time) (OrganizationSettings, error) {
	update, err := update.normalized()
	if err != nil {
		return OrganizationSettings{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OrganizationSettings{}, fmt.Errorf("begin organization settings update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row before comparing versions, so concurrent writers based on
	// the same version serialize and the later one sees the change.
	previous, err := getOrganizationSettings(ctx, tx, " FOR UPDATE")
	if err != nil {
		return OrganizationSettings{}, err
	}
	if expected != nil && !previous.UpdatedAt.Equal(*expected) {
		return OrganizationSettings{}, ErrOrganizationSettingsConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE identity.organization_settings
		SET default_locale = COALESCE($1, default_locale), name = COALESCE($2, name), updated_by_identity_id = $3
		WHERE id`, update.DefaultLocale, update.Name, actorID); err != nil {
		return OrganizationSettings{}, fmt.Errorf("update organization settings: %w", err)
	}
	if err := AppendSecurityEventTx(ctx, tx, SecurityEvent{
		Event:                     EventOrganizationSettingsUpdated,
		Outcome:                   OutcomeSuccess,
		InitiatingHumanIdentityID: actorID,
		RequestActorIdentityID:    actorID,
		CredentialKind:            CredentialBrowser,
		Detail:                    update.auditDetail(previous),
		CreatedAt:                 now,
	}); err != nil {
		return OrganizationSettings{}, err
	}
	updated, err := getOrganizationSettings(ctx, tx, "")
	if err != nil {
		return OrganizationSettings{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OrganizationSettings{}, fmt.Errorf("commit organization settings update: %w", err)
	}
	return updated, nil
}

// SeedOrganizationName stores the installation name from deployment
// configuration only while no name is stored, so it never overwrites a name an
// Admin set. It reports whether the seed was stored.
func (s *Store) SeedOrganizationName(ctx context.Context, seed string) (bool, error) {
	name, err := NormalizeOrganizationName(seed)
	if err != nil {
		return false, err
	}
	if name == "" {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE identity.organization_settings SET name = $1 WHERE id AND name = ''`, name)
	if err != nil {
		return false, fmt.Errorf("seed organization name: %w", err)
	}
	return tag.RowsAffected() == 1, nil
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
