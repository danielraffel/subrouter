package stackauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifierAcceptsAuthenticatedSelectedTeam(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	projectID := "project-1"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "key-1", "kty": "EC", "crv": "P-256", "alg": "ES256",
			"x": encodeBigInt(privateKey.X), "y": encodeBigInt(privateKey.Y),
		}}})
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	token := signedToken(t, privateKey, map[string]any{
		"sub":              "user-1",
		"project_id":       projectID,
		"selected_team_id": "team-1",
		"email":            "user@example.com",
		"role":             "authenticated",
		"iss":              server.URL + "/projects/" + projectID,
		"aud":              []string{projectID},
		"exp":              now.Add(time.Hour).Unix(),
		"is_anonymous":     false,
		"is_restricted":    false,
	})
	verifier := &Verifier{
		APIURL: server.URL, ProjectID: projectID, HTTPClient: server.Client(),
		Now: func() time.Time { return now },
	}
	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SelectedTeamID != "team-1" || claims.Email != "user@example.com" {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestVerifierRejectsAnonymousAndWrongProject(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "key-1", "kty": "EC", "crv": "P-256", "alg": "ES256",
			"x": encodeBigInt(privateKey.X), "y": encodeBigInt(privateKey.Y),
		}}})
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	verifier := &Verifier{
		APIURL: server.URL, ProjectID: "project-1", HTTPClient: server.Client(),
		Now: func() time.Time { return now },
	}
	for name, overrides := range map[string]map[string]any{
		"anonymous":     {"is_anonymous": true},
		"wrong project": {"project_id": "project-2"},
		"missing team":  {"selected_team_id": ""},
	} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"sub": "user-1", "project_id": "project-1", "selected_team_id": "team-1",
				"role": "authenticated", "iss": server.URL + "/projects/project-1",
				"aud": []string{"project-1"}, "exp": now.Add(time.Hour).Unix(),
				"is_anonymous": false, "is_restricted": false,
			}
			for key, value := range overrides {
				claims[key] = value
			}
			token := signedToken(t, privateKey, claims)
			if _, err := verifier.Verify(context.Background(), token); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func signedToken(t *testing.T, privateKey *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "key-1", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	first := base64.RawURLEncoding.EncodeToString(header)
	second := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := first + "." + second
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(raw)
}

func encodeBigInt(value *big.Int) string {
	raw := make([]byte, 32)
	value.FillBytes(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestAudienceContains(t *testing.T) {
	if !audienceContains(json.RawMessage(`"p"`), "p") ||
		!audienceContains(json.RawMessage(`["x","p"]`), "p") ||
		audienceContains(json.RawMessage(`["x"]`), "p") ||
		audienceContains(json.RawMessage(`null`), "p") {
		t.Fatal("audience matching failed")
	}
	if !strings.Contains((Claims{}).ExpiresAt.String(), "0001") {
		t.Fatal("keep Claims.ExpiresAt usable")
	}
}
