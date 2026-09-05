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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/xsrftoken"
	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/envknob"
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
	clickCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "golink_clicks_total",
			Help: "Total number of clicks for a recognized GoLink",
		},
		[]string{"path"},
	)
	clickNotFound = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "golink_not_found_total",
			Help: "Total number of clicks for a GoLink doesn't exist",
		},
		[]string{"path"},
	)
	totalLinkCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "golinks_total",
			Help: "Total number of GoLinks being served",
		},
	)
)

// LastSnapshot is the data snapshot (as returned by the /.export handler)
// that will be loaded on startup.
var LastSnapshot []byte

//go:embed static tmpl/*.html tmpl/*.xml
var embeddedFS embed.FS

// Options configures a golink HTTP application. The caller retains ownership
// of DB and LocalClient and must keep both usable while serving requests.
type Options struct {
	// DB is the database that stores short links. It is required.
	DB *SQLiteDB

	// LocalClient is the LocalAPI client used for request authorization and
	// tailnet user lookup. It is required for serving requests unless Dev is
	// true; it may be nil for offline use such as CLI link resolution.
	LocalClient *local.Client

	// Hostname is the service name used when rendering links.
	// It defaults to "go".
	Hostname string

	// ReadOnly starts the server in read-only mode.
	ReadOnly bool

	// AllowUnknownUsers allows unknown users to save links.
	AllowUnknownUsers bool

	// ServiceName is the Tailscale Service (e.g. "svc:golink") the server is
	// registered as, if any. In service mode, identity headers injected by
	// tsnet's internal proxy are trusted for requests from loopback.
	ServiceName string

	// Dev runs the server in development mode: requests are authenticated as
	// a fake user without consulting the LocalAPI.
	Dev bool

	// Verbose enables verbose logging.
	Verbose bool
}

// Server is the golink HTTP application. It serves the shortlink UI and API;
// the caller retains ownership of tsnet, listener, and state lifecycle.
type Server struct {
	db                *SQLiteDB
	lc                *local.Client
	hostname          string
	readonly          bool
	allowUnknownUsers bool
	serviceName       string
	dev               bool
	verbose           bool

	xsrfKey string

	stats struct {
		mu     sync.Mutex
		clicks ClickStats // short link -> number of times visited

		// dirty identifies short link clicks that have not yet been stored.
		dirty ClickStats
	}

	// homeTmpl is the template used by the http://go/ index page where you can
	// create or edit links.
	homeTmpl *template.Template

	// detailTmpl is the template used by the link detail page to view or edit links.
	detailTmpl *template.Template

	// successTmpl is the template used when a link is successfully created or updated.
	successTmpl *template.Template

	// helpTmpl is the template used by the http://go/.help page
	helpTmpl *template.Template

	// deleteTmpl is the template used after a link has been deleted.
	deleteTmpl *template.Template

	// opensearchTmpl is the template used by the http://go/.opensearch page
	opensearchTmpl *template.Template

	// searchTmpl is the template used by the http://go/.search page
	searchTmpl *template.Template

	// The following funcs are fields so that they can be overridden in tests.

	// currentUser returns the Tailscale user associated with the request.
	currentUser func(r *http.Request) (user, error)
	// whoisFunc calls the LocalAPI WhoIs; by default it calls lc.WhoIs.
	whoisFunc func(ctx context.Context, ip string) (*apitype.WhoIsResponse, error)
	// trustIdentityHeaders returns whether identity headers injected by
	// tsnet's internal proxy should be trusted for the request.
	trustIdentityHeaders func(r *http.Request) bool
	// extractUserFromHeaders extracts the user from HTTP headers injected by
	// tsnet's internal proxy.
	extractUserFromHeaders func(r *http.Request) user
}

// New creates an initialized golink HTTP application. It does not start any
// listeners or take ownership of the database or LocalAPI client.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("nil database")
	}
	s := &Server{
		db:                opts.DB,
		lc:                opts.LocalClient,
		hostname:          opts.Hostname,
		readonly:          opts.ReadOnly,
		allowUnknownUsers: opts.AllowUnknownUsers,
		serviceName:       opts.ServiceName,
		dev:               opts.Dev,
		verbose:           opts.Verbose,
	}
	if s.hostname == "" {
		s.hostname = defaultHostname
	}

	b := make([]byte, 24)
	rand.Read(b)
	s.xsrfKey = base64.StdEncoding.EncodeToString(b)

	s.homeTmpl = s.newTemplate("base.html", "home.html")
	s.detailTmpl = s.newTemplate("base.html", "detail.html")
	s.successTmpl = s.newTemplate("base.html", "success.html")
	s.helpTmpl = s.newTemplate("base.html", "help.html")
	s.deleteTmpl = s.newTemplate("base.html", "delete.html")
	s.opensearchTmpl = s.newTemplate("opensearch.xml")
	s.searchTmpl = s.newTemplate("base.html", "search.html")

	s.currentUser = s.defaultCurrentUser
	s.whoisFunc = func(ctx context.Context, ip string) (*apitype.WhoIsResponse, error) {
		return s.lc.WhoIs(ctx, ip)
	}
	s.trustIdentityHeaders = s.defaultTrustIdentityHeaders
	s.extractUserFromHeaders = s.defaultExtractUserFromHeaders

	if err := s.restoreSnapshot(LastSnapshot); err != nil {
		log.Printf("restoring snapshot: %v", err)
	}
	if err := s.initStats(); err != nil {
		log.Printf("initializing stats: %v", err)
	}
	if err := s.initMetricsData(); err != nil {
		log.Printf("initializing metrics data: %v", err)
	}
	return s, nil
}

// Run runs golink as a standalone command, owning flag parsing, tsnet, and
// listener lifecycle. It serves until ctx is canceled, then drains its HTTP
// listeners and flushes link stats before it returns. It is the entry point
// of cmd/golink; embedders should use New instead.
func Run(ctx context.Context) error {
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
		advertiseTags     = flag.String("advertise-tags", os.Getenv("TS_ADVERTISE_TAGS"), "comma-separated list of ACL tags to advertise (e.g. tag:golink)")
		serviceName       = flag.String("register-as-service", envknob.String("TS_SERVICE_NAME"), "register as a Tailscale Service (e.g., svc:golink); requires tagged node")
	)
	flag.Parse()

	devMode := func() bool { return *dev != "" }

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

	db, err := NewSQLiteDB(*sqlitefile)
	if err != nil {
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

	s, err := New(Options{
		DB:                db,
		Hostname:          *hostname,
		ReadOnly:          *readonly,
		AllowUnknownUsers: *allowUnknownUsers,
		ServiceName:       *serviceName,
		Dev:               devMode(),
		Verbose:           *verbose,
	})
	if err != nil {
		return err
	}

	// if link specified on command line, resolve and exit
	if flag.NArg() > 0 {
		u, err := url.Parse(flag.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		dst, err := s.resolveLink(u)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(dst.String())
		os.Exit(0)
	}

	// flush stats periodically
	go s.FlushStatsLoop(ctx)

	// Flush once more after the listeners drain. FlushStatsLoop's final
	// flush can run before in-flight requests record their clicks.
	defer func() {
		if err := s.FlushStats(); err != nil {
			log.Printf("flushing stats: %v", err)
		}
	}()

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
		ln, err := net.Listen("tcp", *dev)
		if err != nil {
			return err
		}
		return serveHTTP(ctx, listenerServer{srv: &http.Server{Handler: s.Handler()}, ln: ln})
	}

	if *hostname == "" {
		return errors.New("--hostname, if specified, cannot be empty")
	}

	tags, err := parseAdvertiseTags(*advertiseTags)
	if err != nil {
		return err
	}

	// create tsNet server and wait for it to be ready & connected.
	ts := &tsnet.Server{
		ControlURL:    *controlURL,
		Dir:           *configDir,
		Hostname:      *hostname,
		Logf:          func(format string, args ...any) {},
		RunWebClient:  true,
		AdvertiseTags: tags,
	}
	if *verbose {
		ts.Logf = log.Printf
	}
	if err := ts.Start(); err != nil {
		return err
	}

	lc, err := ts.LocalClient()
	if err != nil {
		return err
	}
	s.lc = lc
out:
	for {
		upCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		status, err := ts.Up(upCtx)
		if err == nil && status != nil {
			break out
		}
	}

	statusCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := lc.Status(statusCtx)
	if err != nil {
		return err
	}
	enableTLS := *useHTTPS && status.Self.HasCap(tailcfg.CapabilityHTTPS) && len(ts.CertDomains()) > 0
	fqdn := strings.TrimSuffix(status.Self.DNSName, ".")

	httpHandler := s.Handler()

	// Service registration mode: use ListenService instead of standard listeners
	if *serviceName != "" {
		if !strings.HasPrefix(*serviceName, "svc:") {
			return fmt.Errorf("service name must start with 'svc:' prefix, got: %q", *serviceName)
		}

		log.Printf("Registering as Tailscale Service: %s", *serviceName)
		serviceListener, err := ts.ListenService(*serviceName, tsnet.ServiceModeHTTP{
			HTTPS: true,
			Port:  443,
		})
		if err != nil {
			if errors.Is(err, tsnet.ErrUntaggedServiceHost) {
				return fmt.Errorf("service registration requires a tagged node; add a tag like 'tag:golink' to this node in the Tailscale admin console")
			}
			return fmt.Errorf("failed to register service: %w", err)
		}

		httpsHandler := HSTS(httpHandler)
		log.Printf("Serving https://%s/ as service %s ...", fqdn, *serviceName)
		return serveHTTP(ctx, listenerServer{srv: &http.Server{Handler: httpsHandler}, ln: serviceListener})
	}

	// Standard mode: use regular listeners
	var servers []listenerServer
	if enableTLS {
		httpsHandler := HSTS(httpHandler)
		httpHandler = RedirectHandler(fqdn)

		httpsListener, err := ts.ListenTLS("tcp", ":443")
		if err != nil {
			return err
		}
		log.Println("Listening on :443")
		log.Printf("Serving https://%s/ ...", fqdn)
		servers = append(servers, listenerServer{srv: &http.Server{Handler: httpsHandler}, ln: httpsListener})
	}

	httpListener, err := ts.Listen("tcp", ":80")
	if err != nil {
		return err
	}
	log.Println("Listening on :80")
	log.Printf("Serving http://%s/ ...", *hostname)
	servers = append(servers, listenerServer{srv: &http.Server{Handler: httpHandler}, ln: httpListener})

	return serveHTTP(ctx, servers...)
}

// listenerServer pairs an http.Server with the listener it serves on.
type listenerServer struct {
	srv *http.Server
	ln  net.Listener
}

// serveHTTP serves each server on its listener until one exits with an error
// or ctx is canceled. On cancel it shuts every server down, letting in-flight
// requests finish first. It returns the first serve or shutdown error it hit,
// or nil when every server stopped cleanly.
func serveHTTP(ctx context.Context, servers ...listenerServer) error {
	errCh := make(chan error, len(servers))
	for _, ls := range servers {
		go func() { errCh <- ls.srv.Serve(ls.ln) }()
	}

	var serveErr error
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ls := range servers {
		if err := ls.srv.Shutdown(shutdownCtx); err != nil && serveErr == nil {
			serveErr = err
		}
	}
	return serveErr
}

type visitData struct {
	Short     string
	NumClicks int
}

// searchResult is a link paired with its current click count, used to render
// the listing template (searchTmpl) served by /.all and /.search.
type searchResult struct {
	*Link
	NumClicks int
}

// searchResults annotates links with their current click counts (read from the
// live in-memory counter, the same source the home page uses), preserving the
// historical alphabetical ordering by short name.
func (s *Server) searchResults(links []*Link) []searchResult {
	s.stats.mu.Lock()
	results := make([]searchResult, len(links))
	for i, link := range links {
		results[i] = searchResult{Link: link, NumClicks: s.stats.clicks[link.Short]}
	}
	s.stats.mu.Unlock()

	sort.Slice(results, func(i, j int) bool {
		return results[i].Short < results[j].Short
	})
	return results
}

// homeData is the data used by homeTmpl.
type homeData struct {
	Short    string
	Long     string
	Clicks   []visitData
	XSRF     string
	ReadOnly bool
	User     string
}

// deleteData is the data used by deleteTmpl.
type deleteData struct {
	Short string
	Long  string
	XSRF  string
}

func init() {
	initMetrics()
}

// tmplFuncs returns the template funcs available to this server's templates.
func (s *Server) tmplFuncs() template.FuncMap {
	return template.FuncMap{
		// go is a template function that returns the hostname of the golink service.
		// This is used throughout the UI to render links, but does not impact link resolution.
		"go": func() string {
			if s.dev {
				// in dev mode, just use "go" instead of "localhost:8080"
				return defaultHostname
			}
			return s.hostname
		},
	}
}

// newTemplate creates a new template with the specified files in the tmpl directory.
// The first file name is used as the template name,
// and tmplFuncs are registered as available funcs.
// This func panics if unable to parse files.
func (s *Server) newTemplate(files ...string) *template.Template {
	if len(files) == 0 {
		return nil
	}
	tf := make([]string, 0, len(files))
	for _, f := range files {
		tf = append(tf, "tmpl/"+f)
	}
	t := template.New(files[0]).Funcs(s.tmplFuncs())
	return template.Must(t.ParseFS(embeddedFS, tf...))
}

// initMetrics initializes Prometheus Metrics
func initMetrics() {
	prometheus.MustRegister(clickCounter)
	prometheus.MustRegister(clickNotFound)
	prometheus.MustRegister(totalLinkCount)
}

// initMetricsData set metrics to what is represented in the DB
func (s *Server) initMetricsData() error {
	// Set the totalLinkCount metric to what is saved in the DB
	var count float64
	err := s.db.db.QueryRow("SELECT COUNT(DISTINCT id) FROM Links").Scan(&count)
	if err != nil {
		return err
	}
	totalLinkCount.Set(count)

	return nil
}

// initStats initializes the in-memory stats counter with counts from db.
func (s *Server) initStats() error {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()

	clicks, err := s.db.LoadStats()
	if err != nil {
		return err
	}

	s.stats.clicks = clicks
	s.stats.dirty = make(ClickStats)

	return nil
}

// FlushStats writes any pending link stats to the database.
func (s *Server) FlushStats() error {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()

	if len(s.stats.dirty) == 0 {
		return nil
	}

	if err := s.db.SaveStats(s.stats.dirty); err != nil {
		return err
	}
	s.stats.dirty = make(ClickStats)
	return nil
}

// FlushStatsLoop flushes stats every minute until ctx is canceled, with a
// final flush on shutdown.
func (s *Server) FlushStatsLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if err := s.FlushStats(); err != nil {
				log.Printf("flushing stats: %v", err)
			}
			return
		case <-time.After(time.Minute):
			if err := s.FlushStats(); err != nil {
				log.Printf("flushing stats: %v", err)
			}
		}
	}
}

// deleteLinkStats removes the link stats from memory.
func (s *Server) deleteLinkStats(link *Link) {
	totalLinkCount.Dec()
	s.stats.mu.Lock()
	delete(s.stats.clicks, link.Short)
	delete(s.stats.dirty, link.Short)
	s.stats.mu.Unlock()

	s.db.DeleteStats(link.Short)
}

// RedirectHandler returns the http.Handler for serving all plaintext HTTP
// requests. It redirects all requests to the HTTPS version of the same URL.
func RedirectHandler(hostname string) http.Handler {
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

// Handler returns the main http.Handler for serving all requests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.detail/", s.serveDetail)
	mux.HandleFunc("/.export", s.serveExport)
	mux.HandleFunc("/.export-stats", s.serveExportStats)
	mux.HandleFunc("/.help", s.serveHelp)
	mux.HandleFunc("/.opensearch", s.serveOpenSearch)
	mux.HandleFunc("/.all", s.serveAll)
	mux.HandleFunc("/.delete/", s.serveDelete)
	mux.HandleFunc("/.search", s.serveSearch)
	mux.Handle("/.metrics", promhttp.Handler())
	mux.Handle("/.static/", http.StripPrefix("/.", http.FileServer(http.FS(embeddedFS))))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never send a Referer header to link destinations, which would
		// otherwise expose the golink host (and thus the tailnet name) to
		// external sites. Setting the policy on redirect responses also
		// strips any referrer inherited from the page that linked to the
		// go link.
		w.Header().Set("Referrer-Policy", "no-referrer")

		// all internal URLs begin with a leading "."; any other URL is treated as a go link.
		// Serve go links directly without passing through the ServeMux,
		// which sometimes modifies the request URL path, which we don't want.
		if !strings.HasPrefix(r.URL.Path, "/.") {
			s.serveGo(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) serveHome(w http.ResponseWriter, r *http.Request, short string) {
	var clicks []visitData

	s.stats.mu.Lock()
	for short, numClicks := range s.stats.clicks {
		clicks = append(clicks, visitData{
			Short:     short,
			NumClicks: numClicks,
		})
	}
	s.stats.mu.Unlock()

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
	if short != "" && s.lc != nil {
		// if a peer exists with the short name, suggest it as the long URL
		st, err := s.lc.Status(r.Context())
		if err == nil {
			for _, p := range st.Peer {
				if host, _, ok := strings.Cut(p.DNSName, "."); ok && host == short {
					long = "http://" + host + "/"
					break
				}
			}
		}
	}

	cu, err := s.currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.homeTmpl.Execute(w, homeData{
		Short:    short,
		Long:     long,
		Clicks:   clicks,
		XSRF:     xsrftoken.Generate(s.xsrfKey, cu.login, newShortName),
		ReadOnly: s.readonly,
		User:     cu.login,
	})
}

func (s *Server) serveAll(w http.ResponseWriter, _ *http.Request) {
	if err := s.FlushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	links, err := s.db.LoadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.searchTmpl.Execute(w, s.searchResults(links))
}

func (s *Server) serveHelp(w http.ResponseWriter, _ *http.Request) {
	s.helpTmpl.Execute(w, nil)
}

func (s *Server) serveOpenSearch(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	s.opensearchTmpl.Execute(w, nil)
}

func (s *Server) serveGo(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		switch r.Method {
		case "GET":
			s.serveHome(w, r, "")
		case "POST":
			s.serveSave(w, r)
		}
		return
	}

	short, remainder, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	// redirect {name}+ links to /.detail/{name}
	if strings.HasSuffix(short, "+") {
		http.Redirect(w, r, "/.detail/"+strings.TrimSuffix(short, "+"), http.StatusFound)
		return
	}

	link, err := s.db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		// Trim common punctuation from the end and try again.
		// This catches auto-linking and copy/paste issues that include punctuation.
		if trimmed := strings.TrimRight(short, ".,()[]{}"); short != trimmed {
			short = trimmed
			link, err = s.db.Load(short)
		}
	}

	if errors.Is(err, fs.ErrNotExist) {
		clickNotFound.WithLabelValues(short).Inc()
		w.WriteHeader(http.StatusNotFound)
		s.serveHome(w, r, short)
		return
	}
	if err != nil {
		clickNotFound.WithLabelValues(short).Inc()
		log.Printf("serving %q: %v", short, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	clickCounter.WithLabelValues(link.Short).Inc()

	s.stats.mu.Lock()
	if s.stats.clicks == nil {
		s.stats.clicks = make(ClickStats)
	}
	s.stats.clicks[link.Short]++
	if s.stats.dirty == nil {
		s.stats.dirty = make(ClickStats)
	}
	s.stats.dirty[link.Short]++
	s.stats.mu.Unlock()

	cu, _ := s.currentUser(r)
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
	Editable      bool
	Link          *Link
	XSRF          string
	AlreadyExists bool
}

func (s *Server) serveDetail(w http.ResponseWriter, r *http.Request) {
	short := strings.TrimPrefix(r.URL.Path, "/.detail/")

	link, err := s.db.Load(short)
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

	cu, err := s.currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	canEdit := s.canEditLink(r.Context(), link, cu)
	ownerExists, err := s.userExists(r.Context(), link.Owner)
	if err != nil {
		log.Printf("looking up tailnet user %q: %v", link.Owner, err)
	}

	data := detailData{
		Link:     link,
		Editable: canEdit,
		XSRF:     xsrftoken.Generate(s.xsrfKey, cu.login, link.Short),
	}
	if r.URL.Query().Get("exists") == "1" {
		data.AlreadyExists = true
	}
	if canEdit && !ownerExists {
		data.Link.Owner = cu.login
	}

	s.detailTmpl.Execute(w, data)
}

// serveSearch handles requests to /.search?q={query}, where {query} can currently only be
// the owner formated like "owner:<email>".
func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	owner, found := strings.CutPrefix(query, "owner:")
	if !found {
		http.Error(w, `search only supports "owner:<email>"`, http.StatusBadRequest)
		return
	}
	links, err := s.db.GetLinksByOwner(owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.searchTmpl.Execute(w, s.searchResults(links))
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

const peerCapName = "tailscale.com/cap/golink"

type capabilities struct {
	Admin bool `json:"admin"`
}

type user struct {
	login   string
	isAdmin bool
}

// defaultCurrentUser returns the Tailscale user associated with the request.
// In most cases, this will be the user that owns the device that made the request.
// For tagged devices, the value "tagged-devices" is returned.
// If the user can't be determined (such as requests coming through a subnet router),
// an error is returned unless the -allow-unknown-users flag is set.
//
// When running as a Tailscale Service, authentication is handled via HTTP headers
// automatically injected by tsnet's internal proxy (Tailscale-User-Login, etc.).
// For regular mode, authentication uses WhoIs with the connection's RemoteAddr.
func (s *Server) defaultCurrentUser(r *http.Request) (user, error) {
	if s.dev {
		return user{login: "foo@example.com"}, nil
	}

	// When running as a Tailscale Service, identity headers are automatically injected
	// by tsnet's internal proxy. Restrict this authentication check to cases
	// when we are running in service mode, and the immediate client connection is
	// on loopback.
	if s.trustIdentityHeaders(r) {
		headerUser := s.extractUserFromHeaders(r)
		if headerUser.login != "" {
			return headerUser, nil
		}
	}

	// Regular mode: use WhoIs with RemoteAddr
	whois, err := s.lc.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		if s.allowUnknownUsers {
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

// defaultTrustIdentityHeaders returns whether we should trust identity headers
// injected by tsnet's internal proxy.
func (s *Server) defaultTrustIdentityHeaders(r *http.Request) bool {
	remoteHost := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remoteHost); err == nil {
		remoteHost = host
	}
	remoteIP := net.ParseIP(remoteHost)

	return s.serviceName != "" && remoteIP != nil && remoteIP.IsLoopback()
}

// defaultExtractUserFromHeaders extracts the user from HTTP headers injected
// by tsnet's internal proxy.
func (s *Server) defaultExtractUserFromHeaders(r *http.Request) user {
	if tsLogin := r.Header.Get("Tailscale-User-Login"); tsLogin != "" {
		// Look for a peer from x-forwarded-for header. We'll use that for the
		// whois/capmap lookup first.
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" {
			// serve.go sets this to a single address, so we can just
			// trim whitespace and use it.
			ip := strings.TrimSpace(xff)

			// only accept well-formed IP addresses
			if net.ParseIP(ip) == nil {
				log.Printf("invalid IP in X-Forwarded-For header: %q", ip)
				return user{login: tsLogin}
			}

			whois, err := s.whoisFunc(r.Context(), ip)

			if err != nil {
				log.Printf("WhoIs lookup for IP %q: %v", ip, err)
				return user{login: tsLogin}
			}

			caps, _ := tailcfg.UnmarshalCapJSON[capabilities](whois.CapMap, peerCapName)

			for _, cap := range caps {
				if cap.Admin {
					return user{login: tsLogin, isAdmin: true}
				}
			}

		}

		// If we can't determine admin status, just return the user without admin privileges
		// This allows the service to continue functioning even if the lookup fails
		return user{login: tsLogin}
	}
	return user{}
}

// userExists returns whether a user exists with the specified login in the current tailnet.
func (s *Server) userExists(ctx context.Context, login string) (bool, error) {
	const userTaggedDevices = "tagged-devices" // owner of tagged devices

	if login == userTaggedDevices {
		return false, nil
	}

	if s.dev {
		// in dev mode, just assume the user exists
		return true, nil
	}
	st, err := s.lc.Status(ctx)
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

func (s *Server) serveDelete(w http.ResponseWriter, r *http.Request) {
	if s.readonly {
		http.Error(w, "golink is in read-only mode", http.StatusMethodNotAllowed)
		return
	}
	short := strings.TrimPrefix(r.URL.Path, "/.delete/")
	if short == "" {
		http.Error(w, "short required", http.StatusBadRequest)
		return
	}

	cu, err := s.currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := s.db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}

	if !s.canEditLink(r.Context(), link, cu) {
		http.Error(w, fmt.Sprintf("cannot delete link owned by %q", link.Owner), http.StatusForbidden)
		return
	}

	// Deletion by CLI has never worked because it has always required the XSRF
	// token. (Refer to commit c7ac33d04c33743606f6224009a5c73aa0b8dec0.) If we
	// want to enable deletion via CLI and to honor allowUnknownUsers for
	// deletion, we could change the below to a call to isRequestAuthorized. For
	// now, always require the XSRF token, thus maintaining the status quo.
	if !xsrftoken.Valid(r.PostFormValue("xsrf"), s.xsrfKey, cu.login, link.Short) {
		http.Error(w, "invalid XSRF token", http.StatusBadRequest)
		return
	}

	if err := s.db.Delete(short); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.deleteLinkStats(link)

	s.deleteTmpl.Execute(w, deleteData{
		Short: link.Short,
		Long:  link.Long,
		XSRF:  xsrftoken.Generate(s.xsrfKey, cu.login, newShortName),
	})
}

// serveSave handles requests to save or update a Link.  Both short name and
// long URL are validated for proper format. Existing links may only be updated
// by their owner.
func (s *Server) serveSave(w http.ResponseWriter, r *http.Request) {
	if s.readonly {
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

	cu, err := s.currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := s.db.Load(short)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !s.canEditLink(r.Context(), link, cu) {
		http.Error(w, fmt.Sprintf("cannot update link owned by %q", link.Owner), http.StatusForbidden)
		return
	}

	// short name to use for XSRF token.
	// For new link creation, the special newShortName value is used.
	// For existing links, the link's short name is used. This intentionally
	// prevents the home page "create" form from overwriting an existing link;
	// to edit an existing link the user must use the detail page edit form
	// which generates a token scoped to that link's short name.
	tokenShortName := newShortName
	if link != nil {
		tokenShortName = link.Short
	}

	if !s.isRequestAuthorized(r, cu, tokenShortName) {
		if link != nil && s.isRequestAuthorized(r, cu, newShortName) {
			// The user submitted from the home page create form but the link
			// already exists. Redirect to the detail page so they can edit it
			// intentionally rather than accidentally overwriting it.
			http.Redirect(w, r, "/.detail/"+url.PathEscape(short)+"?exists=1", http.StatusSeeOther)
		} else {
			http.Error(w, "invalid XSRF token", http.StatusBadRequest)
		}
		return
	}

	// allow transferring ownership to valid users. If empty, set owner to current user.
	owner := r.FormValue("owner")
	if owner != "" {
		exists, err := s.userExists(r.Context(), owner)
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
	newLink := false
	if link == nil {
		link = &Link{
			Short:   short,
			Created: now,
		}
		newLink = true
	}
	link.Short = short
	link.Long = long
	link.LastEdit = now
	link.Owner = owner
	if err := s.db.Save(link); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if acceptHTML(r) {
		s.successTmpl.Execute(w, homeData{Short: short})
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(link)
	}
	// If this is a new link and not an update inc
	if newLink {
		totalLinkCount.Inc()
	}
}

// canEditLink returns whether the specified user has permission to edit link.
// Admin users can edit all links.
// Non-admin users can only edit their own links or links without an active owner.
func (s *Server) canEditLink(ctx context.Context, link *Link, u user) bool {
	if s.readonly {
		return false
	}
	if link == nil || link.Owner == "" {
		// new or unowned link
		return true
	}

	if u.isAdmin || link.Owner == u.login {
		return true
	}

	owned, err := s.userExists(ctx, link.Owner)
	if err != nil {
		log.Printf("looking up tailnet user %q: %v", link.Owner, err)
	}
	// Allow editing if the link is currently unowned
	return err == nil && !owned
}

// serveExport prints a snapshot of the link database. Links are JSON encoded
// and printed one per line. This format is used to restore link snapshots on
// startup.
func (s *Server) serveExport(w http.ResponseWriter, _ *http.Request) {
	if err := s.FlushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	links, err := s.db.LoadAll()
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

// serveExportStats prints a snapshot of the stats database table.
//
// Stats are printed in CSV format with three columns: link ID, UNIX timestamp, and click count.
// Each stat line represents the number of clicks in the previous minute.
func (s *Server) serveExportStats(w http.ResponseWriter, _ *http.Request) {
	if err := s.FlushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rows, err := s.db.db.Query("SELECT ID, Created, Clicks FROM Stats ORDER BY Created, ID")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		rows.Close()
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}()

	for rows.Next() {
		var id string
		var created int64
		var clicks int
		err := rows.Scan(&id, &created, &clicks)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// id is not permitted to contain commas, so no need to worry about CSV quoting
		fmt.Fprintf(w, "%s,%d,%d\n", id, created, clicks)
	}
}

// restoreSnapshot loads a data snapshot (as returned by the /.export handler)
// into the database, skipping links that already exist.
func (s *Server) restoreSnapshot(snapshot []byte) error {
	bs := bufio.NewScanner(bytes.NewReader(snapshot))
	var restored int
	for bs.Scan() {
		link := new(Link)
		if err := json.Unmarshal(bs.Bytes(), link); err != nil {
			return err
		}
		if link.Short == "" {
			continue
		}
		_, err := s.db.Load(link.Short)
		if err == nil {
			continue // exists
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := s.db.Save(link); err != nil {
			return err
		}
		restored++
	}
	if restored > 0 && s.verbose {
		log.Printf("Restored %v links.", restored)
	}
	return bs.Err()
}

func (s *Server) resolveLink(link *url.URL) (*url.URL, error) {
	path := link.Path

	// if link was specified as "go/name", it will parse with no scheme or host.
	// Trim "go" prefix from beginning of path.
	if link.Host == "" {
		path = strings.TrimPrefix(path, s.hostname)
	}

	short, remainder, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	l, err := s.db.Load(short)
	if err != nil {
		return nil, err
	}
	dst, err := expandLink(l.Long, expandEnv{Now: time.Now().UTC(), Path: remainder})
	if err == nil {
		if dst.Host == "" || dst.Host == s.hostname {
			dst, err = s.resolveLink(dst)
		}
	}
	return dst, err
}

func (s *Server) isRequestAuthorized(r *http.Request, u user, short string) bool {
	if s.allowUnknownUsers {
		return true
	}
	if r.Header.Get(secHeaderName) != "" {
		return true
	}

	return xsrftoken.Valid(r.PostFormValue("xsrf"), s.xsrfKey, u.login, short)
}

// parseAdvertiseTags parses a comma-separated list of ACL tags.
// Each tag must start with "tag:". Empty strings are ignored.
func parseAdvertiseTags(s string) ([]string, error) {
	var tags []string
	for _, tag := range strings.Split(s, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if !strings.HasPrefix(tag, "tag:") {
			return nil, fmt.Errorf("invalid advertise tag %q: must start with \"tag:\"", tag)
		}
		tags = append(tags, tag)
	}
	return tags, nil
}
