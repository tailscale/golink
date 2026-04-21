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
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	iapIssuer      = "https://cloud.google.com/iap"
	iapJWKSURL     = "https://www.gstatic.com/iap/verify/public_key-jwk"
	iapHeaderName  = "X-Goog-IAP-JWT-Assertion"
	iapJWKSRefresh = 1 * time.Hour
)

// iapEnabled reports whether IAP-based authentication is configured.
func iapEnabled() bool { return *iapAudience != "" }

type iapJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type iapJWKSet struct {
	Keys []iapJWK `json:"keys"`
}

// iapKeyCache caches IAP's ES256 public keys by kid. Google rotates these
// periodically so we refetch when the cache is older than iapJWKSRefresh, or
// immediately on a cache miss (which covers key rotation between refreshes).
type iapKeyCache struct {
	mu        sync.Mutex
	keys      map[string]*ecdsa.PublicKey
	fetchedAt time.Time
}

var iapKeys = &iapKeyCache{keys: map[string]*ecdsa.PublicKey{}}

func (c *iapKeyCache) getKey(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if k, ok := c.keys[kid]; ok && time.Since(c.fetchedAt) < iapJWKSRefresh {
		return k, nil
	}
	if err := c.refreshLocked(ctx); err != nil {
		return nil, err
	}
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("iap key %q not found", kid)
}

func (c *iapKeyCache) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iapJWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch iap jwks: status %d", resp.StatusCode)
	}
	var set iapJWKSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return err
	}
	keys := make(map[string]*ecdsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		pk, err := jwkToECDSA(k)
		if err != nil {
			continue
		}
		keys[k.Kid] = pk
	}
	c.keys = keys
	c.fetchedAt = time.Now()
	return nil
}

func jwkToECDSA(k iapJWK) (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" || k.Crv != "P-256" {
		return nil, fmt.Errorf("unsupported jwk: kty=%q crv=%q", k.Kty, k.Crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("decode x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("decode y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
}

type iapClaims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

// verifyIAPAssertion validates a Google IAP JWT assertion and returns the
// asserted email. It checks the signature against Google's published IAP
// keys, that the issuer is cloud.google.com/iap, and that the audience
// matches the expected value.
func verifyIAPAssertion(ctx context.Context, assertion, audience string) (string, error) {
	tok, err := jwt.ParseWithClaims(assertion, &iapClaims{}, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return iapKeys.getKey(ctx, kid)
	},
		jwt.WithIssuer(iapIssuer),
		jwt.WithAudience(audience),
		jwt.WithValidMethods([]string{"ES256"}),
	)
	if err != nil {
		return "", err
	}
	c, ok := tok.Claims.(*iapClaims)
	if !ok || c.Email == "" {
		return "", errors.New("iap assertion missing email claim")
	}
	return c.Email, nil
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
	email, err := verifyIAPAssertion(r.Context(), assertion, *iapAudience)
	if err != nil {
		if *allowUnknownUsers {
			return user{}, nil
		}
		return user{}, fmt.Errorf("verify iap assertion: %w", err)
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
