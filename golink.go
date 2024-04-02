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
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/util/dnsname"
)

const (
	defaultHostname = "go"

	// Used as a placeholder short name for generating the XSRF defense token,
	// when creating new links.
	newShortName = ".new"

	// If the caller sends this header set to a non-empty value, we will allow
	// them to make the call even without an XSRF token. JavaScript in browser
	// cannot set this header, per the [Fetch Spec].
	//
	// [Fetch Spec]: https://fetch.spec.whatwg.org
	secHeaderName = "Sec-Golink"
)

var (
	verbose           = flag.Bool("verbose", false, "be verbose")
	controlURL        = flag.String("control-url", ipn.DefaultControlURL, "the URL base of the control plane (i.e. coordination server)")
	sqlitefile        = flag.String("sqlitedb", "", "path of SQLite database to store links")
	dev               = flag.String("dev-listen", "", "if non-empty, listen on this addr and run in dev mode; auto-set sqlitedb if empty and don't use tsnet")
	useHTTPS          = flag.Bool("https", true, "serve golink over HTTPS if enabled on tailnet")
	snapshot          = flag.String("snapshot", "", "file path of snapshot file")
	hostname          = flag.String("hostname", defaultHostname, "service name")
	configDir         = flag.String("config-dir", "", `tsnet configuration directory ("" to use default)`)
	resolveFromBackup = flag.String("resolve-from-backup", "", "resolve a link from snapshot file and exit")
	allowUnknownUsers = flag.Bool("allow-unknown-users", false, "allow unknown users to save links")
	readonly          = flag.Bool("readonly", false, "start golink server in read-only mode")
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
			tmpdir, err := os.MkdirTemp("", "golink_dev_*")
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
		log.Fatal(http.ListenAndServe(*dev, serveHandler()))
	}

	if *hostname == "" {
		return errors.New("--hostname, if specified, cannot be empty")
	}

	// create tsNet server and wait for it to be ready & connected.
	srv := &tsnet.Server{
		ControlURL:   *controlURL,
		Dir:          *configDir,
		Hostname:     *hostname,
		Logf:         func(format string, args ...any) {},
		RunWebClient: true,
	}
	if *verbose {
		srv.Logf = log.Printf
	}
	if err := srv.Start(); err != nil {
		return err
	}

	localClient, _ = srv.LocalClient()
out:
	for {
		upCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		status, err := srv.Up(upCtx)
		if err == nil && status != nil {
			break out
		}
	}

	statusCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := localClient.Status(statusCtx)
	if err != nil {
		return err
	}
	enableTLS := *useHTTPS && status.Self.HasCap(tailcfg.CapabilityHTTPS) && len(srv.CertDomains()) > 0
	fqdn := strings.TrimSuffix(status.Self.DNSName, ".")

	httpHandler := serveHandler()
	if enableTLS {
		httpsHandler := HSTS(httpHandler)
		httpHandler = redirectHandler(fqdn)

		httpsListener, err := srv.ListenTLS("tcp", ":443")
		if err != nil {
			return err
		}
		log.Println("Listening on :443")
		go func() {
			log.Printf("Serving https://%s/ ...", fqdn)
			if err := http.Serve(httpsListener, httpsHandler); err != nil {
				log.Fatal(err)
			}
		}()
	}

	httpListener, err := srv.Listen("tcp", ":80")
	log.Println("Listening on :80")
	if err != nil {
		return err
	}
	log.Printf("Serving http://%s/ ...", *hostname)
	if err := http.Serve(httpListener, httpHandler); err != nil {
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

// homeData is the data used by homeTmpl.
type homeData struct {
	Short    string
	Long     string
	Clicks   []visitData
	XSRF     string
	ReadOnly bool
}

// deleteData is the data used by deleteTmpl.
type deleteData struct {
	Short string
	Long  string
	XSRF  string
}

var xsrfKey string

func init() {
	homeTmpl = newTemplate("base.html", "home.html")
	detailTmpl = newTemplate("base.html", "detail.html")
	successTmpl = newTemplate("base.html", "success.html")
	helpTmpl = newTemplate("base.html", "help.html")
	allTmpl = newTemplate("base.html", "all.html")
	deleteTmpl = newTemplate("base.html", "delete.html")
	opensearchTmpl = newTemplate("opensearch.xml")

	b := make([]byte, 24)
	rand.Read(b)
	xsrfKey = base64.StdEncoding.EncodeToString(b)
}

var tmplFuncs = template.FuncMap{
	// go is a template function that returns the hostname of the golink service.
	// This is used throughout the UI to render links, but does not impact link resolution.
	"go": func() string {
		if devMode() {
			// in dev mode, just use "go" instead of "localhost:8080"
			return defaultHostname
		}
		return *hostname
	},
}

// newTemplate creates a new template with the specified files in the tmpl directory.
// The first file name is used as the template name,
// and tmplFuncs are registered as available funcs.
// This func panics if unable to parse files.
func newTemplate(files ...string) *template.Template {
	if len(files) == 0 {
		return nil
	}
	tf := make([]string, 0, len(files))
	for _, f := range files {
		tf = append(tf, "tmpl/"+f)
	}
	t := template.New(files[0]).Funcs(tmplFuncs)
	return template.Must(t.ParseFS(embeddedFS, tf...))
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

// redirectHandler returns the http.Handler for serving all plaintext HTTP
// requests. It redirects all requests to the HTTPs version of the same URL.
func redirectHandler(hostname string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := &url.URL{
			Scheme:   "https",
			Host:     hostname,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
}

// HSTS wraps the provided handler and sets Strict-Transport-Security header on
// responses. It inspects the Host header to ensure we do not specify HSTS
// response on non fully qualified domain name origins.
func HSTS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, found := r.Header["Host"]
		if found {
			host := host[0]
			fqdn, err := dnsname.ToFQDN(host)
			if err == nil {
				segCount := fqdn.NumLabels()
				if segCount > 1 {
					w.Header().Set("Strict-Transport-Security", "max-age=31536000")
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// serverHandler returns the main http.Handler for serving all requests.
func serveHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.detail/", serveDetail)
	mux.HandleFunc("/.export", serveExport)
	mux.HandleFunc("/.help", serveHelp)
	mux.HandleFunc("/.opensearch", serveOpenSearch)
	mux.HandleFunc("/.all", serveAll)
	mux.HandleFunc("/.delete/", serveDelete)
	mux.Handle("/.static/", http.StripPrefix("/.", http.FileServer(http.FS(embeddedFS))))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// all internal URLs begin with a leading "."; any other URL is treated as a go link.
		// Serve go links directly without passing through the ServeMux,
		// which sometimes modifies the request URL path, which we don't want.
		if !strings.HasPrefix(r.URL.Path, "/.") {
			serveGo(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func serveHome(w http.ResponseWriter, r *http.Request, short string) {
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

	var long string
	if short != "" && localClient != nil {
		// if a peer exists with the short name, suggest it as the long URL
		st, err := localClient.Status(r.Context())
		if err == nil {
			for _, p := range st.Peer {
				if host, _, ok := strings.Cut(p.DNSName, "."); ok && host == short {
					long = "http://" + host + "/"
					break
				}
			}
		}
	}

	cu, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	homeTmpl.Execute(w, homeData{
		Short:    short,
		Long:     long,
		Clicks:   clicks,
		XSRF:     xsrftoken.Generate(xsrfKey, cu.login, newShortName),
		ReadOnly: *readonly,
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
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	opensearchTmpl.Execute(w, nil)
}

func serveGo(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		switch r.Method {
		case "GET":
			serveHome(w, r, "")
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
		// Trim common punctuation from the end and try again.
		// This catches auto-linking and copy/paste issues that include punctuation.
		if s := strings.TrimRight(short, ".,()[]{}"); short != s {
			short = s
			link, err = db.Load(short)
		}
	}

	if errors.Is(err, fs.ErrNotExist) {
		w.WriteHeader(http.StatusNotFound)
		serveHome(w, r, short)
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

	cu, _ := currentUser(r)
	env := expandEnv{Now: time.Now().UTC(), Path: remainder, user: cu.login, query: r.URL.Query()}
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

	// http.Redirect always cleans the redirect URL, which we don't always want.
	// Instead, manually set status and Location header.
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
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
	if short != link.Short {
		// redirect to canonical short name
		http.Redirect(w, r, "/.detail/"+link.Short, http.StatusFound)
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

	cu, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	canEdit := canEditLink(r.Context(), link, cu)
	ownerExists, err := userExists(r.Context(), link.Owner)
	if err != nil {
		log.Printf("looking up tailnet user %q: %v", link.Owner, err)
	}

	data := detailData{
		Link:     link,
		Editable: canEdit,
		XSRF:     xsrftoken.Generate(xsrfKey, cu.login, link.Short),
	}
	if canEdit && !ownerExists {
		data.Link.Owner = cu.login
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
	"TrimPrefix":  strings.TrimPrefix,
	"TrimSuffix":  strings.TrimSuffix,
	"ToLower":     strings.ToLower,
	"ToUpper":     strings.ToUpper,
	"Match":       regexMatch,
}

func regexMatch(pattern string, s string) bool {
	b, _ := regexp.MatchString(pattern, s)
	return b
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

const peerCapName = "tailscale.com/cap/golink"

type capabilities struct {
	Admin bool `json:"admin"`
}

type user struct {
	login   string
	isAdmin bool
}

// currentUser returns the Tailscale user associated with the request.
// In most cases, this will be the user that owns the device that made the request.
// For tagged devices, the value "tagged-devices" is returned.
// If the user can't be determined (such as requests coming through a subnet router),
// an error is returned unless the -allow-unknown-users flag is set.
var currentUser = func(r *http.Request) (user, error) {
	if devMode() {
		return user{login: "foo@example.com"}, nil
	}
	whois, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		if *allowUnknownUsers {
			// Don't report the error if we are allowing unknown users.
			return user{}, nil
		}
		return user{}, err
	}
	login := whois.UserProfile.LoginName
	caps, _ := tailcfg.UnmarshalCapJSON[capabilities](whois.CapMap, peerCapName)
	for _, cap := range caps {
		if cap.Admin {
			return user{login: login, isAdmin: true}, nil
		}
	}
	return user{login: login}, nil
}

// userExists returns whether a user exists with the specified login in the current tailnet.
func userExists(ctx context.Context, login string) (bool, error) {
	const userTaggedDevices = "tagged-devices" // owner of tagged devices

	if login == userTaggedDevices {
		return false, nil
	}

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
	if *readonly {
		http.Error(w, "golink is in read-only mode", http.StatusMethodNotAllowed)
		return
	}
	short := strings.TrimPrefix(r.URL.Path, "/.delete/")
	if short == "" {
		http.Error(w, "short required", http.StatusBadRequest)
		return
	}

	cu, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}

	if !canEditLink(r.Context(), link, cu) {
		http.Error(w, fmt.Sprintf("cannot delete link owned by %q", link.Owner), http.StatusForbidden)
		return
	}

	// Deletion by CLI has never worked because it has always required the XSRF
	// token. (Refer to commit c7ac33d04c33743606f6224009a5c73aa0b8dec0.) If we
	// want to enable deletion via CLI and to honor allowUnknownUsers for
	// deletion, we could change the below to a call to isRequestAuthorized. For
	// now, always require the XSRF token, thus maintaining the status quo.
	if !xsrftoken.Valid(r.PostFormValue("xsrf"), xsrfKey, cu.login, link.Short) {
		http.Error(w, "invalid XSRF token", http.StatusBadRequest)
		return
	}

	if err := db.Delete(short); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	deleteLinkStats(link)

	deleteTmpl.Execute(w, deleteData{
		Short: link.Short,
		Long:  link.Long,
		XSRF:  xsrftoken.Generate(xsrfKey, cu.login, newShortName),
	})
}

// serveSave handles requests to save or update a Link.  Both short name and
// long URL are validated for proper format. Existing links may only be updated
// by their owner.
func serveSave(w http.ResponseWriter, r *http.Request) {
	if *readonly {
		http.Error(w, "golink is in read-only mode", http.StatusMethodNotAllowed)
		return
	}
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

	cu, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := db.Load(short)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !canEditLink(r.Context(), link, cu) {
		http.Error(w, fmt.Sprintf("cannot update link owned by %q", link.Owner), http.StatusForbidden)
		return
	}

	// short name to use for XSRF token.
	// For new link creation, the special newShortName value is used.
	tokenShortName := newShortName
	if link != nil {
		tokenShortName = link.Short
	}

	if !isRequestAuthorized(r, cu, tokenShortName) {
		http.Error(w, "invalid XSRF token", http.StatusBadRequest)
		return
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
		owner = cu.login
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

// canEditLink returns whether the specified user has permission to edit link.
// Admin users can edit all links.
// Non-admin users can only edit their own links or links without an active owner.
func canEditLink(ctx context.Context, link *Link, u user) bool {
	if *readonly {
		return false
	}
	if link == nil || link.Owner == "" {
		// new or unowned link
		return true
	}

	if u.isAdmin || link.Owner == u.login {
		return true
	}

	owned, err := userExists(ctx, link.Owner)
	if err != nil {
		log.Printf("looking up tailnet user %q: %v", link.Owner, err)
	}
	// Allow editing if the link is currently unowned
	return err == nil && !owned
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
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := db.Save(link); err != nil {
			return err
		}
		restored++
	}
	if restored > 0 && *verbose {
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

func isRequestAuthorized(r *http.Request, u user, short string) bool {
	if *allowUnknownUsers {
		return true
	}
	if r.Header.Get(secHeaderName) != "" {
		return true
	}

	return xsrftoken.Valid(r.PostFormValue("xsrf"), xsrfKey, u.login, short)
}
