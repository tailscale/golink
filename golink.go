// The golink server runs http://go/, a private shortlink service for tailnets.
package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

var (
	verbose    = flag.Bool("verbose", false, "be verbose")
	linkDir    = flag.String("linkdir", "", "the directory to store one JSON file per go/ shortlink")
	sqlitefile = flag.String("sqlitedb", "", "path of SQLite database to store links")
	migrate    = flag.Bool("migrate-to-sqlite", false, "migrate link data from file storage to sqlite")
	dev        = flag.String("dev-listen", "", "if non-empty, listen on this addr and run in dev mode; auto-set linkDir if empty and don't use tsnet")
	doMkdir    = flag.Bool("mkdir", false, "whether to make --linkdir at start")
)

var stats struct {
	mu     sync.Mutex
	clicks ClickStats // short link -> number of times visited

	// dirty identifies short link clicks that have not yet been stored.
	dirty ClickStats
}

//go:embed link-snapshot.json
var lastSnapshot []byte

//go:embed static tmpl/*.html
var embeddedFS embed.FS

// db stores short links.
var db DB

var localClient *tailscale.LocalClient

func main() {
	flag.Parse()

	var err error
	db, err = setupDB()
	if err != nil {
		log.Fatalf("setting up database: %v", err)
	}

	if err := restoreLastSnapshot(); err != nil {
		log.Printf("restoring snapshot: %v", err)
	}
	if err := initStats(); err != nil {
		log.Printf("initializing stats: %v", err)
	}

	// flush stats periodically
	go flushStatsLoop()

	http.HandleFunc("/", serveGo)
	http.HandleFunc("/.export", serveExport)
	http.HandleFunc("/.help", serveHelp)
	http.Handle("/_/export", http.RedirectHandler("/.export", http.StatusMovedPermanently))
	http.Handle("/.static/", http.StripPrefix("/.", http.FileServer(http.FS(embeddedFS))))

	if *dev != "" {
		log.Printf("Running in dev mode on %s ...", *dev)
		log.Fatal(http.ListenAndServe(*dev, nil))
	}

	srv := &tsnet.Server{
		Hostname: "go",
		Logf:     func(format string, args ...any) {},
	}
	if *verbose {
		srv.Logf = log.Printf
	}
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
	localClient, _ = srv.LocalClient()

	l80, err := srv.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Serving http://go/ ...")
	if err := http.Serve(l80, nil); err != nil {
		log.Fatal(err)
	}
}

// setupDB returns a DB used for link storage based on CLI flags and migrates
// data if requested.  If flags are provided for both sqlite and file-base
// storage, sqlite is preferred.
func setupDB() (DB, error) {
	if *sqlitefile == "" && *linkDir == "" && !devMode() {
		return nil, errors.New("must specify linkdir or sqlitedb")
	}

	var sqliteDB *SQLiteDB
	if *sqlitefile != "" {
		var err error
		if sqliteDB, err = NewSQLiteDB(*sqlitefile); err != nil {
			return nil, fmt.Errorf("NewSQLiteDB(%q): %w", *sqlitefile, err)
		}
		if !*migrate {
			// not migrating data, so return early
			return sqliteDB, nil
		}
	}

	if *linkDir == "" && devMode() {
		var err error
		*linkDir, err = ioutil.TempDir("", "golink_dev_*")
		if err != nil {
			return nil, err
		}
		log.Printf("Dev mode temp dir: %s", *linkDir)
	}

	var fileDB *FileDB
	if *linkDir != "" {
		var err error
		if fileDB, err = NewFileDB(*linkDir, *doMkdir); err != nil {
			return nil, fmt.Errorf("NewFileDB(%q): %w", *linkDir, err)
		}
	}

	if *migrate {
		if sqliteDB == nil || fileDB == nil {
			return nil, errors.New("migrate-to-sqlite requires both linkdir and sqlitedb to be specified")
		}

		links, err := fileDB.LoadAll()
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			if err := sqliteDB.Save(link); err != nil {
				return nil, err
			}
		}

		stats, err := fileDB.LoadStats()
		if err != nil {
			return nil, err
		}
		if err := sqliteDB.SaveStats(stats); err != nil {
			return nil, err
		}
	}

	if sqliteDB != nil {
		return sqliteDB, nil
	} else {
		return fileDB, nil
	}
}

// homeTmpl is the template used by the http://go/ index page where you can
// create or edit links.
var homeTmpl *template.Template

// helpTmpl is the template used by the http://go/.help page
var helpTmpl *template.Template

type visitData struct {
	Short     string
	NumClicks int
}

// homeData is the data used by the homeTmpl template.
type homeData struct {
	Short  string
	Clicks []visitData
}

func init() {
	homeTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/home.html"))
	helpTmpl = template.Must(template.ParseFS(embeddedFS, "tmpl/base.html", "tmpl/help.html"))
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

func serveHome(w http.ResponseWriter, short string) {
	var clicks []visitData

	stats.mu.Lock()
	for short, numClicks := range stats.clicks {
		clicks = append(clicks, visitData{
			Short:     html.EscapeString(short),
			NumClicks: numClicks,
		})
	}
	stats.mu.Unlock()

	if len(clicks) > 200 {
		clicks = clicks[:200]
	}
	sort.Slice(clicks, func(i, j int) bool {
		if clicks[i].NumClicks != clicks[j].NumClicks {
			return clicks[i].NumClicks > clicks[j].NumClicks
		}
		return clicks[i].Short < clicks[j].Short
	})

	homeTmpl.Execute(w, homeData{
		Short:  html.EscapeString(short),
		Clicks: clicks,
	})
}

func serveHelp(w http.ResponseWriter, r *http.Request) {
	helpTmpl.Execute(w, nil)
}

func serveGo(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "/" {
		switch r.Method {
		case "GET":
			serveHome(w, "")
		case "POST":
			serveSave(w, r)
		}
		return
	}

	short, remainder, _ := strings.Cut(strings.TrimPrefix(r.RequestURI, "/"), "/")

	var serveInfo bool
	if strings.HasSuffix(short, "+") {
		serveInfo = true
		short = strings.TrimSuffix(short, "+")
	}

	link, err := db.Load(short)
	if errors.Is(err, fs.ErrNotExist) {
		serveHome(w, short)
		return
	}
	if err != nil {
		log.Printf("serving %q: %v", short, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if serveInfo {
		j, err := json.MarshalIndent(link, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(j)
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

	target, err := expandLink(link.Long, expandEnv{Now: time.Now().UTC(), Path: remainder})
	if err != nil {
		log.Printf("expanding %q: %v", link.Long, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

type expandEnv struct {
	Now time.Time

	// Path is the remaining path after short name.  For example, in
	// "http://go/who/amelie", Path is "amelie".
	Path string
}

var expandFuncMap = template.FuncMap{
	"PathEscape":  url.PathEscape,
	"QueryEscape": url.QueryEscape,
}

// expandLink returns the expanded long URL to redirect to, executing any
// embedded templates with env data.
//
// If long does not include templates, the default behavior is to append
// env.Path to long.
func expandLink(long string, env expandEnv) (string, error) {
	if !strings.Contains(long, "{{") {
		// default behavior is to append remaining path to long URL
		if strings.HasSuffix(long, "/") {
			long += "{{.Path}}"
		} else {
			long += "{{with .Path}}/{{.}}{{end}}"
		}
	}
	tmpl, err := template.New("").Funcs(expandFuncMap).Parse(long)
	if err != nil {
		return "", err
	}
	buf := new(bytes.Buffer)
	tmpl.Execute(buf, env)
	long = buf.String()

	_, err = url.Parse(long)
	if err != nil {
		return "", err
	}
	return long, nil
}

func devMode() bool { return *dev != "" }

func currentUser(r *http.Request) (string, error) {
	login := ""
	if devMode() {
		login = "foo@example.com"
	} else {
		res, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			return "", err
		}
		login = res.UserProfile.LoginName
	}
	return login, nil

}

// userExists returns whether a user exists with the specified login in the current tailnet.
func userExists(ctx context.Context, login string) (bool, error) {
	st, err := localClient.Status(ctx)
	if err != nil {
		return false, err
	}
	for _, user := range st.User {
		if user.LoginName == login {
			return true, nil
		}
	}
	return false, nil
}

var reShortName = regexp.MustCompile(`^\w[\w\-\.]*$`)

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
	if _, err := template.New("").Funcs(expandFuncMap).Parse(long); err != nil {
		http.Error(w, fmt.Sprintf("long contains an invalid template: %v", err), http.StatusBadRequest)
		return
	}

	login, err := currentUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	link, err := db.Load(short)
	if err == nil && link.Owner != "" && link.Owner != login {
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
	link.Owner = login
	if err := db.Save(link); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		fmt.Fprintf(w, "<h1>saved</h1>made <a href='http://go/%s'>http://go/%s</a>", html.EscapeString(short), html.EscapeString(short))
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(link)
	}
}

// serveExport prints a snapshot of the link database. Links are JSON encoded
// and printed one per line. This format is used to restore link snapshots on
// startup.
func serveExport(w http.ResponseWriter, r *http.Request) {
	if err := flushStats(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	includeClicks := true
	if v := r.FormValue("clicks"); v != "" {
		includeClicks, _ = strconv.ParseBool(v)
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
		if !includeClicks {
			link.Clicks = 0
		}
		if err := encoder.Encode(link); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func restoreLastSnapshot() error {
	bs := bufio.NewScanner(bytes.NewReader(lastSnapshot))
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
