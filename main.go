package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultTimeout  = 30 * time.Minute
	defaultPoll     = 2 * time.Second
	defaultMaxBytes = 4 << 30
	headerTimeout   = 10 * time.Second
)

func main() {
	cache, err := os.UserCacheDir()
	if err != nil {
		log.Fatal(err)
	}
	addr := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	cacheDir := flag.String("cache", filepath.Join(cache, "swh-git"), "bare repository cache")
	api := flag.String("api", "https://archive.softwareheritage.org/api/1/", "Software Heritage API root")
	timeout := flag.Duration("timeout", defaultTimeout, "maximum time to prepare a repository")
	poll := flag.Duration("poll", defaultPoll, "Vault polling interval")
	maxSize := flag.Int64("max-bytes", defaultMaxBytes, "maximum unpacked bundle size")
	flag.Parse()
	p, err := newProxy(*api, *cacheDir, os.Getenv("SWH_TOKEN"), *timeout, *poll, *maxSize)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *addr, Handler: p, ReadHeaderTimeout: headerTimeout}
	log.Printf("serving on http://%s; cache %s", *addr, p.cache)
	log.Fatal(server.ListenAndServe())
}
