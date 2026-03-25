package tempmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeYYDSMailBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty uses default", raw: "", want: defaultYYDSMailBaseURL},
		{name: "root domain unchanged", raw: "https://maliapi.215.im", want: "https://maliapi.215.im"},
		{name: "strip trailing slash", raw: "https://maliapi.215.im/", want: "https://maliapi.215.im"},
		{name: "strip v1 suffix", raw: "https://maliapi.215.im/v1", want: "https://maliapi.215.im"},
		{name: "strip v1 suffix with slash", raw: "https://maliapi.215.im/v1/", want: "https://maliapi.215.im"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeYYDSMailBaseURL(tc.raw); got != tc.want {
				t.Fatalf("normalizeYYDSMailBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestYYDSMailFetchVerificationCodeUsesLatestMessage(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"messages": []map[string]interface{}{
					{
						"id":         "old-msg",
						"subject":    "Gemini Enterprise verification code",
						"created_at": "2026-03-18T14:00:00Z",
					},
					{
						"id":         "new-msg",
						"subject":    "Gemini Enterprise verification code",
						"created_at": "2026-03-18T14:05:00Z",
					},
				},
			},
		})
	})
	mux.HandleFunc("/v1/messages/old-msg", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"text": "Your Google verification code is OLD111",
				"html": []string{},
			},
		})
	})
	mux.HandleFunc("/v1/messages/new-msg", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"text": "",
				"html": []string{
					`<div>Your one-time verification code is:</div>`,
					`<div>DZJW4P</div>`,
				},
			},
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	provider := NewYYDSMailProvider(server.URL, "test-api-key")
	code, err := provider.FetchVerificationCode(context.Background(), "gemini@example.com", map[string]string{
		"provider": "yydsmail",
		"token":    "temp-token",
	}, 1, 0)
	if err != nil {
		t.Fatalf("FetchVerificationCode() error = %v", err)
	}
	if code != "DZJW4P" {
		t.Fatalf("FetchVerificationCode() = %q, want %q", code, "DZJW4P")
	}
}

func TestYYDSMailFetchVerificationCodePreservesOriginalOrderWhenTimestampMissing(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"messages": []map[string]interface{}{
					{
						"id":      "latest-without-time",
						"subject": "Verification code",
					},
					{
						"id":         "older-with-time",
						"subject":    "Verification code",
						"created_at": "2026-03-18T14:00:00Z",
					},
				},
			},
		})
	})
	mux.HandleFunc("/v1/messages/latest-without-time", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"text": "Use this passcode: 456789",
				"html": []string{},
			},
		})
	})
	mux.HandleFunc("/v1/messages/older-with-time", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"text": "Use this passcode: 123456",
				"html": []string{},
			},
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	provider := NewYYDSMailProvider(server.URL, "test-api-key")
	code, err := provider.FetchVerificationCode(context.Background(), "latest@example.com", map[string]string{
		"provider": "yydsmail",
		"token":    "temp-token",
	}, 1, 0)
	if err != nil {
		t.Fatalf("FetchVerificationCode() error = %v", err)
	}
	if code != "456789" {
		t.Fatalf("FetchVerificationCode() = %q, want %q", code, "456789")
	}
}
