package worker

import "testing"

func TestExtractContinueURLAndPageType(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"continue_url":"/email-verification",
		"page":{"type":"email_otp_verification"}
	}`)

	continueURL, pageType := extractContinueURLAndPageType(body)
	if continueURL != "/email-verification" {
		t.Fatalf("continueURL = %q, want %q", continueURL, "/email-verification")
	}
	if pageType != "email_otp_verification" {
		t.Fatalf("pageType = %q, want %q", pageType, "email_otp_verification")
	}
}

func TestOAuthNeedsEmailOTP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		continueURL string
		pageType    string
		want        bool
	}{
		{name: "page type drives otp", continueURL: "", pageType: "email_otp_verification", want: true},
		{name: "email verification url drives otp", continueURL: "/u/email-verification", pageType: "", want: true},
		{name: "email otp url drives otp", continueURL: "/u/email-otp", pageType: "", want: true},
		{name: "consent page does not require otp", continueURL: "/sign-in-with-chatgpt/codex/consent", pageType: "consent", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := oauthNeedsEmailOTP(tc.continueURL, tc.pageType); got != tc.want {
				t.Fatalf("oauthNeedsEmailOTP(%q, %q) = %v, want %v", tc.continueURL, tc.pageType, got, tc.want)
			}
		})
	}
}

func TestOAuthNeedsConsentFallback(t *testing.T) {
	t.Parallel()

	if !oauthNeedsConsentFallback("consent") {
		t.Fatal("expected consent page type to require fallback")
	}
	if !oauthNeedsConsentFallback("organization_selection") {
		t.Fatal("expected organization page type to require fallback")
	}
	if oauthNeedsConsentFallback("email_otp_verification") {
		t.Fatal("did not expect otp page type to require consent fallback")
	}
}
