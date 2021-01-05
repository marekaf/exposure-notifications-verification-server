// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package envstest

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/exposure-notifications-server/pkg/keys"
	"github.com/google/exposure-notifications-server/pkg/server"

	"github.com/google/exposure-notifications-verification-server/internal/clients"
	"github.com/google/exposure-notifications-verification-server/internal/routes"
	"github.com/google/exposure-notifications-verification-server/pkg/cache"
	"github.com/google/exposure-notifications-verification-server/pkg/config"
	"github.com/google/exposure-notifications-verification-server/pkg/database"

	"github.com/sethvargo/go-envconfig"
	"github.com/sethvargo/go-limiter"
)

// APIServerResponse is the response from a test APIServer instance.
type APIServerResponse struct {
	Config      *config.APIServerConfig
	Database    *database.Database
	Cacher      cache.Cacher
	KeyManager  keys.KeyManager
	RateLimiter limiter.Store
	Server      *server.Server
}

// NewAPIServer creates a new test APIServer server instance. See
// NewHarnessServer for more information.
func NewAPIServer(tb testing.TB, testDatabaseInstance *database.TestInstance) *APIServerResponse {
	return NewAPIServerConfig(tb, testDatabaseInstance).NewServer(tb)
}

// NewAPIServerClient creates a new API key and returns a client authorized with
// that API key. The generated API key is cleaned up by t.
func (r *APIServerResponse) NewAPIServerClient(tb testing.TB) *clients.APIServerClient {
	tb.Helper()

	realm, err := r.Database.FindRealm(1)
	if err != nil {
		tb.Fatal(err)
	}

	authApp := &database.AuthorizedApp{
		Name:       "Appy-" + randomString(tb, 6),
		APIKeyType: database.APIKeyTypeDevice,
	}
	apiKey, err := realm.CreateAuthorizedApp(r.Database, authApp, database.SystemTest)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		past := time.Now().UTC().Add(-30 * 24 * time.Hour)
		authApp.DeletedAt = &past
		if err := r.Database.SaveAuthorizedApp(authApp, database.SystemTest); err != nil && !database.IsNotFound(err) {
			tb.Fatal(err)
		}
	})

	u := &url.URL{Scheme: "http", Host: r.Server.Addr()}
	client, err := clients.NewAPIServerClient(u.String(), apiKey)
	if err != nil {
		tb.Fatal(err)
	}
	return client
}

// APIServerConfigResponse is the response from creating an API server config.
type APIServerConfigResponse struct {
	Config      *config.APIServerConfig
	Database    *database.Database
	Cacher      cache.Cacher
	KeyManager  keys.KeyManager
	RateLimiter limiter.Store
}

// NewAPIServerConfig creates a new API server configuration.
func NewAPIServerConfig(tb testing.TB, testDatabaseInstance *database.TestInstance) *APIServerConfigResponse {
	tb.Helper()

	ctx := context.Background()

	harness := NewTestHarness(tb, testDatabaseInstance)

	certificateSigningKey := keys.TestSigningKey(tb, harness.KeyManager)
	certTyp, ok := harness.KeyManager.(keys.SigningKeyManager)
	if !ok {
		tb.Fatal("kms cannot manage signing keys")
	}
	certificateSigningKeyVersion, err := certTyp.CreateKeyVersion(ctx, certificateSigningKey)
	if err != nil {
		tb.Fatal(err)
	}

	// Create the token key manager. We need both the signing key and the IDs, so
	// we cannot use the helper here.
	tokenSigningKey := keys.TestSigningKey(tb, harness.KeyManager)
	tokenTyp, ok := harness.KeyManager.(keys.SigningKeyManager)
	if !ok {
		tb.Fatal("kms cannot manage signing keys")
	}
	tokenSigningKeyVersion, err := tokenTyp.CreateKeyVersion(ctx, tokenSigningKey)
	if err != nil {
		tb.Fatal(err)
	}

	// Extract the kid from the key (this is a filesystem key).
	parts := strings.Split(tokenSigningKeyVersion, "/")
	if len(parts) == 0 {
		tb.Fatalf("invalid signing key version %q", tokenSigningKeyVersion)
	}
	kid := parts[len(parts)-1]

	// Create the config.
	cfg := &config.APIServerConfig{
		Database:                  *harness.DatabaseConfig,
		Observability:             *harness.ObservabilityConfig,
		Cache:                     *harness.CacheConfig,
		APIKeyCacheDuration:       5 * time.Second,
		VerificationTokenDuration: 5 * time.Second,
		CertificateSigning: config.CertificateSigningConfig{
			Keys:                  *harness.KeyManagerConfig,
			CertificateSigningKey: certificateSigningKeyVersion,
			CertificateIssuer:     "test-iss",
			CertificateAudience:   "test-aud",
		},
		TokenSigning: config.TokenSigningConfig{
			Keys:               *harness.KeyManagerConfig,
			TokenSigningKeys:   []string{tokenSigningKeyVersion},
			TokenSigningKeyIDs: []string{kid},
			TokenIssuer:        "test-iss",
		},
		RateLimit: *harness.RateLimiterConfig,
		DevMode:   true,
	}

	// Process the config - this simulates production setups and also ensures we
	// get the defaults for any unset values.
	emptyLookuper := envconfig.MapLookuper(nil)
	if err := config.ProcessWith(context.Background(), cfg, emptyLookuper); err != nil {
		tb.Fatal(err)
	}

	return &APIServerConfigResponse{
		Config:      cfg,
		Database:    harness.Database,
		Cacher:      harness.Cacher,
		KeyManager:  harness.KeyManager,
		RateLimiter: harness.RateLimiter,
	}
}

func (r *APIServerConfigResponse) NewServer(tb testing.TB) *APIServerResponse {
	ctx := context.Background()
	mux, closer, err := routes.APIServer(ctx, r.Config, r.Database, r.Cacher, r.RateLimiter, r.KeyManager, r.KeyManager)
	tb.Cleanup(closer)
	if err != nil {
		tb.Fatal(err)
	}

	srv := NewHarnessServer(tb, mux)

	return &APIServerResponse{
		Config:      r.Config,
		Database:    r.Database,
		Cacher:      r.Cacher,
		KeyManager:  r.KeyManager,
		RateLimiter: r.RateLimiter,
		Server:      srv,
	}
}