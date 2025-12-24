// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/xsrftoken"
	"tailscale.com/types/ptr"
	"tailscale.com/util/must"
)

func init() {
	// tests always need golink to be run in dev mode
	*dev = ":8080"
}

func TestServeGo(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "who", Long: "http://who/"})
	db.Save(&Link{Short: "me", Long: "/who/{{.User}}"})
	db.Save(&Link{Short: "invalid-var", Long: "/who/{{.Invalid}}"})

	tests := []struct {
		name        string
		link        string
		currentUser func(*http.Request) (user, error)
		wantStatus  int
		wantLink    string
	}{
		{
			name:       "simple link",
			link:       "/who",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			name:        "simple link, anonymous request",
			link:        "/who",
			currentUser: func(*http.Request) (user, error) { return user{}, nil },
			wantStatus:  http.StatusFound,
			wantLink:    "http://who/",
		},
		{
			name:       "simple link with path",
			link:       "/who/p",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/p",
		},
		{
			name:       "simple link with query",
			link:       "/who?q=1",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/?q=1",
		},
		{
			name:       "simple link with path and query",
			link:       "/who/p?q=1",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/p?q=1",
		},
		{
			name:       "simple link with double slash in path",
			link:       "/who/http://host",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/http://host",
		},
		{
			name:       "simple link, trailing period",
			link:       "/who.",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			name:       "simple link, trailing comma",
			link:       "/who,",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			// This seems like an incredibly unlikely typo, but test it anyway.
			name:       "simple link, trailing comma and path",
			link:       "/who,/p",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/p",
		},
		{
			name:       "simple link, trailing paren",
			link:       "/who)",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			name:       "user link",
			link:       "/me",
			wantStatus: http.StatusFound,
			wantLink:   "/who/foo@example.com",
		},
		{
			name:       "unknown link",
			link:       "/does-not-exist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown variable",
			link:       "/invalid-var",
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:        "user link, anonymous request",
			link:        "/me",
			currentUser: func(*http.Request) (user, error) { return user{}, nil },
			wantStatus:  http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.currentUser != nil {
				oldCurrentUser := currentUser
				currentUser = tt.currentUser
				t.Cleanup(func() {
					currentUser = oldCurrentUser
				})
			}

			r := httptest.NewRequest("GET", tt.link, nil)
			w := httptest.NewRecorder()
			serveHandler().ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveGo(%q) = %d; want %d", tt.link, w.Code, tt.wantStatus)
			}
			if gotLink := w.Header().Get("Location"); gotLink != tt.wantLink {
				t.Errorf("serveGo(%q) = %q; want %q", tt.link, gotLink, tt.wantLink)
			}
		})
	}
}

func TestServeSave(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC)
	db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices", Created: fixedTime})
	db.Save(&Link{Short: "who", Long: "http://who/", Owner: "foo@example.com", Created: fixedTime})

	fooXSRF := func(short string) string {
		return xsrftoken.Generate(xsrfKey, "foo@example.com", short)
	}
	barXSRF := func(short string) string {
		return xsrftoken.Generate(xsrfKey, "bar@example.com", short)
	}

	tests := []struct {
		name              string
		short             string
		xsrf              string
		long              string
		allowUnknownUsers bool
		currentUser       func(*http.Request) (user, error)
		wantStatus        int
	}{
		{
			name:       "missing short",
			short:      "",
			long:       "http://who/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing long",
			short:      "",
			long:       "http://who/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:        "save simple link",
			short:       "whoami",
			xsrf:        fooXSRF(".new"),
			long:        "http://who/",
			currentUser: func(*http.Request) (user, error) { return user{login: "foo@example.com"}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			name:        "disallow editing another's link",
			short:       "who",
			xsrf:        barXSRF("who"),
			long:        "http://who/",
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com"}, nil },
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "allow editing link owned by tagged-devices",
			short:       "link-owned-by-tagged-devices",
			xsrf:        barXSRF("link-owned-by-tagged-devices"),
			long:        "/after",
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com"}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			name:        "admins can edit any link",
			short:       "who",
			xsrf:        barXSRF("who"),
			long:        "http://who/",
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com", isAdmin: true}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			name:        "disallow unknown users",
			short:       "who2",
			xsrf:        fooXSRF("who2"),
			long:        "http://who/",
			currentUser: func(*http.Request) (user, error) { return user{}, errors.New("") },
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:              "allow unknown users",
			short:             "who2",
			long:              "http://who/",
			allowUnknownUsers: true,
			currentUser:       func(*http.Request) (user, error) { return user{}, nil },
			wantStatus:        http.StatusOK,
		},
		{
			name:       "invalid xsrf",
			short:      "goat",
			xsrf:       fooXSRF("sheep"),
			long:       "https://goat.example.com/goat.php?goat=true",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.currentUser != nil {
				oldCurrentUser := currentUser
				currentUser = tt.currentUser
				t.Cleanup(func() {
					currentUser = oldCurrentUser
				})
			}

			oldAllowUnknownUsers := *allowUnknownUsers
			*allowUnknownUsers = tt.allowUnknownUsers
			t.Cleanup(func() { *allowUnknownUsers = oldAllowUnknownUsers })

			r := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{
				"short": {tt.short},
				"long":  {tt.long},
				"xsrf":  {tt.xsrf},
			}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			serveSave(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSave(%q, %q) = %d; want %d", tt.short, tt.long, w.Code, tt.wantStatus)
			}
		})
	}
}

func TestServeDelete(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC)
	db.Save(&Link{Short: "a", Owner: "a@example.com", Created: fixedTime})
	db.Save(&Link{Short: "foo", Owner: "foo@example.com", Created: fixedTime})
	db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices", Created: fixedTime})

	xsrf := func(short string) string {
		return xsrftoken.Generate(xsrfKey, "foo@example.com", short)
	}

	tests := []struct {
		name        string
		short       string
		xsrf        string
		currentUser func(*http.Request) (user, error)
		wantStatus  int
	}{
		{
			name:       "missing short",
			short:      "",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "nonexistent link",
			short:      "does-not-exist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unowned link",
			short:      "a",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "allow deleting link owned by tagged-devices",
			short:      "link-owned-by-tagged-devices",
			xsrf:       xsrf("link-owned-by-tagged-devices"),
			wantStatus: http.StatusOK,
		},
		{
			name:        "admin can delete unowned link",
			short:       "a",
			currentUser: func(*http.Request) (user, error) { return user{login: "foo@example.com", isAdmin: true}, nil },
			xsrf:        xsrf("a"),
			wantStatus:  http.StatusOK,
		},
		{
			name:       "invalid xsrf",
			short:      "foo",
			xsrf:       xsrf("invalid"),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid xsrf",
			short:      "foo",
			xsrf:       xsrf("foo"),
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.currentUser != nil {
				oldCurrentUser := currentUser
				currentUser = tt.currentUser
				t.Cleanup(func() {
					currentUser = oldCurrentUser
				})
			}

			r := httptest.NewRequest("POST", "/.delete/"+tt.short, strings.NewReader(url.Values{
				"xsrf": {tt.xsrf},
			}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			serveDelete(w, r)
			t.Logf("response body: %v", w.Body.String())
			if w.Code != tt.wantStatus {
				t.Errorf("serveDelete(%q) = %d; want %d", tt.short, w.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				timeout := time.After(1 * time.Second)
				cleanup_signaled := false
				for {
					select {
					case <-statsCleanupChan:
						cleanup_signaled = true
					case <-timeout:
						if !cleanup_signaled {
							t.Errorf("serveDelete(%q) did not queue stats cleanup", tt.short)
						}
						return
					}
				}
			}
		})
	}
}

func TestServeExport(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC)
	db.Save(&Link{Short: "a", Owner: "a@example.com", Created: fixedTime})
	db.Save(&Link{Short: "foo", Owner: "foo@example.com", Created: fixedTime})
	db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices", Created: fixedTime})

	click := func(id string) {
		r := httptest.NewRequest("GET", "/"+id, nil)
		w := httptest.NewRecorder()
		serveHandler().ServeHTTP(w, r)
	}
	initStats()
	click("a")
	click("foo")
	click("foo")
	flushStats()
	click("a")

	// export links
	r := httptest.NewRequest("GET", "/.export", nil)
	w := httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)

	if want := http.StatusOK; w.Code != want {
		t.Errorf("serveExport = %d; want %d", w.Code, want)
	}
	wantOutput := `{"Short":"a","Long":"","Created":"2025-01-01T12:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"a@example.com"}
{"Short":"foo","Long":"","Created":"2025-01-01T12:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"foo@example.com"}
{"Short":"link-owned-by-tagged-devices","Long":"/before","Created":"2025-01-01T12:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"tagged-devices"}
`
	if got := w.Body.String(); got != wantOutput {
		t.Errorf("serveExport = %v; want %v", got, wantOutput)
	}

	// export links stats
	r = httptest.NewRequest("GET", "/.export-stats", nil)
	w = httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)

	if want := http.StatusOK; w.Code != want {
		t.Errorf("serveExportStats = %d; want %d", w.Code, want)
	}
	// Just verify stats have the right structure and counts, not exact timestamps
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 stat lines, got %d: %v", len(lines), lines)
	}
	// Verify the format of stats: short,timestamp,count
	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			t.Errorf("expected stat line format 'short,timestamp,count', got %q", line)
		}
	}
}

func TestReadOnlyMode(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "who", Long: "http://who/"})

	oldReadOnly := readonly
	readonly = ptr.To(true)
	defer func() { readonly = oldReadOnly }()

	// resolving link should succeed
	r := httptest.NewRequest("GET", "/who", nil)
	w := httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)
	if want := http.StatusFound; w.Code != want {
		t.Errorf("serveHandler() = %d; want %d", w.Code, want)
	}
	wantLocation := "http://who/"
	if location := w.Header().Get("Location"); location != wantLocation {
		t.Errorf("serveHandler() location = %v; want %v", location, wantLocation)
	}

	// updating link should fail
	r = httptest.NewRequest("POST", "/", nil)
	w = httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)
	if want := http.StatusMethodNotAllowed; w.Code != want {
		t.Errorf("serveHandler() = %d; want %d", w.Code, want)
	}

	// deleting link should fail
	r = httptest.NewRequest("POST", "/.delete/who", nil)
	w = httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)
	if want := http.StatusMethodNotAllowed; w.Code != want {
		t.Errorf("serveHandler() = %d; want %d", w.Code, want)
	}
}

func TestExpandLink(t *testing.T) {
	tests := []struct {
		name      string    // test name
		long      string    // long URL for golink
		now       time.Time // current time
		user      string    // current user resolving link
		query     string    // query string
		remainder string    // remainder of URL path after golink name
		wantErr   bool      // whether we expect an error
		want      string    // expected redirect URL
	}{
		{
			name: "dont-mangle-escapes",
			long: "http://host.com/foo%2f/bar",
			want: "http://host.com/foo%2f/bar",
		},
		{
			name:      "dont-mangle-escapes-and-remainder",
			long:      "http://host.com/foo%2f/bar",
			remainder: "extra",
			want:      "http://host.com/foo%2f/bar/extra",
		},
		{
			name:      "remainder-insert-slash",
			long:      "http://host.com/foo",
			remainder: "extra",
			want:      "http://host.com/foo/extra",
		},
		{
			name:      "remainder-long-as-trailing-slash",
			long:      "http://host.com/foo/",
			remainder: "extra",
			want:      "http://host.com/foo/extra",
		},
		{
			name: "var-expansions-time",
			long: `https://roamresearch.com/#/app/ts-corp/page/{{.Now.Format "01-02-2006"}}`,
			want: "https://roamresearch.com/#/app/ts-corp/page/06-02-2022",
			now:  time.Date(2022, 06, 02, 1, 2, 3, 4, time.UTC),
		},
		{
			name: "var-expansions-user",
			long: `http://host.com/{{.User}}`,
			user: "foo@example.com",
			want: "http://host.com/foo@example.com",
		},
		{
			name:    "var-expansions-no-user",
			long:    `http://host.com/{{.User}}`,
			wantErr: true,
		},
		{
			name:    "unknown-field",
			long:    `http://host.com/{{.Foo}}`,
			wantErr: true,
		},
		{
			name: "template-no-path",
			long: "https://calendar.google.com/{{with .Path}}calendar/embed?mode=week&src={{.}}@tailscale.com{{end}}",
			want: "https://calendar.google.com/",
		},
		{
			name:      "template-with-path",
			long:      "https://calendar.google.com/{{with .Path}}calendar/embed?mode=week&src={{.}}@tailscale.com{{end}}",
			remainder: "amelie",
			want:      "https://calendar.google.com/calendar/embed?mode=week&src=amelie@tailscale.com",
		},
		{
			name:      "template-with-pathescape-func",
			long:      "http://host.com/{{PathEscape .Path}}",
			remainder: "a/b+c",
			want:      "http://host.com/a%2Fb+c",
		},
		{
			name:      "template-with-queryescape-func",
			long:      "http://host.com/{{QueryEscape .Path}}",
			remainder: "a/b+c",
			want:      "http://host.com/a%2Fb%2Bc",
		},
		{
			name:      "template-with-trimprefix-func",
			long:      `http://host.com/{{TrimPrefix .Path "BUG-"}}`,
			remainder: "BUG-123",
			want:      "http://host.com/123",
		},
		{
			name:      "template-with-trimsuffix-func",
			long:      `http://host.com/{{TrimSuffix .Path "/"}}`,
			remainder: "a/",
			want:      "http://host.com/a",
		},
		{
			name:      "template-with-tolower-func",
			long:      `http://host.com/{{ToLower .Path}}`,
			remainder: "BUG-123",
			want:      "http://host.com/bug-123",
		},
		{
			name:      "template-with-toupper-func",
			long:      `http://host.com/{{ToUpper .Path}}`,
			remainder: "bug-123",
			want:      "http://host.com/BUG-123",
		},
		{
			name:      "template-with-match-func",
			long:      `http://host.com/{{if Match "\\d+" .Path}}id/{{.Path}}{{else}}search/{{.Path}}{{end}}`,
			remainder: "123",
			want:      "http://host.com/id/123",
		},
		{
			name:      "template-with-match-func2",
			long:      `http://host.com/{{if Match "\\d+" .Path}}id/{{.Path}}{{else}}search/{{.Path}}{{end}}`,
			remainder: "query",
			want:      "http://host.com/search/query",
		},
		{
			name:      "relative-link",
			long:      `rel`,
			remainder: "a",
			want:      "rel/a",
		},
		{
			name:      "relative-link-with-slash",
			long:      `/rel`,
			remainder: "a",
			want:      "/rel/a",
		},
		{
			name:  "query-string",
			long:  `/rel`,
			query: "a=b",
			want:  "/rel?a=b",
		},
		{
			name:      "path-and-query-string",
			long:      `/rel`,
			remainder: "path",
			query:     "a=b",
			want:      "/rel/path?a=b",
		},
		{
			name:  "combine-query-string",
			long:  `/rel?a=1`,
			query: "a=2&b=2",
			want:  "/rel?a=1&a=2&b=2",
		},
		{
			name:      "template-and-combined-query-string",
			long:      `/rel{{with .Path}}/{{.}}{{end}}?a=1`,
			remainder: "path",
			query:     "b=2",
			want:      "/rel/path?a=1&b=2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, _ := url.ParseQuery(tt.query)
			env := expandEnv{Now: tt.now, Path: tt.remainder, user: tt.user, query: query}
			link, err := expandLink(tt.long, env)
			if (err != nil) != tt.wantErr {
				t.Fatalf("expandLink(%q) returned error %v; want %v", tt.long, err, tt.wantErr)
			}
			var got string
			if link != nil {
				got = link.String()
			}
			if got != tt.want {
				t.Errorf("expandLink(%q) = %q; want %q", tt.long, got, tt.want)
			}
		})
	}
}

func TestResolveLink(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "meet", Long: "https://meet.google.com/lookup/"})
	db.Save(&Link{Short: "cs", Long: "http://codesearch/{{with .Path}}search?q={{.}}{{end}}"})
	db.Save(&Link{Short: "m", Long: "http://go/meet"})
	db.Save(&Link{Short: "chat", Long: "/meet"})

	tests := []struct {
		link string
		want string
	}{
		{
			link: "meet",
			want: "https://meet.google.com/lookup/",
		},
		{
			link: "meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			link: "go/meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			link: "http://go/meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			// if absolute URL provided, host doesn't actually matter
			link: "http://mygo/meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			link: "cs",
			want: "http://codesearch/",
		},
		{
			link: "cs/term",
			want: "http://codesearch/search?q=term",
		},
		{
			// aliased go links with hostname
			link: "m/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			// aliased go links without hostname
			link: "chat/foo",
			want: "https://meet.google.com/lookup/foo",
		},
	}
	for _, tt := range tests {
		name := "golink " + tt.link
		t.Run(name, func(t *testing.T) {
			u := must.Get(url.Parse(tt.link))
			got, err := resolveLink(u)
			if err != nil {
				t.Error(err)
			}
			if got.String() != tt.want {
				t.Errorf("ResolveLink(%q) = %q; want %q", tt.link, got.String(), tt.want)
			}
		})
	}
}

func TestNoHSTSShortDomain(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "foobar", Long: "http://foobar/"})

	tests := []struct {
		host       string
		expectHsts bool
	}{
		{
			host:       "go",
			expectHsts: false,
		},
		{
			host:       "go.prawn-universe.ts.net",
			expectHsts: true,
		},
	}
	for _, tt := range tests {
		name := "HSTS: " + tt.host
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/foobar", nil)
			r.Header.Add("Host", tt.host)

			w := httptest.NewRecorder()
			HSTS(serveHandler()).ServeHTTP(w, r)

			_, found := w.Header()["Strict-Transport-Security"]
			if found != tt.expectHsts {
				t.Errorf("HSTS expectation: domain %s want: %t got: %t", tt.host, tt.expectHsts, found)
			}
		})
	}
}

func TestHTTPSRedirectHandlerWithQuery(t *testing.T) {
	h := redirectHandler("foobar.com")
	r := httptest.NewRequest("GET", "http://example.com/?query=bar", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Errorf("got %d; want %d", w.Code, http.StatusFound)
	}
	if w.Header().Get("Location") != "https://foobar.com/?query=bar" {
		t.Errorf("got %q; want %q", w.Header().Get("Location"), "https://foobar.com/?query=bar")
	}
}

// Test immediate cleanup: deleted-retention=0 should clean immediately
func TestCleanupBehavior_Immediate(t *testing.T) {
	// Create a fresh database for this test
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// Set up for immediate cleanup (retention = 0)
	*deletedRetention = 0

	// Create and delete a link
	link := &Link{Short: "cleanup-test-1", Long: "https://example.com", Owner: "test@example.com"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Verify link exists
	loaded, err := db.Load("cleanup-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DeletedAt != nil {
		t.Error("Link should not be deleted initially")
	}

	// Delete it
	if err := db.Delete(context.Background(), "cleanup-test-1"); err != nil {
		t.Fatal(err)
	}

	// Simulate what cleanupDeletedLinksLoop does with immediate retention
	// cutoff = now - 0 = now (anything before now is deleted)
	cutoff := time.Now()
	deletedCount, err := db.CleanupDeleted(cutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Most recent deleted record is preserved for audit
	if deletedCount != 0 {
		t.Errorf("Expected 0 deletions (most recent preserved), got %d", deletedCount)
	}

	// Link should still exist as deleted (most recent is preserved)
	_, err = db.LoadDeleted("cleanup-test-1")
	if err != nil {
		t.Errorf("Expected deleted link to exist (preserved for audit): %v", err)
	}

	// Normal Load should fail
	_, err = db.Load("cleanup-test-1")
	if err == nil {
		t.Error("Expected Load() to fail for deleted link")
	}
}

// Test delayed cleanup: deleted-retention>0 keeps link recoverable for specified duration
func TestCleanupBehavior_Delayed(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// Set up for delayed cleanup (retention = 1 hour)
	*deletedRetention = 1 * time.Hour

	// Create and delete a link
	link := &Link{Short: "cleanup-test-2", Long: "https://example.com", Owner: "test@example.com"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	if err := db.Delete(context.Background(), "cleanup-test-2"); err != nil {
		t.Fatal(err)
	}

	// Simulate cleanup with 1 hour retention
	// cutoff = now - 1 hour (delete anything before 1 hour ago)
	cutoff := time.Now().Add(-1 * time.Hour)
	deletedCount, err := db.CleanupDeleted(cutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Link was just deleted (< 1 hour ago), should not be cleaned
	if deletedCount != 0 {
		t.Errorf("Expected 0 deletions (within retention window), got %d", deletedCount)
	}

	// Link should be recoverable
	deleted, err := db.LoadDeleted("cleanup-test-2")
	if err != nil {
		t.Errorf("Expected deleted link to be recoverable: %v", err)
	}
	if deleted.DeletedAt == nil {
		t.Error("Expected link to be marked as deleted")
	}

	// Normal Load should fail
	_, err = db.Load("cleanup-test-2")
	if err == nil {
		t.Error("Expected Load() to fail for deleted link")
	}
}

// Test cleanup with multiple deleted links at different times
func TestCleanupBehavior_Multiple_Deletions(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// Create multiple links
	for i := 1; i <= 3; i++ {
		link := &Link{
			Short: fmt.Sprintf("multi-cleanup-%d", i),
			Long:  fmt.Sprintf("https://example%d.com", i),
			Owner: "test@example.com",
		}
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}
	}

	// Delete all of them
	for i := 1; i <= 3; i++ {
		if err := db.Delete(context.Background(), fmt.Sprintf("multi-cleanup-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Verify all are deleted
	for i := 1; i <= 3; i++ {
		_, err := db.Load(fmt.Sprintf("multi-cleanup-%d", i))
		if err == nil {
			t.Errorf("Link %d should be deleted", i)
		}
	}

	// Cleanup with future cutoff (simulates immediate cleanup)
	futureCutoff := time.Now().Add(24 * time.Hour)
	deletedCount, err := db.CleanupDeleted(futureCutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// All most recent deleted records are preserved
	if deletedCount != 0 {
		t.Errorf("Expected 0 deletions (all most recent preserved), got %d", deletedCount)
	}

	// All links should still exist as deleted
	for i := 1; i <= 3; i++ {
		deleted, err := db.LoadDeleted(fmt.Sprintf("multi-cleanup-%d", i))
		if err != nil {
			t.Errorf("Link %d should be recoverable: %v", i, err)
		}
		if deleted.DeletedAt == nil {
			t.Errorf("Link %d should be marked as deleted", i)
		}
	}
}
