package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pfap/lab/internal/api"
	"github.com/pfap/lab/internal/store"
)

//go:embed web/*
var web embed.FS

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address")
	data := flag.String("data", "./data/lab.json", "state file")
	passwordFile := flag.String("password-file", "./data/password", "file containing the Web login password")
	flag.Parse()
	abs, err := filepath.Abs(*data)
	if err != nil {
		log.Fatal(err)
	}
	s, err := store.Open(abs)
	if err != nil {
		log.Fatal(err)
	}
	assets, err := fs.Sub(web, "web")
	if err != nil {
		log.Fatal(err)
	}
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		log.Fatalf("read password file: %v", err)
	}
	passwordText := strings.TrimSpace(string(password))
	if passwordText == "" {
		log.Fatal("password file is empty")
	}
	h := newAuthenticator(passwordText).Wrap(api.New(s).Handler(http.FileServer(http.FS(assets))))
	server := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10_000_000_000}
	log.Printf("PFAP Lab listening on http://%s (data=%s)", *listen, abs)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Println(err)
		os.Exit(1)
	}
}
