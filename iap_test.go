// Copyright 2026 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import "testing"

func TestIsIAPAdmin(t *testing.T) {
	tests := []struct {
		name   string
		flag   string
		email  string
		want   bool
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
