package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeriveWebhookSecret_Deterministic(t *testing.T) {
	ConfigureWebhook("https://example.test", "signing-key-1")
	id := "019e8e7f-1738-7259-ab42-a9cba986f8db"

	a := deriveWebhookSecret(id)
	b := deriveWebhookSecret(id)
	if a == "" || a != b {
		t.Fatalf("secret should be stable and non-empty: %q vs %q", a, b)
	}
	if got := deriveWebhookSecret("different-id"); got == a {
		t.Fatal("different instance ids must yield different secrets")
	}
	// Telegram allows only [A-Za-z0-9_-]; hex output qualifies.
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("secret has non-hex char %q", r)
		}
	}

	ConfigureWebhook("https://example.test", "signing-key-2")
	if deriveWebhookSecret(id) == a {
		t.Fatal("rotating the signing key must change the derived secret")
	}
}

func TestWebhookDispatcher_AuthAndRouting(t *testing.T) {
	ConfigureWebhook("https://example.test", "k")
	const id = "11111111-1111-1111-1111-111111111111"
	secret := deriveWebhookSecret(id)

	// Minimal channel: a non-message update exercises decode+auth+routing
	// without needing the full handler/bot stack.
	c := &Channel{webhookSecret: secret, webhookCtx: context.Background()}
	registerWebhookChannel(id, c)
	defer unregisterWebhookChannel(id)

	h := WebhookDispatcher()
	body := `{"update_id":1}`

	cases := []struct {
		name   string
		method string
		path   string
		secret string
		want   int
	}{
		{"valid", http.MethodPost, WebhookPathPrefix + id, secret, http.StatusOK},
		{"bad secret", http.MethodPost, WebhookPathPrefix + id, "wrong", http.StatusForbidden},
		{"missing secret", http.MethodPost, WebhookPathPrefix + id, "", http.StatusForbidden},
		{"unknown instance", http.MethodPost, WebhookPathPrefix + "22222222-2222-2222-2222-222222222222", secret, http.StatusOK},
		{"wrong method", http.MethodGet, WebhookPathPrefix + id, secret, http.StatusMethodNotAllowed},
		{"no instance in path", http.MethodPost, WebhookPathPrefix, secret, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(body))
			if tc.secret != "" {
				req.Header.Set("X-Telegram-Bot-Api-Secret-Token", tc.secret)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
