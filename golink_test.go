// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/net/xsrftoken"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest"
	"tailscale.com/util/must"
)

// newTestServer returns a golink Server backed by an in-memory database,
// running in dev mode so that requests are authenticated as a fake user
// without a LocalAPI client.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{DB: db, Dev: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with nil database succeeded")
	}
}

func TestServeGo(t *testing.T) {
	s := newTestServer(t)
	s.db.Save(&Link{Short: "who", Long: "http://who/"})
	s.db.Save(&Link{Short: "me", Long: "/who/{{.User}}"})
	s.db.Save(&Link{Short: "invalid-var", Long: "/who/{{.Invalid}}"})

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
				oldCurrentUser := s.currentUser
				s.currentUser = tt.currentUser
				t.Cleanup(func() {
					s.currentUser = oldCurrentUser
				})
			}

			r := httptest.NewRequest("GET", tt.link, nil)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveGo(%q) = %d; want %d", tt.link, w.Code, tt.wantStatus)
			}
			if gotLink := w.Header().Get("Location"); gotLink != tt.wantLink {
				t.Errorf("serveGo(%q) = %q; want %q", tt.link, gotLink, tt.wantLink)
			}
		})
	}
}

func TestReferrerPolicy(t *testing.T) {
	s := newTestServer(t)
	s.db.Save(&Link{Short: "who", Long: "http://who/"})

	// all responses should ask the browser to suppress the Referer header,
	// so that link destinations never learn the golink host.
	for _, path := range []string{"/", "/who", "/.detail/who"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)

		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("GET %q: Referrer-Policy = %q; want %q", path, got, "no-referrer")
		}
	}
}

func TestServeSave(t *testing.T) {
	s := newTestServer(t)
	s.db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices"})

	fooXSRF := func(short string) string {
		return xsrftoken.Generate(s.xsrfKey, "foo@example.com", short)
	}
	barXSRF := func(short string) string {
		return xsrftoken.Generate(s.xsrfKey, "bar@example.com", short)
	}

	tests := []struct {
		name              string
		short             string
		xsrf              string
		long              string
		allowUnknownUsers bool
		currentUser       func(*http.Request) (user, error)
		wantStatus        int
		wantLocation      string
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
			name:       "save simple link",
			short:      "who",
			xsrf:       fooXSRF(newShortName),
			long:       "http://who/",
			wantStatus: http.StatusOK,
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
			name:         "redirect to detail page when creating link that already exists",
			short:        "who",
			xsrf:         barXSRF(newShortName),
			long:         "http://who/updated",
			currentUser:  func(*http.Request) (user, error) { return user{login: "bar@example.com", isAdmin: true}, nil },
			wantStatus:   http.StatusSeeOther,
			wantLocation: "/.detail/who?exists=1",
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
				oldCurrentUser := s.currentUser
				s.currentUser = tt.currentUser
				t.Cleanup(func() {
					s.currentUser = oldCurrentUser
				})
			}

			oldAllowUnknownUsers := s.allowUnknownUsers
			s.allowUnknownUsers = tt.allowUnknownUsers
			t.Cleanup(func() { s.allowUnknownUsers = oldAllowUnknownUsers })

			r := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{
				"short": {tt.short},
				"long":  {tt.long},
				"xsrf":  {tt.xsrf},
			}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			s.serveSave(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSave(%q, %q) = %d; want %d", tt.short, tt.long, w.Code, tt.wantStatus)
			}
			if tt.wantLocation != "" {
				if got := w.Header().Get("Location"); got != tt.wantLocation {
					t.Errorf("serveSave(%q, %q) Location = %q; want %q", tt.short, tt.long, got, tt.wantLocation)
				}
			}
		})
	}
}

func TestServeDelete(t *testing.T) {
	s := newTestServer(t)
	s.db.Save(&Link{Short: "a", Owner: "a@example.com"})
	s.db.Save(&Link{Short: "foo", Owner: "foo@example.com"})
	s.db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices"})

	xsrf := func(short string) string {
		return xsrftoken.Generate(s.xsrfKey, "foo@example.com", short)
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
				oldCurrentUser := s.currentUser
				s.currentUser = tt.currentUser
				t.Cleanup(func() {
					s.currentUser = oldCurrentUser
				})
			}

			r := httptest.NewRequest("POST", "/.delete/"+tt.short, strings.NewReader(url.Values{
				"xsrf": {tt.xsrf},
			}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			s.serveDelete(w, r)
			t.Logf("response body: %v", w.Body.String())
			if w.Code != tt.wantStatus {
				t.Errorf("serveDelete(%q) = %d; want %d", tt.short, w.Code, tt.wantStatus)
			}
		})
	}
}

func TestServeExport(t *testing.T) {
	clock := tstest.NewClock(tstest.ClockOpts{
		Start: time.Date(2022, 06, 02, 1, 2, 3, 4, time.UTC),
	})

	s := newTestServer(t)
	s.db.clock = clock
	s.db.Save(&Link{Short: "a", Owner: "a@example.com"})
	s.db.Save(&Link{Short: "foo", Owner: "foo@example.com"})
	s.db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices"})

	click := func(id string) {
		r := httptest.NewRequest("GET", "/"+id, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
	}
	s.initStats()
	click("a")
	click("foo")
	click("foo")
	s.FlushStats()
	clock.Advance(3 * time.Minute)
	click("a")

	// export links
	r := httptest.NewRequest("GET", "/.export", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if want := http.StatusOK; w.Code != want {
		t.Errorf("serveExport = %d; want %d", w.Code, want)
	}
	wantOutput := `{"Short":"a","Long":"","Created":"0001-01-01T00:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"a@example.com"}
{"Short":"foo","Long":"","Created":"0001-01-01T00:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"foo@example.com"}
{"Short":"link-owned-by-tagged-devices","Long":"/before","Created":"0001-01-01T00:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"tagged-devices"}
`
	if got := w.Body.String(); got != wantOutput {
		t.Errorf("serveExport = %v; want %v", got, wantOutput)
	}

	// export links stats
	r = httptest.NewRequest("GET", "/.export-stats", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if want := http.StatusOK; w.Code != want {
		t.Errorf("serveExportStats = %d; want %d", w.Code, want)
	}
	wantOutput = `a,1654131723,1
foo,1654131723,2
a,1654131903,1
`
	if got := w.Body.String(); got != wantOutput {
		t.Errorf("serveExportStats = %v; want %v", got, wantOutput)
	}
}

func TestReadOnlyMode(t *testing.T) {
	s := newTestServer(t)
	s.db.Save(&Link{Short: "who", Long: "http://who/"})

	s.readonly = true

	// resolving link should succeed
	r := httptest.NewRequest("GET", "/who", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if want := http.StatusFound; w.Code != want {
		t.Errorf("Handler() = %d; want %d", w.Code, want)
	}
	wantLocation := "http://who/"
	if location := w.Header().Get("Location"); location != wantLocation {
		t.Errorf("Handler() location = %v; want %v", location, wantLocation)
	}

	// updating link should fail
	r = httptest.NewRequest("POST", "/", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if want := http.StatusMethodNotAllowed; w.Code != want {
		t.Errorf("Handler() = %d; want %d", w.Code, want)
	}

	// deleting link should fail
	r = httptest.NewRequest("POST", "/.delete/who", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if want := http.StatusMethodNotAllowed; w.Code != want {
		t.Errorf("Handler() = %d; want %d", w.Code, want)
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
	s := newTestServer(t)
	s.db.Save(&Link{Short: "meet", Long: "https://meet.google.com/lookup/"})
	s.db.Save(&Link{Short: "cs", Long: "http://codesearch/{{with .Path}}search?q={{.}}{{end}}"})
	s.db.Save(&Link{Short: "m", Long: "http://go/meet"})
	s.db.Save(&Link{Short: "chat", Long: "/meet"})

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
			got, err := s.resolveLink(u)
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
	s := newTestServer(t)
	s.db.Save(&Link{Short: "foobar", Long: "http://foobar/"})

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
			// Set r.Host, as net/http does for incoming requests; it does
			// not keep Host in r.Header.
			r.Host = tt.host

			w := httptest.NewRecorder()
			HSTS(s.Handler()).ServeHTTP(w, r)

			_, found := w.Header()["Strict-Transport-Security"]
			if found != tt.expectHsts {
				t.Errorf("HSTS expectation: domain %s want: %t got: %t", tt.host, tt.expectHsts, found)
			}
		})
	}
}

func TestHTTPSRedirectHandlerWithQuery(t *testing.T) {
	h := RedirectHandler("foobar.com")
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

func TestServeSearch(t *testing.T) {
	s := newTestServer(t)
	links := []*Link{
		{Short: "alpha", Long: "http://alpha/", Owner: "foo@example.com"},
		{Short: "beta", Long: "http://beta/", Owner: "foo@example.com"},
		{Short: "gamma", Long: "http://gamma/", Owner: "bar@example.com"},
		{Short: "delta", Long: "http://delta/", Owner: "FOO@example.com"},
	}
	for _, link := range links {
		if err := s.db.Save(link); err != nil {
			t.Error(err)
		}
	}

	tests := []struct {
		name            string
		owner           string
		wantStatus      int
		wantContains    []string // substrings that should appear in response body
		wantNotContains []string // substrings that should NOT appear in response body
	}{
		{
			name:            "search by owner with multiple links",
			owner:           "foo@example.com",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"alpha", "beta", "delta", "3 total"},
			wantNotContains: []string{"gamma"},
		},
		{
			name:         "search by owner case insensitive",
			owner:        "FOO@EXAMPLE.COM",
			wantStatus:   http.StatusOK,
			wantContains: []string{"alpha", "beta", "delta"},
		},
		{
			name:            "search by owner with single link",
			owner:           "bar@example.com",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"gamma", "1 total"},
			wantNotContains: []string{"alpha", "beta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testURL := "/.search?q=owner:" + url.QueryEscape(tt.owner)
			r := httptest.NewRequest("GET", testURL, nil)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSearch(owner=%q) = %d; want %d", tt.owner, w.Code, tt.wantStatus)
			}

			body := w.Body.String()
			for _, s := range tt.wantContains {
				if !strings.Contains(body, s) {
					t.Errorf("serveSearch(owner=%q) body missing %q", tt.owner, s)
				}
			}
			for _, s := range tt.wantNotContains {
				if strings.Contains(body, s) {
					t.Errorf("serveSearch(owner=%q) body unexpectedly contains %q", tt.owner, s)
				}
			}
		})
	}
}

func TestSearchResults(t *testing.T) {
	s := newTestServer(t)
	s.stats.mu.Lock()
	s.stats.clicks = ClickStats{"alpha": 3, "beta": 10}
	s.stats.mu.Unlock()

	links := []*Link{
		{Short: "alpha"},
		{Short: "beta"},
		{Short: "gamma"}, // no recorded clicks; should annotate to 0
	}

	// Expect the historical alphabetical ordering by short name, with each
	// link annotated with its current click count.
	want := []struct {
		Short     string
		NumClicks int
	}{
		{Short: "alpha", NumClicks: 3},
		{Short: "beta", NumClicks: 10},
		{Short: "gamma", NumClicks: 0},
	}

	got := s.searchResults(links)
	if len(got) != len(want) {
		t.Fatalf("searchResults returned %d results; want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Short != w.Short || got[i].NumClicks != w.NumClicks {
			t.Errorf("result[%d] = {%q, %d}; want {%q, %d}", i, got[i].Short, got[i].NumClicks, w.Short, w.NumClicks)
		}
	}
}

func TestParseAdvertiseTags(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "empty string",
			input: "",
			want:  nil,
		},
		{
			name:  "single tag",
			input: "tag:golink",
			want:  []string{"tag:golink"},
		},
		{
			name:  "multiple tags",
			input: "tag:golink,tag:server",
			want:  []string{"tag:golink", "tag:server"},
		},
		{
			name:  "whitespace trimmed",
			input: " tag:golink , tag:server ",
			want:  []string{"tag:golink", "tag:server"},
		},
		{
			name:  "trailing comma ignored",
			input: "tag:golink,",
			want:  []string{"tag:golink"},
		},
		{
			name:    "missing tag prefix",
			input:   "golink",
			wantErr: true,
		},
		{
			name:    "one valid one invalid",
			input:   "tag:golink,invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAdvertiseTags(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseAdvertiseTags(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("parseAdvertiseTags(%q) diff (-want +got):\n%s", tt.input, cmp.Diff(tt.want, got))
			}
		})
	}
}

func TestTrustIdentityHeaders(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string // value for the server's ServiceName
		remoteAddr  string
		want        bool
	}{
		{
			name:        "no service node, loopback",
			serviceName: "",
			remoteAddr:  "127.0.0.1:1234",
			want:        false,
		},
		{
			name:        "service mode, loopback",
			serviceName: "svc:golink",
			remoteAddr:  "127.0.0.1:1234",
			want:        true,
		},
		{
			name:        "service mode, non-loopback",
			serviceName: "svc:golink",
			remoteAddr:  "100.64.1.1:1234",
			want:        false,
		},
		{
			name:        "service mode, IPV6 loopback",
			serviceName: "svc:golink",
			remoteAddr:  "[::1]:1234",
			want:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			s.serviceName = tt.serviceName
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if got := s.trustIdentityHeaders(r); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractUserFromHeaders(t *testing.T) {
	adminCapMap := tailcfg.PeerCapMap{
		peerCapName: []tailcfg.RawMessage{
			tailcfg.RawMessage(must.Get(json.Marshal(capabilities{Admin: true}))),
		},
	}
	noCapMap := tailcfg.PeerCapMap{}

	tests := []struct {
		name      string
		headers   map[string]string
		whoisFunc func(context.Context, string) (*apitype.WhoIsResponse, error) // mock for LocalClient WhoIs
		wantLogin string
		wantAdmin bool
	}{
		{
			name:      "no headers",
			headers:   nil,
			wantLogin: "",
		},
		{
			name:      "login header only, no XFF",
			headers:   map[string]string{"Tailscale-User-Login": "alice@example.com"},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
		{
			name: "login header with XFF, peer has admin cap",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "100.64.1.1",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return &apitype.WhoIsResponse{CapMap: adminCapMap}, nil
			},
			wantLogin: "alice@example.com",
			wantAdmin: true,
		},
		{
			name: "login header with XFF, peer has no admin cap",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "100.64.1.1",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return &apitype.WhoIsResponse{CapMap: noCapMap}, nil
			},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
		{
			name: "login header with XFF, WhoIs fails",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "100.64.1.1",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return nil, errors.New("peer not found")
			},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
		{
			name: "Invalid X-Forwarded-For header",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "invalid-ip",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return &apitype.WhoIsResponse{CapMap: adminCapMap}, nil
			},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			if tt.whoisFunc != nil {
				s.whoisFunc = tt.whoisFunc
			}

			r := httptest.NewRequest("GET", "/", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			got := s.extractUserFromHeaders(r)
			if got.login != tt.wantLogin {
				t.Errorf("login: got %q, want %q", got.login, tt.wantLogin)
			}
			if got.isAdmin != tt.wantAdmin {
				t.Errorf("isAdmin: got %v, want %v", got.isAdmin, tt.wantAdmin)
			}
		})
	}
}

// TestServeHTTPDrainsInFlightOnCancel checks that canceling ctx lets
// in-flight requests finish before serveHTTP returns.
func TestServeHTTPDrainsInFlightOnCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serveHTTP(ctx, listenerServer{srv: srv, ln: ln}) }()

	type result struct {
		resp *http.Response
		err  error
	}
	respCh := make(chan result, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://%s/", ln.Addr()))
		respCh <- result{resp, err}
	}()

	<-entered
	cancel()
	select {
	case err := <-served:
		t.Fatalf("serveHTTP returned %v while a request was in flight", err)
	default:
	}
	close(release)

	select {
	case res := <-respCh:
		if res.err != nil {
			t.Fatalf("in-flight request failed: %v", res.err)
		}
		res.resp.Body.Close()
		if res.resp.StatusCode != http.StatusOK {
			t.Errorf("in-flight request status = %d; want %d", res.resp.StatusCode, http.StatusOK)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not finish")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveHTTP returned %v; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not return after ctx was canceled")
	}
}

func TestServeHTTPReturnsServeError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	served := make(chan error, 1)
	go func() {
		served <- serveHTTP(context.Background(), listenerServer{srv: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}, ln: ln})
	}()

	ln.Close()

	select {
	case err := <-served:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serveHTTP returned %v; want a serve error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not return after the listener closed")
	}
}
