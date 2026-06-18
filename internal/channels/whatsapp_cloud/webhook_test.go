package whatsappcloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	if !verifySignature(body, sign(body, "s3cr3t"), "s3cr3t") {
		t.Error("valid signature rejected")
	}
	if verifySignature(body, sign(body, "wrong"), "s3cr3t") {
		t.Error("bad signature accepted")
	}
	if verifySignature(body, "sha1=abc", "s3cr3t") {
		t.Error("wrong prefix accepted")
	}
}

func TestGetVerifyChallenge(t *testing.T) {
	ch, _ := New(Config{AccessToken: "t", PhoneNumberID: "PN1", VerifyToken: "vtok"}, bus.New())
	registerChannel("PN1", ch)
	defer unregisterChannel("PN1")

	r := httptest.NewRequest(http.MethodGet, WebhookPath+"?hub.mode=subscribe&hub.verify_token=vtok&hub.challenge=12345", nil)
	w := httptest.NewRecorder()
	WebhookDispatcher().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "12345" {
		t.Errorf("challenge echo failed: code=%d body=%q", w.Code, w.Body.String())
	}

	// Wrong verify token → forbidden.
	r2 := httptest.NewRequest(http.MethodGet, WebhookPath+"?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=x", nil)
	w2 := httptest.NewRecorder()
	WebhookDispatcher().ServeHTTP(w2, r2)
	if w2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for bad verify token, got %d", w2.Code)
	}
}

func TestInboundRoutesAndPublishes(t *testing.T) {
	msgBus := bus.New()
	ch, err := New(Config{AccessToken: "t", PhoneNumberID: "PN42", AppSecret: "sec", VerifyToken: "v"}, msgBus)
	if err != nil {
		t.Fatal(err)
	}
	registerChannel("PN42", ch)
	defer unregisterChannel("PN42")

	body := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"PN42"},"contacts":[{"profile":{"name":"Nick"},"wa_id":"15551234567"}],"messages":[{"from":"15551234567","id":"wamid.X","type":"text","text":{"body":"hello agent"}}]}}]}]}`)

	r := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
	r.Header.Set("X-Hub-Signature-256", sign(body, "sec"))
	w := httptest.NewRecorder()
	WebhookDispatcher().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	in, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("no inbound message published")
	}
	if in.Content != "hello agent" || in.SenderID != "15551234567" {
		t.Errorf("inbound mismatch: %+v", in)
	}
}

func TestInboundRejectsBadSignature(t *testing.T) {
	msgBus := bus.New()
	ch, _ := New(Config{AccessToken: "t", PhoneNumberID: "PN7", AppSecret: "sec", VerifyToken: "v"}, msgBus)
	registerChannel("PN7", ch)
	defer unregisterChannel("PN7")

	body := []byte(`{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PN7"},"messages":[{"from":"1","type":"text","text":{"body":"hi"}}]}}]}]}`)
	r := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
	r.Header.Set("X-Hub-Signature-256", sign(body, "WRONG"))
	w := httptest.NewRecorder()
	WebhookDispatcher().ServeHTTP(w, r)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, ok := msgBus.ConsumeInbound(ctx); ok {
		t.Error("message published despite bad signature")
	}
}

func TestChunkText(t *testing.T) {
	got := chunkText(strings.Repeat("a", 5000), maxMessageLen)
	if len(got) != 2 || len(got[0]) != maxMessageLen {
		t.Errorf("chunk split wrong: %d chunks, first=%d", len(got), len(got[0]))
	}
}
