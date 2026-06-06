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

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	if !validSignature("sek", body, sign("sek", body)) {
		t.Fatal("correct signature should validate")
	}
	if validSignature("sek", body, sign("wrong", body)) {
		t.Fatal("wrong-secret signature must fail")
	}
	if validSignature("sek", body, "") {
		t.Fatal("empty header must fail")
	}
	if validSignature("", body, sign("", body)) {
		t.Fatal("empty app secret must fail (not configured)")
	}
}

func TestWebhookVerifyHandshake(t *testing.T) {
	ConfigureWebhook("appsecret", "verifytok", "")
	h := WebhookDispatcher()

	// Correct token → echo challenge.
	req := httptest.NewRequest(http.MethodGet, WebhookPath+"?hub.mode=subscribe&hub.verify_token=verifytok&hub.challenge=12345", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "12345" {
		t.Fatalf("verify: code=%d body=%q, want 200/12345", rec.Code, rec.Body.String())
	}

	// Wrong token → 403.
	req = httptest.NewRequest(http.MethodGet, WebhookPath+"?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=12345", nil)
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("verify wrong token: code=%d, want 403", rec.Code)
	}
}

func TestWebhookEvent_SignatureAndRouting(t *testing.T) {
	ConfigureWebhook("appsecret", "verifytok", "")
	mb := bus.New()
	defer mb.Close()

	const pnid = "PNID_123"
	ch := New(mb, "access-token", pnid, nil) // nil allowFrom → open
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	ch.SetName("whatsapp_cloud-test")
	defer ch.Stop(context.Background())

	h := WebhookDispatcher()
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"` + pnid + `"},"messages":[{"from":"15551230000","id":"wamid.X","type":"text","text":{"body":"hello agent"}}]}}]}]}`)

	// Valid signature → 200 + message on the bus.
	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sign("appsecret", body))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid event: code=%d, want 200", rec.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	msg, ok := mb.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected an inbound message published")
	}
	if msg.Content != "hello agent" || msg.SenderID != "15551230000" || msg.ChatID != "15551230000" {
		t.Fatalf("unexpected inbound: %+v", msg)
	}

	// Bad signature → 403, nothing published.
	req = httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sign("wrongsecret", body))
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad signature: code=%d, want 403", rec.Code)
	}
}

func TestWebhookEvent_UnknownPhoneNumberIsNoOp(t *testing.T) {
	ConfigureWebhook("appsecret", "verifytok", "")
	h := WebhookDispatcher()
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"UNREGISTERED"},"messages":[{"from":"1","type":"text","text":{"body":"hi"}}]}}]}]}`)
	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sign("appsecret", body))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown phone_number_id should 200 (no-op), got %d", rec.Code)
	}
}

func TestChunkText(t *testing.T) {
	if got := chunkText("short", 4096); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short text should be one chunk: %v", got)
	}
	long := strings.Repeat("a", 5000)
	got := chunkText(long, 4096)
	if len(got) != 2 || len([]rune(got[0])) > 4096 {
		t.Fatalf("expected 2 chunks within limit, got %d (first=%d)", len(got), len([]rune(got[0])))
	}
}
