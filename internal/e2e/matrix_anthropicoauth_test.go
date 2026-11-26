package e2e

import (
	"context"
	"strings"
	"testing"

	openaiclient "github.com/openai/openai-go/v3"

	"github.com/tubeof/proxyllm/internal/provider/anthropicoauth"
)

// TestMatrix_Upstream_AnthropicOAuth drives the Anthropic OAuth provider
// (ADR-022) end to end: the seeded access token is expired, so the first
// request rotates it against MockServer's token endpoint and the Messages
// request rides with the minted Bearer plus the oauth beta header — never
// x-api-key and never the client's own credential. The second request reuses
// the minted token without another rotation.
func TestMatrix_Upstream_AnthropicOAuth(t *testing.T) {
	ctx := context.Background()
	up := upstream{ID: "anthropic-oauth-up", Type: "anthropic-oauth"}
	e := newMatrixEnv(t, up)

	const (
		mintedAccess   = "anthropic-minted-access"
		rotatedRefresh = "anthropic-rotated-refresh"
	)
	e.ms.Upsert(t, map[string]any{
		"httpRequest": map[string]any{"method": "POST", "path": "/oauth/token"},
		"httpResponse": map[string]any{"statusCode": 200, "body": `{"access_token":"` + mintedAccess + `",` +
			`"refresh_token":"` + rotatedRefresh + `","expires_in":28800}`},
	})
	e.ms.Upsert(t, expectation("ANTHROPIC", up.ID, "/v1/messages",
		chatCompletion("anthropic oauth answer", false)))

	ask := func() string {
		resp, err := e.openai.Chat.Completions.New(ctx, openaiclient.ChatCompletionNewParams{
			Model:    up.ID,
			Messages: []openaiclient.ChatCompletionMessageParamUnion{openaiclient.UserMessage("hi")},
		})
		if err != nil {
			t.Fatalf("chat completion: %v", err)
		}
		if len(resp.Choices) == 0 {
			t.Fatal("client answer carries no choices")
		}
		return resp.Choices[0].Message.Content
	}
	for i := 1; i <= 2; i++ {
		if got := ask(); !strings.Contains(got, "anthropic oauth answer") {
			t.Fatalf("client answer %d = %q, want the upstream text", i, got)
		}
	}

	var tokenReqs, messageReqs int
	for _, r := range e.ms.Requests(t) {
		switch r.Path {
		case "/oauth/token":
			tokenReqs++
			body := upstreamJSON(t, r)
			if body["grant_type"] != "refresh_token" {
				t.Errorf("refresh grant_type = %v", body["grant_type"])
			}
			if body["refresh_token"] != anthropicOAuthSeedRefresh {
				t.Errorf("refresh_token = %v, want the seeded one", body["refresh_token"])
			}
			if body["client_id"] != anthropicoauth.OAuthClientID {
				t.Errorf("client_id = %v, want the pinned Anthropic OAuth client id", body["client_id"])
			}
		case "/v1/messages":
			messageReqs++
			if auth := recordedHeader(r, "Authorization"); len(auth) == 0 || auth[0] != "Bearer "+mintedAccess {
				t.Errorf("messages Authorization = %v, want the rotated bearer", auth)
			}
			if key := recordedHeader(r, "X-Api-Key"); len(key) != 0 {
				t.Errorf("messages carried x-api-key = %v; the OAuth provider must not send it", key)
			}
			beta := strings.Join(recordedHeader(r, "Anthropic-Beta"), ",")
			if !strings.Contains(beta, anthropicoauth.BetaHeader) {
				t.Errorf("anthropic-beta = %q, want %s", beta, anthropicoauth.BetaHeader)
			}
			if body := upstreamJSON(t, r); body["model"] != up.ID {
				t.Errorf("messages model = %v, want %q", body["model"], up.ID)
			}
		}
	}
	if tokenReqs != 1 {
		t.Fatalf("issuer /oauth/token requests = %d, want 1 (rotate, then reuse)", tokenReqs)
	}
	if messageReqs != 2 {
		t.Fatalf("Messages requests = %d, want 2", messageReqs)
	}

	// The rotation persisted the minted pair, not the seed.
	access, err := e.store.GetProviderToken(ctx, up.ID, anthropicoauth.KindAccessToken)
	if err != nil || string(access.Secret) != mintedAccess {
		t.Fatalf("stored access = %v/%v, want the minted token", access, err)
	}
	refresh, err := e.store.GetProviderToken(ctx, up.ID, anthropicoauth.KindRefreshToken)
	if err != nil || string(refresh.Secret) != rotatedRefresh {
		t.Fatalf("stored refresh = %v/%v, want the rotated token", refresh, err)
	}
}
