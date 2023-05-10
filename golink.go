// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

// The golink server runs http://go/, a private shortlink service for tailnets.
package golink

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"

	"golang.org/x/net/xsrftoken"
	"tailscale.com/client/tailscale"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

const defaultHostname = "go"

var (
	verbose           = flag.Bool("verbose", false, "be verbose")
	controlURL        = flag.String("control-url", ipn.DefaultControlURL, "the URL base of the control plane (i.e. coordination server)")
	sqlitefile        = flag.String("sqlitedb", "", "path of SQLite database to store links")
	dev               = flag.String("dev-listen", "", "if non-empty, listen on this addr and run in dev mode; auto-set sqlitedb if empty and don't use tsnet")
	snapshot          = flag.String("snapshot", "", "file path of snapshot file")
	hostname          = flag.String("hostname", defaultHostname, "service name")
	resolveFromBackup = flag.String("resolve-from-backup", "", "resolve a link from snapshot file and exit")
	allowUnknownUsers = flag.Bool("allow-unknown-users", false, "allow unknown users to save links")
)

var stats struct {
	mu     sync.Mutex
	clicks ClickStats // short link -> number of times visited

	// dirty identifies short link clicks that have not yet been stored.
	dirty ClickStats
}

// LastSnapshot is the data snapshot (as returned by the /.export handler)
// that will be loaded on startup.
var LastSnapshot []byte

//go:embed static tmpl/*.html tmpl/*.xml
var embeddedFS embed.FS

// db stores short links.
var db *SQLiteDB

var localClient *tailscale.LocalClient

func Run() error {
	flag.Parse()

	hostinfo.SetApp("golink")

	// if resolving from backup, set sqlitefile and snapshot flags to
	// restore links into an in-memory sqlite database.
	if *resolveFromBackup != "" {
		*sqlitefile = ":memory:"
		snapshot = resolveFromBackup
		if flag.NArg() != 1 {
			log.Fatal("--resolve-from-backup also requires a link to be resolved")
		}
	}

	if *sqlitefile == "" {
		if devMode() {
			tmpdir, err := ioutil.TempDir("", "golink_dev_*")
			if err != nil {
				return err
			}
			*sqlitefile = filepath.Join(tmpdir, "golink.db")
			log.Printf("Dev mode temp db: %s", *sqlitefile)
		} else {
			return errors.New("--sqlitedb is required")
		}
	}

	var err error
	if db, err = NewSQLiteDB(*sqlitefile); err != nil {
		return fmt.Errorf("NewSQLiteDB(%q): %w", *sqlitefile, err)
	}

	if *snapshot != "" {
		if LastSnapshot != nil {
			log.Printf("LastSnapshot already set; ignoring --snapshot")
		} else {
			var err error
			LastSnapshot, err = os.ReadFile(*snapshot)
			if err != nil {
				log.Fatalf("error reading snapshot file %q: %v", *snapshot, err)
			}
		}
	}
	if err := restoreLastSnapshot(); err != nil {
		log.Printf("restoring snapshot: %v", err)
	}
	if err := initStats(); err != nil {
		log.Printf("initializing stats: %v", err)
	}

	// if link specified on command line, resolve and exit
	if flag.NArg() > 0 {
		u, err := url.Parse(flag.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		dst, err := resolveLink(u)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(dst.String())
		os.Exit(0)
	}

	// flush stats periodically
	go flushStatsLoop()

	http.HandleFunc("/", serveGo)
	http.HandleFunc("/.detail/", serveDetail)
	http.HandleFunc("/.export", serveExport)
	http.HandleFunc("/.help", serveHelp)
	http.HandleFunc("/.opensearch", serveOpenSearch)
	http.HandleFunc("/.all", serveAll)
	http.HandleFunc("/.delete/", serveDelete)
	http.Handle("/.static/", http.StripPrefix("/.", http.FileServer(http.FS(embeddedFS))))

	if *dev != "" {
		// override default hostname for dev mode
		if *hostname == defaultHostname {
			if h, p, err := net.SplitHostPort(*dev); err == nil {
				if h == "" {
					h = "localhost"
				}
				*hostname = fmt.Sprintf("%s:%s", h, p)
			}
		}

		log.Printf("Running in dev mode on %s ...", *dev)
		log.Fatal(http.ListenAndServe(*dev, nil))
	}

	if *hostname == "" {
		return errors.New("--hostname, if specified, cannot be empty")
	}

	srv := &tsnet.Server{
		ControlURL: *controlURL,
		Hostname:   *hostname,
		Logf:       func(format string, args ...any) {},
	}
	if *verbose {
		srv.Logf = log.Printf
	}
	if err := srv.Start(); err != nil {
		return err
	}
	localClient, _ = srv.LocalClient()

	l80, err := srv.Listen("tcp", ":80")
	if err != nil {
		return err
	}

	log.Printf("Serving http://%s/ ...", *hostname)
	if err := http.Serve(l80, nil); err != nil {
		return err
	}
	return nil
}

var (
	// homeTmpl is the template used by the http://go/ index page where you can
	// create or edit links.
	homeTmpl *template.Template

	// detailTmpl is the template used by the link detail page to view or edit links.
	detailTmpl *template.Template

	// successTmpl is the template used when a link is successfully created or updated.
	successTmpl *template.Template

	// helpTmpl is the template used by the http://go/.help page
	helpTmpl *template.Template

	// allTmpl is the template used by the http://go/.all page
	allTmpl *template.Template

	// deleteTmpl is the template used after a link has been deleted.
	deleteTmpl *template.Template

	// opensearchTmpl is the template used by the http://go/.opensearch page
	opensearchTmpl *template.Template
)

type visitData struct {
	Short     string
	NumClicks int
}

// homeData is the data used by the homeTmpl template.
type homeData struct {
	Short  string
	Clicks []visitData
}

var xsrfKey string

func init() {
	homeTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/home.html"))
	detailTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/detail.html"))
	successTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/success.html"))
	helpTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/help.html"))
	allTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/all.html"))
	deleteTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/delete.html"))
	opensearchTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/opensearch.xml"))

	b := make([]byte, 24)
	rand.Read(b)
	xsrfKey = base64.StdEncoding.EncodeToString(b)
}

// initStats initializes the in-memory stats counter with counts from db.
func initStats() error {
	stats.mu.Lock()
	defer stats.mu.Unlock()

	clicks, err := db.LoadStats()
	if err != nil {
		return err
	}

	stats.clicks = clicks
	stats.dirty = make(ClickStats)

	return nil
}

// flushStats writes any pending link stats to db.
func flushStats() error {
	stats.mu.Lock()
	defer stats.mu.Unlock()

	if len(stats.dirty) == 0 {
		return nil
	}

	if err := db.SaveStats(stats.dirty); err != nil {
		return err
	}
	stats.dirty = make(ClickStats)
	return nil
}

// flushStatsLoop will flush stats every minute.  This function never returns.
func flushStatsLoop() {
	for {
		if err := flushStats(); err != nil {
			log.Printf("flushing stats: %v", err)
		}
		time.Sleep(time.Minute)
	}
}

// deleteLinkStats removes the link stats from memory.
func deleteLinkStats(link *Link) {
	stats.mu.Lock()
	delete(stats.clicks, link.Short)
	delete(stats.dirty, link.Short)
	stats.mu.Unlock()

	db.DeleteStats(link.Short)
}

func serveHome(w http.ResponseWriter, short string) {
	var clicks []visitData

	stats.mu.Lock()
	for short, numClicks := range stats.clicks {
		clicks = append(clicks, visitData{
			Short:     short,
			NumClicks: numClicks,
		})
	}
	stats.mu.Unlock()

	sort.Slice(clicks, func(i, j int) bool {
		if clicks[i].NumClicks != clicks[j].NumClicks {
			return clicks[i].NumClicks > clicks[j].NumClicks
		}
		return clicks[i].Short < clicks[j].Short
	})
	if len(clicks) > 200 {
		clicks = clicks[:200]
	}

	homeTmpl.Execute(w, homeData{
		Short:  short,
		Clicks: clicks,
	})
}

func serveAll(w http.ResponseWriter, _ *http.Request) {
	if err := flushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	links, err := db.LoadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(links, func(i, j int) bool {
		return links[i].Short < links[j].Short
	})

	allTmpl.Execute(w, links)
}

func serveHelp(w http.ResponseWriter, _ *http.Request) {
	helpTmpl.Execute(w, nil)
}

func serveOpenSearch(w http.ResponseWriter, _ *http.Request) {
	type opensearchData struct {
		Hostname string
	}

	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	opensearchTmpl.Execute(w, opensearchData{Hostname: *hostname})
}

func serveGo(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		switch r.Method {
		case "GET":
			serveHome(w, "")
		case "POST":
			serveSave(w, r)
		}
		return
	}

	short, remainder, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	// redirect {name}+ links to /.detail/{name}
	if strings.HasSuffix(short, "+") {
		http.Redirect(w, r, "/.detail/"+strings.TrimSuffix(short, "+"), http.StatusFound)
		return
	}

	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		w.WriteHeader(http.StatusNotFound)
		serveHome(w, short)
		return
	}
	if err != nil {
		log.Printf("serving %q: %v", short, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stats.mu.Lock()
	if stats.clicks == nil {
		stats.clicks = make(ClickStats)
	}
	stats.clicks[link.Short]++
	if stats.dirty == nil {
		stats.dirty = make(ClickStats)
	}
	stats.dirty[link.Short]++
	stats.mu.Unlock()

	login, _ := currentUser(r)
	env := expandEnv{Now: time.Now().UTC(), Path: remainder, user: login, query: r.URL.Query()}
	target, err := expandLink(link.Long, env)
	if err != nil {
		log.Printf("expanding %q: %v", link.Long, err)
		if errors.Is(err, errNoUser) {
			http.Error(w, "link requires a valid user", http.StatusUnauthorized)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// acceptHTML returns whether the request can accept a text/html response.
func acceptHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// detailData is the data used by the detailTmpl template.
type detailData struct {
	// Editable indicates whether the current user can edit the link.
	Editable bool
	Link     *Link
	XSRF     string
}

func serveDetail(w http.ResponseWriter, r *http.Request) {
	short := strings.TrimPrefix(r.URL.Path, "/.detail/")

	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("serving detail %q: %v", short, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !acceptHTML(r) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(link)
		return
	}

	login, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ownerExists, err := userExists(r.Context(), link.Owner)
	if err != nil {
		log.Printf("looking up tailnet user %q: %v", link.Owner, err)
	}

	data := detailData{Link: link}
	if link.Owner == login || !ownerExists {
		data.Editable = true
		data.Link.Owner = login
		data.XSRF = xsrftoken.Generate(xsrfKey, login, short)
	}

	detailTmpl.Execute(w, data)
}

type expandEnv struct {
	Now time.Time

	// Path is the remaining path after short name.  For example, in
	// "http://go/who/amelie", Path is "amelie".
	Path string

	// user is the current user, if any.
	// For example, "foo@example.com" or "foo@github".
	user string

	// query is the query parameters from the original request.
	query url.Values
}

var errNoUser = errors.New("no user")

// User returns the current user, or errNoUser if there is no user.
func (e expandEnv) User() (string, error) {
	if e.user == "" {
		return "", errNoUser
	}
	return e.user, nil
}

var expandFuncMap = texttemplate.FuncMap{
	"PathEscape":  url.PathEscape,
	"QueryEscape": url.QueryEscape,
	"TrimSuffix":  strings.TrimSuffix,
}

// expandLink returns the expanded long URL to redirect to, executing any
// embedded templates with env data.
//
// If long does not include templates, the default behavior is to append
// env.Path to long.
func expandLink(long string, env expandEnv) (*url.URL, error) {
	if !strings.Contains(long, "{{") {
		// default behavior is to append remaining path to long URL
		if strings.HasSuffix(long, "/") {
			long += "{{.Path}}"
		} else {
			long += "{{with .Path}}/{{.}}{{end}}"
		}
	}
	tmpl, err := texttemplate.New("").Funcs(expandFuncMap).Parse(long)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	if err := tmpl.Execute(buf, env); err != nil {
		return nil, err
	}

	u, err := url.Parse(buf.String())
	if err != nil {
		return nil, err
	}

	// add query parameters from original request
	if len(env.query) > 0 {
		query := u.Query()
		for key, values := range env.query {
			for _, v := range values {
				query.Add(key, v)
			}
		}
		u.RawQuery = query.Encode()
	}

	return u, nil
}

func devMode() bool { return *dev != "" }

// currentUser returns the Tailscale user associated with the request.
// In most cases, this will be the user that owns the device that made the request.
// For tagged devices, the value "tagged-devices" is returned.
// If the user can't be determined (such as requests coming through a subnet router),
// an error is returned unless the -allow-unknown-users flag is set.
var currentUser = func(r *http.Request) (string, error) {
	if devMode() {
		return "foo@example.com", nil
	}
	whois, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		if *allowUnknownUsers {
			// Don't report the error if we are allowing unknown users.
			return "", nil
		}
		return "", err
	}
	return whois.UserProfile.LoginName, nil
}

// userExists returns whether a user exists with the specified login in the current tailnet.
func userExists(ctx context.Context, login string) (bool, error) {
	const userTaggedDevices = "tagged-devices" // owner of tagged devices

	if devMode() {
		// in dev mode, just assume the user exists
		return true, nil
	}
	st, err := localClient.Status(ctx)
	if err != nil {
		return false, err
	}
	for _, user := range st.User {
		if user.LoginName == userTaggedDevices {
			continue
		}
		if user.LoginName == login {
			return true, nil
		}
	}
	return false, nil
}

var reShortName = regexp.MustCompile(`^\w[\w\-\.]*$`)

func serveDelete(w http.ResponseWriter, r *http.Request) {
	short := strings.TrimPrefix(r.URL.Path, "/.delete/")
	if short == "" {
		http.Error(w, "short required", http.StatusBadRequest)
		return
	}

	login, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}

	if link.Owner != login {
		http.Error(w, "cannot delete link owned by another user", http.StatusForbidden)
		return
	}

	if !xsrftoken.Valid(r.PostFormValue("xsrf"), xsrfKey, login, short) {
		http.Error(w, "invalid XSRF token", http.StatusBadRequest)
		return
	}

	if err := db.Delete(short); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	deleteLinkStats(link)

	deleteTmpl.Execute(w, link)
}

// serveSave handles requests to save or update a Link.  Both short name and
// long URL are validated for proper format. Existing links may only be updated
// by their owner.
func serveSave(w http.ResponseWriter, r *http.Request) {
	short, long := r.FormValue("short"), r.FormValue("long")
	if short == "" || long == "" {
		http.Error(w, "short and long required", http.StatusBadRequest)
		return
	}
	if !reShortName.MatchString(short) {
		http.Error(w, "short may only contain letters, numbers, dash, and period", http.StatusBadRequest)
		return
	}
	if _, err := texttemplate.New("").Funcs(expandFuncMap).Parse(long); err != nil {
		http.Error(w, fmt.Sprintf("long contains an invalid template: %v", err), http.StatusBadRequest)
		return
	}

	login, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := db.Load(short)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	if link != nil && link.Owner != "" && link.Owner != login {
		exists, err := userExists(r.Context(), link.Owner)
		if err != nil {
			log.Printf("looking up tailnet user %q: %v", link.Owner, err)
		}
		// Don't allow taking over links if the owner account still exists
		// or if we're unsure because an error occurred.
		if exists || err != nil {
			http.Error(w, "not your link; owned by "+link.Owner, http.StatusForbidden)
			return
		}
	}

	// allow transferring ownership to valid users. If empty, set owner to current user.
	owner := r.FormValue("owner")
	if owner != "" {
		exists, err := userExists(r.Context(), owner)
		if err != nil {
			log.Printf("looking up tailnet user %q: %v", owner, err)
		}
		if !exists {
			http.Error(w, "new owner not a valid user: "+owner, http.StatusBadRequest)
			return
		}
	} else {
		owner = login
	}

	now := time.Now().UTC()
	if link == nil {
		link = &Link{
			Short:   short,
			Created: now,
		}
	}
	link.Short = short
	link.Long = long
	link.LastEdit = now
	link.Owner = owner
	if err := db.Save(link); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if acceptHTML(r) {
		successTmpl.Execute(w, homeData{Short: short})
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(link)
	}
}

// serveExport prints a snapshot of the link database. Links are JSON encoded
// and printed one per line. This format is used to restore link snapshots on
// startup.
func serveExport(w http.ResponseWriter, _ *http.Request) {
	if err := flushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	links, err := db.LoadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(links, func(i, j int) bool {
		return links[i].Short < links[j].Short
	})
	encoder := json.NewEncoder(w)
	for _, link := range links {
		if err := encoder.Encode(link); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func restoreLastSnapshot() error {
	bs := bufio.NewScanner(bytes.NewReader(LastSnapshot))
	var restored int
	for bs.Scan() {
		link := new(Link)
		if err := json.Unmarshal(bs.Bytes(), link); err != nil {
			return err
		}
		if link.Short == "" {
			continue
		}
		_, err := db.Load(link.Short)
		if err == nil {
			continue // exists
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := db.Save(link); err != nil {
			return err
		}
		restored++
	}
	if restored > 0 {
		log.Printf("Restored %v links.", restored)
	}
	return bs.Err()
}

func resolveLink(link *url.URL) (*url.URL, error) {
	path := link.Path

	// if link was specified as "go/name", it will parse with no scheme or host.
	// Trim "go" prefix from beginning of path.
	if link.Host == "" {
		path = strings.TrimPrefix(path, *hostname)
	}

	short, remainder, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	l, err := db.Load(short)
	if err != nil {
		return nil, err
	}
	dst, err := expandLink(l.Long, expandEnv{Now: time.Now().UTC(), Path: remainder})
	if err == nil {
		if dst.Host == "" || dst.Host == *hostname {
			dst, err = resolveLink(dst)
		}
	}
	return dst, err
}
