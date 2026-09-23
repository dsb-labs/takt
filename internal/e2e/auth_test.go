package e2e_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// testPolicy is the document the auth tests apply: one principal per role,
// and a group the fake identity provider asserts.
var testPolicy = manifest.Policy{
	Version: "v1",
	OIDC:    &manifest.PolicyOIDC{PrincipalClaim: "email", GroupsClaim: "groups"},
	Grants: []manifest.PolicyGrant{
		{Principals: []string{"scraper"}, Role: manifest.RoleViewer},
		{Principals: []string{"operator@example.com"}, Role: manifest.RoleOperator},
		{Principals: []string{"group:infra"}, Role: manifest.RoleAdmin},
	},
}

// TestAuthDisabled proves that a server without an [auth] block behaves as it
// always has: anonymous requests hold the whole API, and whoami says so.
func (s *Suite) TestAuthDisabled() {
	identity, err := s.client.WhoAmI(s.ctx())
	s.Require().NoError(err)

	s.False(identity.Enabled)
	s.Equal("anonymous", identity.Principal)
	s.Equal("admin", identity.Role)

	// Anonymous writes still work: the network boundary is the whole check.
	name := s.variableName()
	defer s.cleanupVariable(name)

	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: name, Value: "value"})
	s.Require().NoError(err)
}

// TestAuthLifecycle walks the whole enablement journey: init once, apply the
// first policy with the recovery token, mint per-principal tokens, and prove
// each role holds exactly what the policy grants until revocation cuts it
// off.
func (s *Suite) TestAuthLifecycle() {
	s.restart(withAuth())

	// The probes carry no security requirement: a supervisor must reach them
	// without credentials or it cannot manage the process.
	s.Require().NoError(s.client.Health(s.ctx()))

	// Everything else is refused until a credential exists.
	_, err := s.client.List(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected 401, got %v", err)

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	_, err = s.client.InitACL(s.ctx())
	s.Require().ErrorIs(err, client.ErrACLInitialized)

	// The recovery token sits above policy, which is what lets it apply the
	// first document to a server that grants nothing to anyone yet.
	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	_, viewerToken, err := admin.CreateToken(s.ctx(), "scraper")
	s.Require().NoError(err)

	_, operatorToken, err := admin.CreateToken(s.ctx(), "operator@example.com")
	s.Require().NoError(err)

	viewer := s.clientWithToken(viewerToken)
	operator := s.clientWithToken(operatorToken)

	// The viewer reads everything, including the endpoints prometheus
	// consumes, and mutates nothing.
	_, err = viewer.List(s.ctx())
	s.Require().NoError(err)
	s.Require().NoError(viewer.Metrics(s.ctx(), io.Discard))

	name := s.variableName()
	defer s.cleanupVariable(name)

	_, _, err = viewer.SetVariable(s.ctx(), manifest.Variable{Name: name, Value: "value"})
	s.Require().True(client.IsForbidden(err), "expected 403, got %v", err)

	_, err = viewer.ListTokens(s.ctx())
	s.Require().True(client.IsForbidden(err), "expected 403, got %v", err)

	identity, err := viewer.WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.True(identity.Enabled)
	s.Equal("scraper", identity.Principal)
	s.Equal("viewer", identity.Role)

	// The operator drives resource lifecycle.
	_, _, err = operator.SetVariable(s.ctx(), manifest.Variable{Name: name, Value: "value"})
	s.Require().NoError(err)

	// Revocation is immediate: the token list names the viewer's credential,
	// and deleting it refuses the very next request.
	tokens, err := admin.ListTokens(s.ctx())
	s.Require().NoError(err)

	var viewerID string
	for _, token := range tokens {
		if token.Principal == "scraper" {
			viewerID = token.ID
		}
	}
	s.Require().NotEmpty(viewerID)

	s.Require().NoError(admin.DeleteToken(s.ctx(), viewerID))

	_, err = viewer.List(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected 401 after revocation, got %v", err)

	// Logout is self-revocation, so the operator can retire its own token.
	s.Require().NoError(operator.Logout(s.ctx()))

	_, err = operator.WhoAmI(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected 401 after logout, got %v", err)

	// The recovery token's revocation path is deliberately host-level.
	s.Require().ErrorIs(admin.Logout(s.ctx()), client.ErrRecoveryLogout)
}

// TestWorkloadTokenMountAuthenticates covers the file form of
// workload identity: the manifest names a principal, the server mints and
// mounts a credential as the instance starts, and the policy alone decides
// what that principal may do — a granted one holds its role, an ungranted one
// authenticates and holds nothing.
func (s *Suite) TestWorkloadTokenMountAuthenticates() {
	s.restart(withAuth())

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{
		{Token: "scraper", To: "/var/run/takt/token", Signal: manifest.SignalHUP},
		// A principal the policy grants nothing. Nothing has to exist before a
		// manifest names one: identity is asserted, authority is granted.
		{Token: "nobody", To: "/var/run/takt/other"},
	}

	_, _, err = admin.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitRunningAs(admin, name)

	// The file holds the credential and nothing else, and it authenticates as
	// the principal the manifest named.
	credential := s.mountedFile(name, "/var/run/takt/token")
	s.Require().NotEmpty(credential, "the token was never mounted")

	scraper := s.clientWithToken(credential)

	identity, err := scraper.WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("scraper", identity.Principal)
	s.Equal("viewer", identity.Role)

	_, err = scraper.List(s.ctx())
	s.Require().NoError(err)

	// The ungranted principal authenticates and holds no role, exactly the
	// state a static token for one has. The policy stays the one description
	// of who may do what.
	other := s.clientWithToken(s.mountedFile(name, "/var/run/takt/other"))

	identity, err = other.WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("nobody", identity.Principal)
	s.Empty(identity.Role)

	_, err = other.List(s.ctx())
	s.Require().True(client.IsForbidden(err), "expected 403 for an ungranted principal, got %v", err)

	// Both credentials are auditable: the token list names them with the
	// workload source, so an operator can tell a projected credential from a
	// static one.
	tokens, err := admin.ListTokens(s.ctx())
	s.Require().NoError(err)

	minted := make(map[string]string, 2)
	for _, token := range tokens {
		if token.Source == "workload" {
			minted[token.Principal] = token.ID
		}
	}
	s.Contains(minted, "scraper")
	s.Contains(minted, "nobody")
}

// TestWorkloadEnvTokenRotatesWithTheInstance covers the env form: the
// credential is fixed at process start, so it lives for the instance and a
// replacement both mints a fresh one and revokes its predecessor.
func (s *Suite) TestWorkloadEnvTokenRotatesWithTheInstance() {
	s.restart(withAuth())

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"TAKT_TOKEN": "${token:scraper}"}

	_, _, err = admin.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitRunningAs(admin, name)

	credential := s.containerEnv(name, "TAKT_TOKEN")
	s.Require().NotEmpty(credential, "the token never reached the environment")

	identity, err := s.clientWithToken(credential).WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("scraper", identity.Principal)

	original := s.containers(name)
	s.Require().NotEmpty(original)

	_, err = admin.Restart(s.ctx(), name)
	s.Require().NoError(err)

	// A replaced instance gets a fresh credential. This is the whole rotation
	// story for the env form: an environment cannot change under a running
	// process, so rotation happens by replacement.
	var replacement string
	s.Require().Eventuallyf(func() bool {
		containers := s.containers(name)
		if len(containers) == 0 || containers[0] == original[0] {
			return false
		}

		replacement = s.containerEnv(name, "TAKT_TOKEN")

		return replacement != "" && replacement != credential
	}, convergeTimeout, 500*time.Millisecond, "the replacement never held a fresh token")

	_, err = s.clientWithToken(replacement).WhoAmI(s.ctx())
	s.Require().NoError(err)

	// And the predecessor died with its instance: the fresh mint replaced it,
	// so the token list never accumulates.
	s.Require().Eventuallyf(func() bool {
		_, err = s.clientWithToken(credential).WhoAmI(s.ctx())

		return client.IsUnauthorized(err)
	}, convergeTimeout, 500*time.Millisecond, "the replaced instance's token still authenticates")
}

// TestDeletingAWorkloadRevokesItsTokens covers the credential's life ending
// with the workload's, which is the wart the hand-managed static token had:
// nothing revoked it, and the token list accumulated credentials for
// workloads that were gone.
func (s *Suite) TestDeletingAWorkloadRevokesItsTokens() {
	s.restart(withAuth())

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Token: "scraper", To: "/var/run/takt/token"}}

	_, _, err = admin.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitRunningAs(admin, name)

	credential := s.mountedFile(name, "/var/run/takt/token")
	s.Require().NotEmpty(credential, "the token was never mounted")

	_, err = s.clientWithToken(credential).WhoAmI(s.ctx())
	s.Require().NoError(err)

	_, err = admin.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// Revocation is immediate once the teardown lands: the very next request
	// presenting the credential finds nothing to hash to.
	s.Require().Eventuallyf(func() bool {
		_, err = s.clientWithToken(credential).WhoAmI(s.ctx())

		return client.IsUnauthorized(err)
	}, convergeTimeout, 500*time.Millisecond, "the deleted workload's token still authenticates")

	tokens, err := admin.ListTokens(s.ctx())
	s.Require().NoError(err)

	for _, token := range tokens {
		s.NotEqual("workload", token.Source, "a workload-minted token outlived its workload")
	}
}

// TestAuthPolicyConflict proves that a stale conditional apply is refused
// rather than silently clobbering a concurrent one.
func (s *Suite) TestAuthPolicyConflict() {
	s.restart(withAuth())

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	current, err := admin.GetPolicy(s.ctx())
	s.Require().NoError(err)

	// The first apply consumes the tag the second one still holds.
	_, err = admin.ApplyPolicy(s.ctx(), testPolicy, client.WithIfMatch(current.ETag))
	s.Require().NoError(err)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy, client.WithIfMatch(current.ETag))
	s.Require().ErrorIs(err, client.ErrPolicyChanged)

	// The one-step form reads the fresh tag itself, which is the re-run the
	// error asks for.
	applied, err := admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)
	s.Equal(testPolicy, applied.Spec)
}

// TestAuthReset proves the lockout recovery: the reset file removes the
// recovery token at startup, init works again, and everything else survives.
func (s *Suite) TestAuthReset() {
	directory := s.T().TempDir()
	s.restart(withAuth(), withDataDirectory(directory))

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	_, clientToken, err := admin.CreateToken(s.ctx(), "scraper")
	s.Require().NoError(err)

	// The operator lost the recovery token. Writing the reset file into the
	// data directory and restarting is the whole procedure.
	s.Require().NoError(os.WriteFile(filepath.Join(directory, "acl.reset"), nil, 0o600))
	s.restart(withAuth(), withDataDirectory(directory))

	// The old recovery token is gone, init works exactly once again, and the
	// client tokens and policy survived.
	_, err = s.clientWithToken(recovery).ListTokens(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected the old recovery token to be revoked, got %v", err)

	_, err = s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	identity, err := s.clientWithToken(clientToken).WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("scraper", identity.Principal)
	s.Equal("viewer", identity.Role)
}

// TestAuthOIDC proves the whole exchange: an authorization code becomes an
// identity token signed by the issuer, which becomes a short-lived client
// token whose principal and groups come from the claims the policy maps.
func (s *Suite) TestAuthOIDC() {
	issuer, sign, answer := s.fakeIssuer()

	s.restart(withOIDC(issuer, "takt"))

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	_, err = s.clientWithToken(recovery).ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	// The CLI's flow discovers the issuer from the server rather than from
	// flags, so the discovery endpoint has to answer anonymously.
	discovered, err := s.client.GetOIDC(s.ctx())
	s.Require().NoError(err)
	s.Equal(issuer, discovered.Issuer)
	s.Equal("takt", discovered.ClientID)

	// The CLI's flow ends at the authorization code: the server performs the
	// exchange, because the exchange is what needs the client secret. The
	// infra group carries admin through the policy's group grant, so the
	// login proves the groups claim travelled from the identity token.
	answer("infra-code", sign(map[string]any{
		"email":  "david@example.com",
		"groups": []string{"infra"},
	}))

	login, err := s.client.LoginCode(s.ctx(), "infra-code", "any-verifier", "http://127.0.0.1:8250/oidc/callback")
	s.Require().NoError(err)
	s.Equal("david@example.com", login.Principal)
	s.False(login.ExpiresAt.IsZero())

	identity, err := s.clientWithToken(login.Credential).WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("david@example.com", identity.Principal)
	s.Equal("admin", identity.Role)
	s.Contains(identity.Groups, "infra")

	// An identity signed by somebody else is refused, even when the issuer
	// hands it over in exchange for a code.
	_, wrongSign, _ := s.fakeIssuer()

	answer("forged-code", wrongSign(map[string]any{"email": "forger@example.com"}))

	_, err = s.client.LoginCode(s.ctx(), "forged-code", "any-verifier", "http://127.0.0.1:8250/oidc/callback")
	s.Require().True(client.IsUnauthorized(err), "expected 401 for a foreign signature, got %v", err)

	// A code the issuer never handed out is refused by the issuer, and the
	// refusal reaches the caller as an invalid credential.
	_, err = s.client.LoginCode(s.ctx(), "unknown-code", "any-verifier", "http://127.0.0.1:8250/oidc/callback")
	s.Require().True(client.IsUnauthorized(err), "expected 401 for an unknown code, got %v", err)

	// A redirect that is not loopback would make the server an exchange
	// oracle for codes obtained some other way, so it is refused.
	_, err = s.client.LoginCode(s.ctx(), "infra-code", "any-verifier", "https://evil.example.com/callback")
	s.Require().True(client.IsBadRequest(err), "expected 400 for a foreign redirect, got %v", err)
}

// fakeIssuer runs an OIDC issuer for the test: a discovery document, a JWKS,
// a signer that mints identity tokens the way the real one would, and a
// token endpoint that answers each authorization code with the identity the
// test registered for it.
func (s *Suite) fakeIssuer() (string, func(claims map[string]any) string, func(code, idToken string)) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	s.Require().NoError(err)

	mux := http.NewServeMux()
	issuer := httptest.NewServer(mux)
	s.T().Cleanup(issuer.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer.URL,
			"authorization_endpoint":                issuer.URL + "/authorize",
			"token_endpoint":                        issuer.URL + "/token",
			"jwks_uri":                              issuer.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})

	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}},
		})
	})

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, &jose.SignerOptions{
		ExtraHeaders: map[jose.HeaderKey]any{"kid": "test"},
	})
	s.Require().NoError(err)

	sign := func(claims map[string]any) string {
		now := time.Now()

		token := map[string]any{
			"iss": issuer.URL,
			"aud": "takt",
			"sub": fmt.Sprintf("subject-%d", now.UnixNano()),
			"iat": now.Unix(),
			"exp": now.Add(time.Hour).Unix(),
		}
		for name, value := range claims {
			token[name] = value
		}

		payload, err := json.Marshal(token)
		s.Require().NoError(err)

		signed, err := signer.Sign(payload)
		s.Require().NoError(err)

		serialized, err := signed.CompactSerialize()
		s.Require().NoError(err)

		return serialized
	}

	// The token endpoint stands in for the exchange the server performs on
	// the CLI's behalf. It answers a registered code with the identity the
	// test chose for it, and refuses any other the way a real issuer would.
	var answers sync.Map

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		idToken, ok := answers.Load(r.FormValue("code"))
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access",
			"token_type":   "bearer",
			"id_token":     idToken,
		})
	})

	answer := func(code, idToken string) {
		answers.Store(code, idToken)
	}

	return issuer.URL, sign, answer
}
