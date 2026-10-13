package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/config"
)

func authBaseConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Auth.PublicOrigin = "https://app.example.com"
	cfg.Auth.Bootstrap.AdminEmail = "admin@example.com"
	cfg.Auth.Bootstrap.AdminLocale = "en"
	cfg.Auth.Email.Host = "smtp.example.com"
	cfg.Auth.Email.Port = 587
	cfg.Auth.Email.Mode = "starttls"
	cfg.Auth.Email.From = "auth@example.com"
	return cfg
}

func TestValidateAuthServeIsAlwaysRequired(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	require.Error(t, config.ValidateAuthServe(cfg), "there is no unauthenticated customer mode")
	origin, err := config.AuthPublicOrigin(cfg)
	assert.Error(t, err)
	assert.Empty(t, origin)
}

func TestValidateAuthServeRejectsInvalidPrerequisites(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*config.Config){
		"missing origin":       func(c *config.Config) { c.Auth.PublicOrigin = "" },
		"insecure origin":      func(c *config.Config) { c.Auth.PublicOrigin = "http://app.example.com" },
		"origin with path":     func(c *config.Config) { c.Auth.PublicOrigin = "https://app.example.com/settings" },
		"origin with userinfo": func(c *config.Config) { c.Auth.PublicOrigin = "https://user:pass@app.example.com" },
		"missing smtp host":    func(c *config.Config) { c.Auth.Email.Host = "" },
		"cleartext smtp mode":  func(c *config.Config) { c.Auth.Email.Mode = "plain" },
		"missing smtp from":    func(c *config.Config) { c.Auth.Email.From = "" },
		"missing bootstrap":    func(c *config.Config) { c.Auth.Bootstrap.AdminEmail = "" },
		"bad locale":           func(c *config.Config) { c.Auth.Bootstrap.AdminLocale = "fr" },
		"password without user": func(c *config.Config) {
			c.Auth.Email.Password = "secret"
		},
		"weak session bound": func(c *config.Config) {
			c.Auth.Session.IdleTTL = "24h"
		},
		"weak absolute bound": func(c *config.Config) {
			c.Auth.Session.AbsoluteTTL = "800h"
		},
		"weak recent auth bound": func(c *config.Config) {
			c.Auth.Session.RecentAuthTTL = "1h"
		},
		"absolute shorter than idle": func(c *config.Config) {
			c.Auth.Session.IdleTTL = "10h"
			c.Auth.Session.AbsoluteTTL = "1h"
		},
		"invalid proxy cidr": func(c *config.Config) {
			c.Auth.TrustedProxies = []string{"not-a-cidr"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := authBaseConfig()
			mutate(cfg)
			assert.Error(t, config.ValidateAuthServe(cfg))
		})
	}
}

func TestValidateAuthServeAcceptsTightenedBounds(t *testing.T) {
	t.Parallel()
	cfg := authBaseConfig()
	cfg.Auth.Session.IdleTTL = "1h"
	cfg.Auth.Session.AbsoluteTTL = "24h"
	cfg.Auth.Session.RecentAuthTTL = "5m"
	cfg.Auth.TrustedProxies = []string{"10.0.0.0/8", "192.168.0.0/16"}
	require.NoError(t, config.ValidateAuthServe(cfg))

	idle, absolute, recent, err := config.AuthSessionTTLs(cfg)
	require.NoError(t, err)
	assert.Equal(t, time.Hour, idle)
	assert.Equal(t, 24*time.Hour, absolute)
	assert.Equal(t, 5*time.Minute, recent)

	proxies, err := config.AuthTrustedProxyNets(cfg)
	require.NoError(t, err)
	require.Len(t, proxies, 2)

	origin, err := config.AuthPublicOrigin(cfg)
	require.NoError(t, err)
	assert.Equal(t, "https://app.example.com", origin)

	cfg.Auth.PublicOrigin = "https://app.example.com/"
	origin, err = config.AuthPublicOrigin(cfg)
	require.NoError(t, err)
	assert.Equal(t, "https://app.example.com", origin, "trailing slash is normalized away")
}

func TestAuthSessionTTLsDefaultToApprovedMaximums(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	idle, absolute, recent, err := config.AuthSessionTTLs(cfg)
	require.NoError(t, err)
	assert.Equal(t, 12*time.Hour, idle)
	assert.Equal(t, 30*24*time.Hour, absolute)
	assert.Equal(t, 15*time.Minute, recent)
}

func TestValidateServeIncludesAuthValidation(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.API.Addr = ":8080"
	assert.Error(t, config.ValidateServe(cfg), "auth without prerequisites must fail serve validation")

	cfg = authBaseConfig()
	cfg.API.Addr = ":8080"
	assert.NoError(t, config.ValidateServe(cfg))
}
