// SPDX-License-Identifier: MIT
//
// The two hooks an embedding host needs to bend the client towards its own UI. Both are
// process-wide and set before Run: the browser opener because there is exactly one way to
// show a page, and the logger because upstream's log lines are what the host shows as the
// client's log tail.

package vkclient

import "log"

// logf is the package's logger. Upstream logged with log.Printf directly; going through one
// function keeps the door open for a host-supplied sink (log.SetOutput) without touching
// every call site.
func logf(format string, args ...any) {
	log.Printf(format, args...)
}

// browserOpener is what starts a page for manual captcha solving.
var browserOpener func(streamID int, url string)

// SetBrowserOpener replaces the "open this URL in a browser" step of manual captcha
// solving. The URL always points at this client's own captcha page on localhost, so a host
// with a WebView shows it there instead of shelling out to a browser (upstream tries
// xdg-open/open/am — on Android an app process usually cannot exec those anyway). Passing
// nil restores the command-line behaviour.
//
// streamID names the client that needs the captcha, so a host running several of them knows
// which one to show; it is the same number the client logs with.
func SetBrowserOpener(opener func(streamID int, url string)) {
	browserOpener = opener
}

// captchaResolvedHook is called when a captcha page has handed its token back, so a host
// can close the page it opened.
var captchaResolvedHook func(streamID int)

// captchaAbandonedHook is called when the wait for a human ended without an answer — a timeout,
// an error, or the attempt being given up on. The page is gone by then, and a host that keeps
// showing its link opens a page nobody is listening on.
var captchaAbandonedHook func(streamID int)

// SetCaptchaResolvedHook installs the "the human is done" callback. Passing nil restores
// the command-line behaviour.
func SetCaptchaResolvedHook(hook func(streamID int)) {
	captchaResolvedHook = hook
}

// notifyCaptchaURL is the manual captcha entry point used by the captcha servers.
func notifyCaptchaURL(url string, streamID int) {
	instanceForStream(streamID).captchaWaiting(streamID, url)

	if browserOpener != nil {
		browserOpener(streamID, url)
		return
	}

	openBrowser(url)
}

// notifyCaptchaDone reports that a captcha was solved.
func notifyCaptchaDone(streamID int) {
	instanceForStream(streamID).captchaResolved()

	if captchaResolvedHook != nil {
		captchaResolvedHook(streamID)
	}
}

// notifyCaptchaAbandoned reports that nobody answered the captcha in time.
func notifyCaptchaAbandoned(streamID int) {
	instanceForStream(streamID).captchaResolved()

	if captchaAbandonedHook != nil {
		captchaAbandonedHook(streamID)
	}
}

// SetCaptchaAbandonedHook installs the "the wait is over" callback.
func SetCaptchaAbandonedHook(hook func(streamID int)) {
	captchaAbandonedHook = hook
}
