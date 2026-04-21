// Copyright 2026 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

// This file implements identity resolution via Google Cloud Identity-Aware
// Proxy (IAP). When the -iap-audience flag is set, golink trusts the signed
// IAP JWT assertion on each request (header X-Goog-IAP-JWT-Assertion) as the
// source of truth for the calling user's email, instead of calling out to the
// Tailscale local client. This is intended for deployments that sit behind an
// HTTPS load balancer with IAP enabled (e.g. go.example.com gated by Google
// Workspace SSO) rather than on a tailnet.

package golink

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/idtoken"
)

var (
	iapAudience = flag.String("iap-audience", "",
		"If set, trust Google IAP's X-Goog-IAP-JWT-Assertion header for identity "+
			"instead of Tailscale WhoIs. The value is the expected JWT audience, "+
			"e.g. /projects/PROJECT_NUMBER/global/backendServices/BACKEND_SERVICE_ID "+
			"(GCLB) or /projects/PROJECT_NUMBER/apps/APP_ID (App Engine).")
	adminEmails = flag.String("admin-emails", "",
		"Comma-separated emails granted admin rights when using -iap-audience.")
	workspaceDomain = flag.String("workspace-domain", "",
		"Google Workspace domain (e.g. example.com). When using -iap-audience, "+
			"any email in this domain is considered a valid user for ownership "+
			"transfers. If empty, all emails are accepted.")
)

const (
	iapIssuer     = "https://cloud.google.com/iap"
	iapHeaderName = "X-Goog-IAP-JWT-Assertion"
)

// iapEnabled reports whether IAP-based authentication is configured.
func iapEnabled() bool { return *iapAudience != "" }

// validateIAPAssertion is overridable in tests. It verifies the JWT signature,
// expiry, and audience, returning the payload on success.
var validateIAPAssertion = func(ctx context.Context, assertion, audience string) (*idtoken.Payload, error) {
	return idtoken.Validate(ctx, assertion, audience)
}

// iapCurrentUser verifies the IAP JWT on the request and returns the
// authenticated user. Admin status is determined by -admin-emails.
func iapCurrentUser(r *http.Request) (user, error) {
	assertion := r.Header.Get(iapHeaderName)
	if assertion == "" {
		if *allowUnknownUsers {
			return user{}, nil
		}
		return user{}, errors.New("missing IAP assertion header")
	}
	payload, err := validateIAPAssertion(r.Context(), assertion, *iapAudience)
	if err != nil {
		if *allowUnknownUsers {
			return user{}, nil
		}
		return user{}, fmt.Errorf("verify iap assertion: %w", err)
	}
	// idtoken.Validate accepts both accounts.google.com and cloud.google.com/iap
	// issuers, so explicitly pin to IAP to avoid accepting unrelated Google ID
	// tokens that happen to share our audience string.
	if payload.Issuer != iapIssuer {
		if *allowUnknownUsers {
			return user{}, nil
		}
		return user{}, fmt.Errorf("verify iap assertion: unexpected issuer %q", payload.Issuer)
	}
	email, _ := payload.Claims["email"].(string)
	if email == "" {
		if *allowUnknownUsers {
			return user{}, nil
		}
		return user{}, errors.New("iap assertion missing email claim")
	}
	return user{login: email, isAdmin: isIAPAdmin(email)}, nil
}

func isIAPAdmin(email string) bool {
	if *adminEmails == "" || email == "" {
		return false
	}
	for _, a := range strings.Split(*adminEmails, ",") {
		if strings.EqualFold(strings.TrimSpace(a), email) {
			return true
		}
	}
	return false
}
