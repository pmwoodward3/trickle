package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
)

type DiskInfo struct {
	free int64
	used int64
}

func (d *DiskInfo) Total() int64   { return d.free + d.used }
func (d *DiskInfo) TotalMB() int64 { return d.Total() / 1024 / 1024 }
func (d *DiskInfo) TotalGB() int64 { return d.TotalMB() / 1024 }

func (d *DiskInfo) Free() int64   { return d.free }
func (d *DiskInfo) FreeMB() int64 { return d.free / 1024 / 1024 }
func (d *DiskInfo) FreeGB() int64 { return d.FreeMB() / 1024 }

func (d *DiskInfo) Used() int64   { return d.used }
func (d *DiskInfo) UsedMB() int64 { return d.used / 1024 / 1024 }
func (d *DiskInfo) UsedGB() int64 { return d.UsedMB() / 1024 }

func (d *DiskInfo) UsedPercent() float64 {
	return (float64(d.used) / float64(d.Total())) * 100
}

func NewDiskInfo(path string) (*DiskInfo, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, fmt.Errorf("diskinfo failed: %s", err)
	}
	free := stat.Bavail * uint64(stat.Bsize)
	used := (stat.Blocks * uint64(stat.Bsize)) - free
	return &DiskInfo{int64(free), int64(used)}, nil
}

func ls(path string) ([]os.FileInfo, []os.FileInfo, error) {
	list, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, nil, err
	}
	var dirs []os.FileInfo
	var files []os.FileInfo
	for _, f := range list {
		if strings.HasPrefix(f.Name(), ".") { // skip hidden files
			continue
		}
		if f.IsDir() {
			dirs = append(dirs, f)
		} else {
			files = append(files, f)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].ModTime().After(dirs[j].ModTime()) })
	sort.Slice(files, func(i, j int) bool { return files[j].ModTime().After(files[i].ModTime()) })
	return dirs, files, nil
}

func GET(ctx context.Context, rawurl string) (*http.Response, error) {
	return req("GET", ctx, rawurl)
}

func POST(ctx context.Context, rawurl string) (*http.Response, error) {
	return req("POST", ctx, rawurl)
}

func DELETE(ctx context.Context, rawurl string) (*http.Response, error) {
	return req("DELETE", ctx, rawurl)
}

const httpUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.87 Safari/537.36"

func req(method string, ctx context.Context, rawurl string) (*http.Response, error) {
	httpClient := &http.Client{}

	log.Debugf("HTTP request: %s %q (%s)", method, rawurl, ctx)
	req, err := http.NewRequest(method, rawurl, nil)
	if err != nil {
		return nil, err
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	} else {
		httpClient.Timeout = 10 * time.Second
	}

	log.Debugf("HTTP request: %s %q (%s)", method, rawurl, ctx)

	req.Header.Set("User-Agent", httpUserAgent)
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return nil, fmt.Errorf("request failed: %s", http.StatusText(res.StatusCode))
	}
	return res, nil
}

func RandomNumber() (int, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int(binary.LittleEndian.Uint32(b[:])), nil
}
