package main

import (
	"bytes"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tricklecloud/trickle/downloader"
	"github.com/tricklecloud/trickle/transcoder"

	log "github.com/Sirupsen/logrus"
	"github.com/eduncan911/podcast"
	"github.com/julienschmidt/httprouter"
)

var (
	version string

	downloadDir string
	incomingDir string

	httpAddr    string
	httpHost    string
	httpPrefix  string
	httpTimeout = 72 * time.Hour // Use a long timeout for slow downloads.

	// torrent
	torrentListenAddr string

	// reverse proxy authentication
	reverseProxyAuthIP     string
	reverseProxyAuthHeader string

	debug bool

	// feed secret
	feedSecret *Secret

	// transcoder
	tcer *transcoder.Transcoder

	// downloader
	dler *downloader.Downloader

	httpReadLimit int64 = 2 * (1024 * 1024) // 2 MB
)

func init() {
	flag.StringVar(&downloadDir, "download-dir", "/data", "download directory")
	flag.StringVar(&incomingDir, "incoming-dir", "/data/.dl", "incoming directory")
	flag.StringVar(&httpAddr, "http-addr", "0.0.0.0:1337", "listen address")
	flag.StringVar(&httpHost, "http-host", "", "HTTP host")
	flag.StringVar(&httpPrefix, "http-prefix", "/trickle", "HTTP URL prefix")
	flag.StringVar(&torrentListenAddr, "torrent-addr", "0.0.0.0:61337", "listen address for torrent client")
	flag.StringVar(&reverseProxyAuthIP, "reverse-proxy-ip", "172.16.1.1", "reverse proxy auth IP")
	flag.StringVar(&reverseProxyAuthHeader, "reverse-proxy-header", "X-Authenticated-User", "reverse proxy auth header")
	flag.BoolVar(&debug, "debug", false, "debug mode")
}

//
// Downloads
//
func index(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	transfers := ListTransfers()

	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	res := NewResponse(r, ps)
	res.Transfers = transfers
	res.Downloads = dls
	res.Section = "downloads"
	HTML(w, "index.html", res)
}

func feedPodcast(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	secret := ps.ByName("secret")
	if secret != feedSecret.Get() {
		log.Errorf("auth: feed invalid secret")
		http.NotFound(w, r)
		return
	}

	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	var updated time.Time
	for _, dl := range dls {
		for _, file := range dl.Files() {
			if !file.Viewable() {
				continue
			}
			modtime := file.Info.ModTime()
			if modtime.After(updated) {
				updated = modtime
			}
		}
	}

	p := podcast.New("Trickle @ "+httpHost, "https://"+httpHost, "Trickle "+httpHost, &updated, &updated)
	p.AddAuthor(httpHost, "trickle@"+httpHost)
	p.AddImage("https://trickle.cloud/static/logo.png") // TODO: serve this directly.

	for _, dl := range dls {
		for _, file := range dl.Files() {
			if !file.Viewable() {
				continue
			}

			typ := podcast.MP4
			switch file.Ext() {
			case "mp4":
				typ = podcast.MP4
			case "m4v":
				typ = podcast.M4V
			case "mp3":
				typ = podcast.MP3
			default:
				continue
			}

			pubDate := file.Info.ModTime()
			stream := fmt.Sprintf("https://%s/trickle/feed/stream/%s/%s?secret=%s", httpHost, dl.ID, file.ID, feedSecret.Get())
			size := file.Info.Size()

			item := podcast.Item{
				Title:       fmt.Sprintf("%s (%s)", file.ID, dl.ID),
				Description: fmt.Sprintf("%s (%s)", file.ID, dl.ID),
				PubDate:     &pubDate,
			}
			item.AddEnclosure(stream, typ, size)
			if _, err := p.AddItem(item); err != nil {
				Error(w, err)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	if err := p.Encode(w); err != nil {
		Error(w, err)
	}
}

func feedReset(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := feedSecret.Reset(); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/?message=feedsecretreset")
}

func feedStream(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	secret := r.FormValue("secret")
	if secret != feedSecret.Get() {
		log.Errorf("auth: feed invalid secret")
		http.NotFound(w, r)
		return
	}

	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	log.Debugf("feed stream: %s", r.URL)
	http.ServeFile(w, r, file.Path)
}

func dlList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}
	res := NewResponse(r, ps)
	res.Downloads = dls
	HTML(w, "downloads/list.html", res)
}

func dlFiles(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	res := NewResponse(r, ps)
	res.Download = dl
	res.Section = "downloads"
	HTML(w, "downloads/files.html", res)
}

func dlView(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	res := NewResponse(r, ps)
	res.Download = dl
	res.File = file
	HTML(w, "downloads/view.html", res)
}

func dlSave(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(file.Path)))

	log.Debugf("%s %s %q (%s)", r.RemoteAddr, ps.ByName("user"), r.Method, r.URL.Path, file.Path)
	http.ServeFile(w, r, file.Path)
}

func dlStream(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	log.Debugf("%s %s %q (%s)", r.RemoteAddr, ps.ByName("user"), r.Method, r.URL.Path, file.Path)
	http.ServeFile(w, r, file.Path)
}

func dlRemove(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := os.RemoveAll(dl.Path()); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/?message=downloadremoved")
}

func dlShare(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")
	dl, err := FindDownload(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := dl.Share(); err != nil {
		Error(w, err)
		return
	}
	JSON(w, `{ status: "success" }`)
}

func dlUnshare(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")
	dl, err := FindDownload(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := dl.Unshare(); err != nil {
		Error(w, err)
		return
	}
	JSON(w, `{ status: "success" }`)
}

//
// Transfers
//
func transferList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	res := NewResponse(r, ps)
	res.Transfers = ListTransfers()
	HTML(w, "transfers/list.html", res)
}

func transferStatus(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	transfer, err := FindTransfer(ps.ByName("id"))

	// Send a blank result so the user doesn't get an error when loading a recently finished transfer.
	if err != nil {
		log.Warn(err)
		fmt.Fprintf(w, "\n")
		return
	}

	res := NewResponse(r, ps)
	res.Transfer = transfer
	res.Section = "nonav"
	HTML(w, "transfers/status.html", res)
}

func transferStart(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	target := r.FormValue("target")
	if target == "" {
		target = ps.ByName("target")
	}
	if err := StartTransfer(target); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/?message=transferstarted")
}

func transferCancel(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := CancelTransfer(ps.ByName("id")); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/?message=transfercanceled")
}

//
// Transcoding
//
func transcodeStart(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	log.Debugf("starting trancode %q", file.Path)

	if err := StartTranscode(file.Path); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/downloads/files/%s", dl.ID)
}

func transcodeCancel(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	log.Debugf("canceling trancode %q", file.Path)

	if err := CancelTranscode(file.Path); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/downloads/files/%s", dl.ID)
}

//
// API v1
//
func v1Status(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	// special auth, localhost only.
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip != "::1" && ip != "127.0.0.1" {
		http.NotFound(w, r)
		return
	}

	status := func() string {
		if tcer.Busy() {
			return "busy"
		}
		if dler.Busy() {
			return "busy"
		}
		return "idle"
	}()

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "%s\n", status)
}

func v1Downloads(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	var downloads []FriendDownload

	for _, dl := range dls {
		if !dl.Shared() {
			continue
		}
		downloads = append(downloads, FriendDownload{
			ID:   dl.ID,
			Size: dl.Size(),
		})
	}

	JSON(w, downloads)
}

func v1Files(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if !dl.Shared() {
		http.NotFound(w, r)
		return
	}

	var files []FriendFile
	for _, f := range dl.Files() {
		files = append(files, FriendFile{
			ID:   f.ID,
			Size: f.Info.Size(),
		})
	}
	JSON(w, files)
}

func v1Stream(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if !dl.Shared() {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	log.Debugf("%s %s %q %q %q", r.RemoteAddr, ps.ByName("user"), r.Method, r.URL.Path, file.Path)
	http.ServeFile(w, r, file.Path)
}

// Friends
func friends(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	friends, err := ListFriends()
	if err != nil {
		Error(w, err)
		return
	}

	res := NewResponse(r, ps)
	res.Friends = friends
	res.Section = "friends"
	HTML(w, "friends.html", res)
}

func friendAdd(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	host := r.FormValue("host")
	if host == "" {
		Error(w, fmt.Errorf("no friend host"))
		return
	}
	if err := AddFriend(host); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/friends?message=friendadded")
}

func friendRemove(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	host := ps.ByName("host")
	if host == "" {
		Error(w, fmt.Errorf("no friend host"))
		return
	}
	if err := RemoveFriend(host); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/friends?message=friendremoved")
}

func friendDownload(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	host := ps.ByName("host")
	dl := ps.ByName("dl")

	f, err := FindFriend(host)
	if err != nil {
		Error(w, err)
		return
	}

	endpoint := fmt.Sprintf("https://%s/trickle/v1/downloads/files/%s?friend=%s", f.ID, dl, httpHost)

	if err := StartTransfer(endpoint); err != nil {
		Error(w, err)
		return
	}

	redirect(w, r, "/?message=transferstarted")
}

// Static assets.
func static(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	path := "static" + ps.ByName("path")
	b, err := Asset(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fi, err := AssetInfo(path)
	if err != nil {
		Error(w, err)
		return
	}
	http.ServeContent(w, r, path, fi.ModTime(), bytes.NewReader(b))
}

func redirect(w http.ResponseWriter, r *http.Request, format string, a ...interface{}) {
	location := httpPrefix
	location += fmt.Sprintf(format, a...)
	http.Redirect(w, r, location, http.StatusFound)
}

func prefix(path string) string {
	return httpPrefix + path
}

func main() {
	flag.Parse()

	usage := func(msg string) {
		if msg != "" {
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", msg)
		}
		fmt.Fprintf(os.Stderr, "Usage: %s [...]", os.Args[0])
		flag.PrintDefaults()
	}

	if debug {
		log.SetLevel(log.DebugLevel)
	}

	if httpHost == "" {
		usage("missing HTTP host")
		os.Exit(1)
	}

	// No trailing slash, please.
	httpPrefix = strings.TrimSuffix(httpPrefix, "/")

	// feed
	feedSecret = NewSecret(filepath.Join(downloadDir, ".secret"))

	// transcoder
	tcer = transcoder.NewTranscoder()

	// downloader
	dler = downloader.NewDownloader(downloadDir, incomingDir, torrentListenAddr, httpHost, func() int64 {
		di, err := NewDiskInfo(downloadDir)
		if err != nil {
			panic(err)
		}
		n := di.Free()
		return n - int64(float64(n)*0.05) // reserve 5% of the disk
	})

	// Trailing slashes break HTTP prefix.
	httpPrefix = strings.TrimSuffix(httpPrefix, "/")

	log.Debugf("debug logging is enabled")

	log.Debugf("download directory is %q", downloadDir)
	log.Debugf("incoming directory is %q", incomingDir)

	//
	// Routes
	//
	r := httprouter.New()

	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.HandleMethodNotAllowed = false
	r.HandleOPTIONS = false
	//r.PanicHandler = func(w http.ResponseWriter, r *http.Request, recv interface{}) { log.Errorf("panic recovery for %v", recv) Error(w, fmt.Errorf("&nbsp;")) return }

	// Feed
	r.GET(prefix("/podcast/:secret"), Log(feedPodcast))
	r.GET(prefix("/feed/stream/:id/*file"), Log(feedStream))
	r.GET(prefix("/feed/reset"), Log(Auth(feedReset, false)))

	// Downloads
	r.GET(prefix("/"), Log(Auth(index, false)))
	r.GET(prefix("/downloads/list"), Log(Auth(dlList, false)))
	r.GET(prefix("/downloads/files/:id"), Log(Auth(dlFiles, false)))
	r.GET(prefix("/downloads/view/:id/*file"), Log(Auth(dlView, false)))
	r.GET(prefix("/downloads/save/:id/*file"), Log(Auth(dlSave, false)))
	r.GET(prefix("/downloads/stream/:id/*file"), Log(Auth(dlStream, false)))
	r.GET(prefix("/downloads/remove/:id"), Log(Auth(dlRemove, false)))
	r.POST(prefix("/downloads/share/:id"), Log(Auth(dlShare, false)))
	r.POST(prefix("/downloads/unshare/:id"), Log(Auth(dlUnshare, false)))

	// Transfers
	r.GET(prefix("/transfers/list"), Log(Auth(transferList, false)))
	r.GET(prefix("/transfers/status/:id"), Log(Auth(transferStatus, false)))
	r.GET(prefix("/transfers/cancel/:id"), Log(Auth(transferCancel, false)))
	r.POST(prefix("/transfers/start"), Log(Auth(transferStart, false)))

	// Transcodings
	r.GET(prefix("/transcode/start/:id/*file"), Log(Auth(transcodeStart, false)))
	r.GET(prefix("/transcode/cancel/:id/*file"), Log(Auth(transcodeCancel, false)))

	// Friends
	r.GET(prefix("/friends"), Log(Auth(friends, true)))
	r.POST(prefix("/friends/add"), Log(Auth(friendAdd, true)))
	r.GET(prefix("/friends/remove/:host"), Log(Auth(friendRemove, true)))
	r.POST(prefix("/friends/download/:host/:dl"), Log(Auth(friendDownload, true)))

	// API v1
	r.GET(prefix("/v1/status"), Log(v1Status))
	r.GET(prefix("/v1/downloads"), Log(Auth(v1Downloads, true)))
	r.GET(prefix("/v1/downloads/files/:id"), Log(Auth(v1Files, true)))
	r.GET(prefix("/v1/downloads/stream/:id/*file"), Log(Auth(v1Stream, true)))

	// Static
	r.GET(prefix("/static/*path"), Log(Auth(static, false)))

	s := &http.Server{
		Handler:      r,
		Addr:         httpAddr,
		WriteTimeout: httpTimeout,
		ReadTimeout:  httpTimeout,
	}
	log.Infof("running HTTP server on http://%s%s", httpAddr, httpPrefix)
	log.Fatal(s.ListenAndServe())
}
