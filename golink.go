// The golink server runs http://go/, a private shortlink service for tailnets.
package main

import (
	"bufio"
	"bytes"
	"embed"
	_ "embed"
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
	"strings"
	"sync"
	"text/template"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

var (
	verbose = flag.Bool("verbose", false, "be verbose")
	linkDir = flag.String("linkdir", "", "the directory to store one JSON file per go/ shortlink")
	dev     = flag.String("dev-listen", "", "if non-empty, listen on this addr and run in dev mode; auto-set linkDir if empty and don't use tsnet")
	doMkdir = flag.Bool("mkdir", false, "whether to make --linkdir at start")
)

var stats struct {
	mu     sync.Mutex
	clicks map[string]int // short link -> number of times visited
}

//go:embed link-snapshot.json
var lastSnapshot []byte

//go:embed *.html static
var embeddedFS embed.FS

// db stores short links.
var db DB

var localClient *tailscale.LocalClient

func main() {
	flag.Parse()

	if *linkDir == "" {
		if devMode() {
			var err error
			*linkDir, err = ioutil.TempDir("", "golink_dev_*")
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Dev mode temp dir: %s", *linkDir)
		} else {
			log.Fatalf("--linkdir is required")
		}
	}

	var err error
	db, err = NewFileDB(*linkDir, *doMkdir)
	if err != nil {
		log.Fatalf("NewFileDB(%q): %v", *linkDir, err)
	}

	restoreLastSnapshot()

	http.HandleFunc("/", serveGo)
	http.HandleFunc("/_/export", serveExport)
	http.Handle("/_/static/", http.StripPrefix("/_/", http.FileServer(http.FS(embeddedFS))))

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

// homeCreate is the template used by the http://go/ index page where you can
// create or edit links.
var homeCreate *template.Template

type visitData struct {
	Short     string
	NumClicks int
}

// homeData is the data used by the homeCreate template.
type homeData struct {
	Short  string
	Clicks []visitData
}

func init() {
	homeCreate = template.Must(template.ParseFS(embeddedFS, "home.html"))
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

	homeCreate.Execute(w, homeData{
		Short:  html.EscapeString(short),
		Clicks: clicks,
	})
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

	stats.mu.Lock()
	if stats.clicks == nil {
		stats.clicks = make(map[string]int)
	}
	stats.clicks[link.Short]++
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

var reShortName = regexp.MustCompile(`^[\w\-\.]+$`)

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
	if err == nil && link.Owner != login {
		http.Error(w, "not your link; owned by "+link.Owner, http.StatusForbidden)
		return
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
	fmt.Fprintf(w, "<h1>saved</h1>made <a href='http://go/%s'>http://go/%s</a>", html.EscapeString(short), html.EscapeString(short))
}

// serveExport prints a snapshot of the link database. Links are JSON encoded
// and printed one per line. This format is used to restore link snapshots on
// startup.
func serveExport(w http.ResponseWriter, r *http.Request) {
	names, err := db.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Strings(names)
	encoder := json.NewEncoder(w)
	for _, name := range names {
		link, err := db.Load(name)
		if err != nil {
			panic(http.ErrAbortHandler)
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
