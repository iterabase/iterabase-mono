package identity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocaleFromAcceptLanguage(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                                "",
		"pt-PT,pt;q=0.9,en;q=0.8":         "pt",
		"pt-BR":                           "pt",
		"en-GB,en;q=0.9":                  "en",
		"fr-FR,fr;q=0.9":                  "",
		"fr-FR,en;q=0.5,pt;q=0.7":         "pt",
		"de, en;q=0.8, pt;q=0":            "en",
		"EN-us":                           "en",
		"*":                               "",
		"es;q=1.0, pt-PT;q=0.4, en;q=0.4": "pt",
	}
	for header, want := range cases {
		assert.Equal(t, want, LocaleFromAcceptLanguage(header), "Accept-Language %q", header)
	}
}

func TestResolveWebLocaleOrder(t *testing.T) {
	t.Parallel()
	// The person's choice wins, then the organisation, then the browser, then
	// English (DES-HOR-670-02).
	assert.Equal(t, "pt", ResolveWebLocale("pt", "en", "en-GB"))
	assert.Equal(t, "en", ResolveWebLocale("", "en", "pt-PT"))
	assert.Equal(t, "pt", ResolveWebLocale("", "", "pt-PT"))
	assert.Equal(t, "en", ResolveWebLocale("", "", "fr"))
	assert.Equal(t, "en", ResolveWebLocale("xx", "yy", ""))
}

func TestResolveLocaleAndOrganizationDefault(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	admin := h.createAdmin(t, "admin@example.com")
	follower, err := h.store.UpsertLocalUser(ctx, "rita@example.com", "rita@example.com", "operator")
	require.NoError(t, err)
	assert.Empty(t, follower.Locale, "a person who never chose follows the organisation")

	// Nothing chosen anywhere: English.
	locale, err := h.store.ResolveLocale(ctx, follower.ID)
	require.NoError(t, err)
	assert.Equal(t, "en", locale)
	settings, err := h.store.GetOrganizationSettings(ctx)
	require.NoError(t, err)
	assert.Empty(t, settings.DefaultLocale)

	// The organisation default applies to people who follow it.
	_, err = h.store.UpdateOrganizationSettings(ctx, admin.ID, OrganizationSettingsUpdate{DefaultLocale: new("xx")}, nil, h.now)
	require.ErrorIs(t, err, ErrInvalidLocale)
	updated, err := h.store.UpdateOrganizationSettings(ctx, admin.ID, OrganizationSettingsUpdate{DefaultLocale: new("pt")}, &settings.UpdatedAt, h.now)
	require.NoError(t, err)
	assert.Equal(t, "pt", updated.DefaultLocale)
	assert.Equal(t, admin.ID, updated.UpdatedByIdentityID)
	locale, err = h.store.ResolveLocale(ctx, follower.ID)
	require.NoError(t, err)
	assert.Equal(t, "pt", locale)

	// A stale version cannot overwrite a newer change.
	_, err = h.store.UpdateOrganizationSettings(ctx, admin.ID, OrganizationSettingsUpdate{DefaultLocale: new("en")}, &settings.UpdatedAt, h.now)
	require.ErrorIs(t, err, ErrOrganizationSettingsConflict)

	// The change is logged with the Admin who made it.
	var actor, detail string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT request_actor_identity_id::text, detail::text FROM identity.security_events
		WHERE event = $1`, EventOrganizationSettingsUpdated).Scan(&actor, &detail))
	assert.Equal(t, admin.ID, actor)
	assert.JSONEq(t, `{"defaultLocale":"pt","previousDefaultLocale":""}`, detail)

	// A person's own choice wins; clearing it follows the organisation again.
	_, err = h.store.UpdateLocalUserProfile(ctx, follower.ID, "Rita", "en", h.now)
	require.NoError(t, err)
	locale, err = h.store.ResolveLocale(ctx, follower.ID)
	require.NoError(t, err)
	assert.Equal(t, "en", locale)
	cleared, err := h.store.UpdateLocalUserProfile(ctx, follower.ID, "Rita", "", h.now)
	require.NoError(t, err)
	assert.Empty(t, cleared.Locale)
	locale, err = h.store.ResolveLocale(ctx, follower.ID)
	require.NoError(t, err)
	assert.Equal(t, "pt", locale)

	_, err = h.store.UpdateLocalUserProfile(ctx, follower.ID, "Rita", "es", h.now)
	require.ErrorIs(t, err, ErrInvalidLocale)
	_, err = h.store.ResolveLocale(ctx, "00000000-0000-0000-0000-000000000000")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAuthEmailsFollowTheRecipientsResolvedLocale(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	admin := h.createAdmin(t, "admin@example.com")
	user, err := h.store.UpsertLocalUser(ctx, "rita@example.com", "rita@example.com", "operator")
	require.NoError(t, err)
	_, err = h.store.CompleteSetup(ctx, mustSetupToken(t, h.store, user.ID, h.now), "Rita", "en", "correct-horse-battery", h.now)
	require.NoError(t, err)
	h.deliver(t)
	sent := h.sender.count()

	// Rita follows the organisation, which is Portuguese: her reset email is
	// Portuguese.
	_, err = h.store.UpdateLocalUserProfile(ctx, user.ID, "Rita", "", h.now)
	require.NoError(t, err)
	_, err = h.store.UpdateOrganizationSettings(ctx, admin.ID, OrganizationSettingsUpdate{DefaultLocale: new("pt")}, nil, h.now)
	require.NoError(t, err)
	queued, err := h.store.RequestPasswordReset(ctx, "rita@example.com", h.now)
	require.NoError(t, err)
	require.True(t, queued)
	h.deliver(t)
	require.Equal(t, sent+1, h.sender.count())
	assert.Contains(t, h.sender.messages[sent].Subject, "palavra-passe")
}

func TestOrganizationDefaultLocaleConcurrentWritersConflict(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	admin := h.createAdmin(t, "admin@example.com")
	settings, err := h.store.GetOrganizationSettings(ctx)
	require.NoError(t, err)

	// Hold the row lock so both writers read the same version and queue up.
	blocker, err := h.store.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = blocker.Exec(ctx, `SELECT 1 FROM identity.organization_settings WHERE id FOR UPDATE`)
	require.NoError(t, err)

	results := make([]error, 2)
	var wg sync.WaitGroup
	for i, locale := range []string{"pt", "en"} {
		wg.Add(1)
		go func(i int, locale string) {
			defer wg.Done()
			_, results[i] = h.store.UpdateOrganizationSettings(ctx, admin.ID, OrganizationSettingsUpdate{DefaultLocale: new(locale)}, &settings.UpdatedAt, h.now)
		}(i, locale)
	}
	require.Eventually(t, func() bool {
		var waiting int
		_ = h.store.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%organization_settings%'`).Scan(&waiting)
		return waiting == 2
	}, 10*time.Second, 20*time.Millisecond, "both writers wait on the settings row")
	require.NoError(t, blocker.Rollback(ctx))
	wg.Wait()

	// Exactly one same-version write wins; the other is a conflict.
	var succeeded, conflicted int
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrOrganizationSettingsConflict):
			conflicted++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, conflicted)
	var events int
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM identity.security_events WHERE event = $1`, EventOrganizationSettingsUpdated).Scan(&events))
	assert.Equal(t, 1, events, "only the winning write is audited")
}

// TestOrganizationNameUpdateAndSeed covers DES-HOR-459-08: the installation
// name is trimmed and bounded, its change is logged with the Admin, and the
// deployment seed is stored only while no name is stored.
func TestOrganizationNameUpdateAndSeed(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	admin := h.createAdmin(t, "admin@example.com")

	// Nothing to seed; then the first seed is stored (trimmed).
	seeded, err := h.store.SeedOrganizationName(ctx, "   ")
	require.NoError(t, err)
	assert.False(t, seeded)
	_, err = h.store.SeedOrganizationName(ctx, strings.Repeat("n", OrganizationNameLimit+1))
	require.ErrorIs(t, err, ErrInvalidOrganizationName)
	seeded, err = h.store.SeedOrganizationName(ctx, " Exemplo Logística ")
	require.NoError(t, err)
	assert.True(t, seeded)
	settings, err := h.store.GetOrganizationSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Exemplo Logística", settings.Name)

	// A stored name is never overwritten by a later seed.
	seeded, err = h.store.SeedOrganizationName(ctx, "Another")
	require.NoError(t, err)
	assert.False(t, seeded)

	// An Admin renames it; the change is logged with the actor and the
	// language is untouched.
	_, err = h.store.UpdateOrganizationSettings(ctx, admin.ID, OrganizationSettingsUpdate{}, nil, h.now)
	require.ErrorIs(t, err, ErrEmptyOrganizationSettingsUpdate)
	_, err = h.store.UpdateOrganizationSettings(ctx, admin.ID,
		OrganizationSettingsUpdate{Name: new(strings.Repeat("ã", OrganizationNameLimit+1))}, nil, h.now)
	require.ErrorIs(t, err, ErrInvalidOrganizationName)
	updated, err := h.store.UpdateOrganizationSettings(ctx, admin.ID,
		OrganizationSettingsUpdate{Name: new("  Exemplo  ")}, &settings.UpdatedAt, h.now)
	require.NoError(t, err)
	assert.Equal(t, "Exemplo", updated.Name)
	assert.Empty(t, updated.DefaultLocale)
	assert.Equal(t, admin.ID, updated.UpdatedByIdentityID)
	var actor, detail string
	require.NoError(t, h.store.pool.QueryRow(ctx, `
		SELECT request_actor_identity_id::text, detail::text FROM identity.security_events
		WHERE event = $1`, EventOrganizationSettingsUpdated).Scan(&actor, &detail))
	assert.Equal(t, admin.ID, actor)
	assert.JSONEq(t, `{"name":"Exemplo","previousName":"Exemplo Logística"}`, detail)

	// An Admin-set name survives the next startup's seed; an Admin may clear
	// it, after which the seed applies again.
	seeded, err = h.store.SeedOrganizationName(ctx, "Exemplo Logística")
	require.NoError(t, err)
	assert.False(t, seeded)
	cleared, err := h.store.UpdateOrganizationSettings(ctx, admin.ID, OrganizationSettingsUpdate{Name: new("")}, nil, h.now)
	require.NoError(t, err)
	assert.Empty(t, cleared.Name)
}
