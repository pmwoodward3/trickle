package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	humanize "github.com/dustin/go-humanize"
	httprouter "github.com/julienschmidt/httprouter"
)

type Response struct {
	Template string
	Error    string

	DiskInfo *DiskInfo
	Request  *http.Request

	HTTPHost string

	User string

	FeedSecret string

	Section string

	Friends []Friend

	Download  Download
	Downloads []Download

	File File

	Transfer  Transfer
	Transfers []Transfer
}

var (
	funcMap = template.FuncMap{
		"dlexists": func(id string) bool {
			// Check if it's an active download.
			active, err := ActiveDownload(id)
			if active {
				return true
			}
			// Check if it's a completed download.
			_, err = FindDownload(id)
			return err == nil
		},
		"percent": func(a, b int64) float64 {
			return (float64(a) / float64(b)) * 100
		},
		"bytes": func(n int64) string {
			return humanize.Bytes(uint64(n))
		},
		"time": humanize.Time,
		"truncate": func(s string, n int) string {
			if len(s) > n {
				s = s[:n-3] + "..."
			}
			return s
		},
	}
)

func NewResponse(r *http.Request, ps httprouter.Params) *Response {
	return &Response{
		Request:    r,
		User:       ps.ByName("user"),
		HTTPHost:   httpHost,
		DiskInfo:   NewDiskInfo(),
		FeedSecret: feedSecret.Get(),
	}
}

func Error(w http.ResponseWriter, err error) {
	log.Error(err)
	http.Error(w, fmt.Sprintf("An error has occured: %s", err), http.StatusInternalServerError)
}

func XML(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	enc := xml.NewEncoder(w)
	enc.Indent("", "    ")
	if err := enc.Encode(data); err != nil {
		log.Error(err)
	}
}

func JSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err := enc.Encode(data); err != nil {
		log.Error(err)
	}
}

func HTML(w http.ResponseWriter, target string, data interface{}) {
	t := template.New(target)
	t.Funcs(funcMap)
	for _, filename := range AssetNames() {
		if !strings.HasPrefix(filename, "templates/") {
			continue
		}
		name := strings.TrimPrefix(filename, "templates/")
		b, err := Asset(filename)
		if err != nil {
			Error(w, err)
			return
		}

		var tmpl *template.Template
		if name == t.Name() {
			tmpl = t
		} else {
			tmpl = t.New(name)
		}
		if _, err := tmpl.Parse(string(b)); err != nil {
			Error(w, err)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := t.Execute(w, data); err != nil {
		Error(w, err)
		return
	}
}

func Log(h httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		start := time.Now()
		h(w, r, ps)
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		xff := r.Header.Get("X-Forwarded-For")
		log.Infof("%s %s %s %q %s", ip, xff, r.Method, r.URL.Path, time.Since(start))
	}
}

func Auth(h httprouter.Handle, friend bool) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		failed := true

		user := r.Header.Get(reverseProxyAuthHeader)
		clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		xff := r.Header.Get("X-Forwarded-For")

		// 1. Reverse Proxy (matching header and IP).
		if clientIP == reverseProxyAuthIP && user != "" {
			failed = false
		}

		// 3. Friend Authorization
		if friend {
			func() {
				host := r.FormValue("friend")
				log.Debugf("auth: friend host %q", host)

				if host == "" {
					return
				}

				// Must be on friends list.
				friends, err := ListFriends()
				if err != nil {
					log.Error(err)
					return
				}
				friendly := false
				for _, friend := range friends {
					if host == friend.ID {
						friendly = true
					}
				}
				if !friendly {
					return
				}

				// Reverse IP address lookup must match claimed host.
				if addrs, err := net.LookupHost(host); err == nil {
					for _, addr := range addrs {
						log.Debugf("auth: friend match on client %q", addr)
						if addr == clientIP {
							failed = false
							user = host
							return
						}
						if clientIP == reverseProxyAuthIP && strings.Contains(xff, addr) {
							log.Debugf("auth: friend match addr %q in xff %q", addr, xff)
							failed = false
							user = host
							return
						}
					}
				}
				return
			}()
		}

		if failed {
			log.Errorf("auth failed: client %q", clientIP)
			http.NotFound(w, r)
			return
		}

		// Add "user" to params.
		ps = append(ps, httprouter.Param{Key: "user", Value: user})
		h(w, r, ps)
	}
}
