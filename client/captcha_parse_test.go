package vkclient

import (
	"net/url"
	"strings"
	"testing"
)

// The challenge VK actually answers with, taken from a phone log (2026-10-18) where the client
// kept refusing it with "missing captcha_sid in captcha error data" and every login ended as a
// plain API error. Note what is NOT in it: no captcha_sid, no captcha_img — the whole challenge
// is the redirect_uri and the session_token inside it.
func liveCaptchaErrData() map[string]interface{} {
	return map[string]interface{}{
		"error_code":         float64(14),
		"error_msg":          "Captcha need",
		"is_enabled_captcha": true,
		"variant":            "popup",
		"blank":              float64(1),
		"expired_at":         float64(1792288766),
		"redirect_uri": "https://id.vk.ru/not_robot_captcha?domain=vk.com" +
			"&session_token=eyJhbGciOiJBMjU2R0NNS1ciLCJlbmMiOiJBMjU2R0NNIiwiaXYiOiJLb3ozMmVhR2JTd25iNUFfIiwia2lkIjoiYTU0MGFlZGUtOTA3My00ZWNlLWE3MmQtYWJiY2MyMzBmYzVmIn0" +
			".ZvgKzfEnXqLtMMPAOHbFLtKhMXhJUaBJ1vqPi4sl_C4.rBFY7QB6nA0IE_Jl.hkseZxQ5zIAEgzTjtuhBTW0sUM_SW8NH7IeUx-lA1GVAQuCnl25nPOuF0cK9yzn8jxMJqBRnDjK4FPlaX4" +
			"&variant=popup&blank=1&expired_at=1792288766",
	}
}

func TestParseVkCaptchaErrorAcceptsTheSessionTokenChallenge(t *testing.T) {
	captchaErr := ParseVkCaptchaError(liveCaptchaErrData())

	if captchaErr == nil {
		t.Fatal("a challenge with a session token must be parsed; refusing it is what made every login fail")
	}
	if !captchaErr.IsCaptchaError() {
		t.Error("the parsed challenge must be recognised as one")
	}
	if captchaErr.SessionToken == "" {
		t.Error("the session token must be extracted from the redirect_uri")
	}
	if !strings.Contains(captchaErr.RedirectURI, "id.vk.ru/not_robot_captcha") {
		t.Errorf("redirect_uri: got %q", captchaErr.RedirectURI)
	}
	if captchaErr.ErrorCode != 14 {
		t.Errorf("error code: got %d", captchaErr.ErrorCode)
	}
}

// The image challenge still has to work: it is the path that needs the pair the session-token
// challenge does not carry.
func TestParseVkCaptchaErrorStillAcceptsTheImageChallenge(t *testing.T) {
	data := map[string]interface{}{
		"error_code":  float64(14),
		"error_msg":   "Captcha need",
		"captcha_sid": "239847239847",
		"captcha_img": "https://api.vk.com/captcha.php?sid=239847239847",
	}

	captchaErr := ParseVkCaptchaError(data)
	if captchaErr == nil {
		t.Fatal("the image challenge must still be parsed")
	}
	if captchaErr.CaptchaSid != "239847239847" || captchaErr.CaptchaImg == "" {
		t.Errorf("sid/img: got %q / %q", captchaErr.CaptchaSid, captchaErr.CaptchaImg)
	}
	// No session token, so this one is solved through the image page instead.
	if captchaErr.IsCaptchaError() {
		t.Error("an image-only challenge is not a session-token error")
	}
}

// Nothing to solve with: better to say so than to pretend there is a captcha.
func TestParseVkCaptchaErrorRefusesTheUnsovable(t *testing.T) {
	if got := ParseVkCaptchaError(map[string]interface{}{
		"error_code": float64(14),
		"error_msg":  "Captcha need",
	}); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}

	if got := ParseVkCaptchaError(map[string]interface{}{
		"error_msg": "Captcha need",
	}); got != nil {
		t.Errorf("without an error code there is nothing to classify, got %+v", got)
	}
}

// The token the human's answer comes back as has to survive the trip to the request.
func TestSessionTokenRoundTripsThroughTheRedirectURI(t *testing.T) {
	uri := liveCaptchaErrData()["redirect_uri"].(string)

	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("the challenge's redirect_uri must be a URL: %v", err)
	}

	if token := parsed.Query().Get("session_token"); token == "" {
		t.Fatal("no session_token in the redirect_uri")
	}
}
