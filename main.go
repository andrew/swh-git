package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/andrew/swh-git/internal/swh"
)

const headerTimeout = 10 * time.Second

func main() {
	cfg, err := swh.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	addr := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	flag.StringVar(&cfg.Cache, "cache", cfg.Cache, "bare repository cache")
	flag.StringVar(&cfg.API, "api", cfg.API, "Software Heritage API root")
	flag.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "maximum time to prepare a repository")
	flag.DurationVar(&cfg.Poll, "poll", cfg.Poll, "Vault polling interval")
	flag.Int64Var(&cfg.MaxBytes, "max-bytes", cfg.MaxBytes, "maximum unpacked bundle size")
	flag.Parse()
	p, err := newProxy(cfg)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *addr, Handler: p, ReadHeaderTimeout: headerTimeout}
	log.Printf("serving on http://%s; cache %s", *addr, p.cache)
	err = server.ListenAndServe()
	p.store.Close()
	log.Fatal(err)
}
