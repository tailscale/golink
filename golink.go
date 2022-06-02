// The golink server runs http://go/, a company shortlink service.
package main

import (
	"bufio"
	"bytes"
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
	"sort"
	"strings"
	"text/template"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

var (
	verbose = flag.Bool("verbose", false, "be verbose")
	linkDir = flag.String("linkdir", "", "the directory to store one JSON file per go/ shortlink")
	dev     = flag.String("dev-listen", "", "if non-empty, listen on this addr and run in dev mode; auto-set linkDir if empty and don't use tsnet")
)

//go:embed link-snapshot.json
var lastSnapshot []byte

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

// homeData is the data used by the homeCreate template.
type homeData struct {
	Short string
}

func init() {
	homeCreate = template.Must(template.New("home").Parse(`<html>
<body>
<h1>go/</h1>
shortlink service.

<h2>create</h2>
<form method="POST" action="/">
http://go/<input name=short size=20 value="{{.Short}}"> ==&gt; <input name=long size=40> <input type=submit value="create">
</form>
`))
}

func linkPath(short string) string {
	name := url.PathEscape(strings.ToLower(short))
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

func serveGo(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "/export" {
		serveExport(w, r)
		return
	}

	if r.RequestURI == "/" {
		switch r.Method {
		case "GET":
			homeCreate.Execute(w, homeData{
				Short: "",
			})
		case "POST":
			serveSave(w, r)
		}
		return
	}

	short := strings.ToLower(strings.TrimPrefix(r.RequestURI, "/"))

	dl, err := loadLink(short)
	if os.IsNotExist(err) {
		homeCreate.Execute(w, homeData{
			Short: short,
		})
		return
	}
	if err != nil {
		log.Printf("serving %q: %v", short, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, dl.Long, http.StatusFound)
}

func devMode() bool { return *dev != "" }

func serveSave(w http.ResponseWriter, r *http.Request) {
	short, long := r.FormValue("short"), r.FormValue("long")
	if short == "" || long == "" {
		http.Error(w, "short and long required", http.StatusBadRequest)
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

	now := time.Now()
	if dl == nil {
		dl = &DiskLink{
			Short:   short,
			Created: now,
		}
	}
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
