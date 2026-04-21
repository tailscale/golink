// Copyright 2026 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"google.golang.org/api/idtoken"
)

func TestIsIAPAdmin(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		email string
		want  bool
	}{
		{"empty flag never matches", "", "alice@example.com", false},
		{"exact match", "alice@example.com", "alice@example.com", true},
		{"case-insensitive", "Alice@Example.com", "alice@example.com", true},
		{"whitespace in list is trimmed", " bob@example.com , alice@example.com ", "alice@example.com", true},
		{"non-member", "alice@example.com,bob@example.com", "mallory@example.com", false},
		{"empty email never matches", "alice@example.com", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := *adminEmails
			*adminEmails = tt.flag
			defer func() { *adminEmails = orig }()
			if got := isIAPAdmin(tt.email); got != tt.want {
				t.Errorf("isIAPAdmin(%q) = %v, want %v", tt.email, got, tt.want)
			}
		})
	}
}

func TestIAPCurrentUser(t *testing.T) {
	const aud = "/projects/1/global/backendServices/2"
	origValidate := validateIAPAssertion
	origAud := *iapAudience
	origAdmin := *adminEmails
	origAllow := *allowUnknownUsers
	t.Cleanup(func() {
		validateIAPAssertion = origValidate
		*iapAudience = origAud
		*adminEmails = origAdmin
		*allowUnknownUsers = origAllow
	})
	*iapAudience = aud
	*adminEmails = "admin@example.com"
	*allowUnknownUsers = false

	validToken := func(email string) func(context.Context, string, string) (*idtoken.Payload, error) {
		return func(_ context.Context, _, _ string) (*idtoken.Payload, error) {
			return &idtoken.Payload{
				Issuer:   iapIssuer,
				Audience: aud,
				Claims:   map[string]any{"email": email},
			}, nil
		}
	}

	t.Run("missing header is rejected", func(t *testing.T) {
		validateIAPAssertion = validToken("anyone@example.com")
		req := httptest.NewRequest("GET", "/", nil)
		if _, err := iapCurrentUser(req); err == nil {
			t.Fatal("want error for missing header, got nil")
		}
	})

	t.Run("invalid JWT is rejected", func(t *testing.T) {
		validateIAPAssertion = func(_ context.Context, _, _ string) (*idtoken.Payload, error) {
			return nil, errors.New("bad signature")
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(iapHeaderName, "opaque")
		if _, err := iapCurrentUser(req); err == nil {
			t.Fatal("want error for invalid JWT, got nil")
		}
	})

	t.Run("wrong issuer is rejected", func(t *testing.T) {
		validateIAPAssertion = func(_ context.Context, _, _ string) (*idtoken.Payload, error) {
			return &idtoken.Payload{
				Issuer: "https://accounts.google.com",
				Claims: map[string]any{"email": "alice@example.com"},
			}, nil
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(iapHeaderName, "opaque")
		if _, err := iapCurrentUser(req); err == nil {
			t.Fatal("want error for wrong issuer, got nil")
		}
	})

	t.Run("valid admin JWT authenticates as admin", func(t *testing.T) {
		validateIAPAssertion = validToken("admin@example.com")
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(iapHeaderName, "opaque")
		u, err := iapCurrentUser(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.login != "admin@example.com" || !u.isAdmin {
			t.Errorf("got %+v, want login=admin@example.com isAdmin=true", u)
		}
	})

	t.Run("valid non-admin JWT authenticates without admin", func(t *testing.T) {
		validateIAPAssertion = validToken("alice@example.com")
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(iapHeaderName, "opaque")
		u, err := iapCurrentUser(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.login != "alice@example.com" || u.isAdmin {
			t.Errorf("got %+v, want login=alice@example.com isAdmin=false", u)
		}
	})

	t.Run("allow-unknown-users swallows verification failures", func(t *testing.T) {
		*allowUnknownUsers = true
		t.Cleanup(func() { *allowUnknownUsers = false })
		validateIAPAssertion = func(_ context.Context, _, _ string) (*idtoken.Payload, error) {
			return nil, errors.New("bad signature")
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(iapHeaderName, "opaque")
		u, err := iapCurrentUser(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.login != "" {
			t.Errorf("want empty user, got %+v", u)
		}
	})
}
