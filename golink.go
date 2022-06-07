// The golink server runs http://go/, a company shortlink service.
package main

import (
	"bufio"
	"bytes"
	"embed"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

//go:embed *.html
var embeddedFS embed.FS

var localClient *tailscale.LocalClient

// DiskLink is the JSON structure stored on disk in a file for each go short link.
type DiskLink struct {
	Short    string // the "foo" part of http://go/foo
	Long     string // the target URL
	Created  time.Time
	LastEdit time.Time // when the link was created
	Owner    string    // foo@tailscale.com
}

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

	if *doMkdir {
		if err := os.MkdirAll(*linkDir, 0755); err != nil {
			log.Fatal(err)
		}
	}

	if fi, err := os.Stat(*linkDir); err != nil {
		log.Fatal(err)
	} else if !fi.IsDir() {
		log.Fatalf("--linkdir %q is not a directory", *linkDir)
	}

	restoreLastSnapshot()

	if *dev != "" {
		log.Printf("Running in dev mode on %s ...", *dev)
		log.Fatal(http.ListenAndServe(*dev, http.HandlerFunc(serveGo)))
	}

	srv := &tsnet.Server{
		Hostname: "go",
		Logf:     func(format string, args ...interface{}) {},
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
	if err := http.Serve(l80, http.HandlerFunc(serveGo)); err != nil {
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

func linkPath(short string) string {
	name := url.PathEscape(strings.ToLower(short))
	name = strings.ReplaceAll(name, "-", "")
	name = strings.ReplaceAll(name, ".", "%2e")
	return filepath.Join(*linkDir, name)
}

func loadLink(short string) (*DiskLink, error) {
	data, err := os.ReadFile(linkPath(short))
	if err != nil {
		return nil, err
	}
	dl := new(DiskLink)
	if err := json.Unmarshal(data, dl); err != nil {
		return nil, err
	}
	return dl, nil
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
	if r.RequestURI == "/_/export" {
		serveExport(w, r)
		return
	}

	if r.RequestURI == "/" {
		switch r.Method {
		case "GET":
			serveHome(w, "")
		case "POST":
			serveSave(w, r)
		}
		return
	}

	short, remainder, _ := strings.Cut(strings.ToLower(strings.TrimPrefix(r.RequestURI, "/")), "/")

	dl, err := loadLink(short)
	if os.IsNotExist(err) {
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
	stats.clicks[dl.Short]++
	stats.mu.Unlock()

	target, err := expandLink(dl.Long, expandEnv{Now: time.Now().UTC(), Path: remainder})
	if err != nil {
		log.Printf("expanding %q: %v", dl.Long, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

var reVarExpand = regexp.MustCompile(`\$\{\w+\}`)

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

var reShortName = regexp.MustCompile(`^[\w\-\.]+$`)

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

	login := ""
	if devMode() {
		login = "foo@example.com"
	} else {
		res, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		login = res.UserProfile.LoginName
	}

	dl, err := loadLink(short)
	if err == nil && dl.Owner != login {
		http.Error(w, "not your link; owned by "+dl.Owner, http.StatusForbidden)
		return
	}

	now := time.Now().UTC()
	if dl == nil {
		dl = &DiskLink{
			Short:   short,
			Created: now,
		}
	}
	dl.Short = short
	dl.Long = long
	dl.LastEdit = now
	dl.Owner = login
	j, err := json.MarshalIndent(dl, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(linkPath(short), j, 0600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "<h1>saved</h1>made <a href='http://go/%s'>http://go/%s</a>", html.EscapeString(short), html.EscapeString(short))
}

func serveExport(w http.ResponseWriter, r *http.Request) {
	d, err := os.Open(*linkDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer d.Close()

	names, err := d.Readdirnames(0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		d, err := os.ReadFile(filepath.Join(*linkDir, name))
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		buf.Reset()
		if err := json.Compact(&buf, d); err != nil {
			panic(http.ErrAbortHandler)
		}
		fmt.Fprintf(w, "%s\n", buf.Bytes())
	}
}

func restoreLastSnapshot() error {
	bs := bufio.NewScanner(bytes.NewReader(lastSnapshot))
	var restored int
	for bs.Scan() {
		data := bs.Bytes()
		dl := new(DiskLink)
		if err := json.Unmarshal(data, dl); err != nil {
			return err
		}
		if dl.Short == "" {
			continue
		}
		file := linkPath(dl.Short)
		_, err := os.Stat(file)
		if err == nil {
			continue // exists
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(file, data, 0644); err != nil {
			return err
		}
		restored++
	}
	if restored > 0 {
		log.Printf("Restored %v links.", restored)
	}
	return bs.Err()
}
