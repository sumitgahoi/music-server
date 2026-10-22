package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"music-server/internal/inventory"
	"music-server/internal/web"
)

func main() {
	rootFlag := flag.String("root", "", "music folder to inventory")
	dbFlag := flag.String("db", "inventory.db", "SQLite inventory database")
	addr := flag.String("addr", "0.0.0.0:8080", "listen address")
	scan := flag.Bool("scan", true, "scan the folder in the background")
	scanInterval := flag.Duration("scan-interval", 15*time.Minute, "rescan period; 0 disables the timer")
	flag.Parse()
	if *rootFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: server -root DIR [-db inventory.db] [-addr 0.0.0.0:8080] [-scan=false] [-scan-interval 15m]")
		os.Exit(2)
	}

	root, err := inventory.CleanRoot(*rootFlag)
	if err != nil {
		log.Fatal(err)
	}
	store, err := inventory.Open(*dbFlag)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if !*scan {
		if bound, err := store.BoundRoot(); err != nil {
			log.Fatal(err)
		} else if bound == "" {
			log.Fatal("inventory has not been scanned")
		} else if bound != root {
			log.Fatalf("inventory database belongs to %s", bound)
		}
	}

	handler := web.New(root, store)
	handler.Start(*scan, *scanInterval)
	server := &http.Server{
		Addr:              *addr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on http://%s (health at /health)", *addr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
