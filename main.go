// cpdlc-proxy shares one Hoppie station per FIR between its sectors (docs/developer-guide.md).
//
//	HOPPIE_LOGON=<proxy's own code> cpdlc-proxy -config config.json -listen :8080
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.json", "FIR configuration")
	listen := flag.String("listen", ":8080", "listen address")
	statePath := flag.String("state", "", "state file; empty: no persistence")
	flag.Parse()

	logon := os.Getenv("HOPPIE_LOGON")
	if logon == "" {
		log.Fatal("HOPPIE_LOGON is not set")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("%s: %v", *configPath, err)
	}
	if cfg.HoppieURL == "" {
		cfg.HoppieURL = "https://www.hoppie.nl/acars/system/connect.html"
	}
	if len(cfg.Stations) == 0 {
		log.Fatalf("%s: no stations", *configPath)
	}

	proxy := newProxy(cfg, logon)
	proxy.statePath = *statePath
	if n, err := proxy.load(); err != nil {
		log.Printf("state %s unreadable, starting empty: %v", *statePath, err)
	} else if *statePath != "" {
		log.Printf("state %s: %d connections restored", *statePath, n)
	} else {
		log.Printf("no state file: state is lost on restart")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ran := make(chan struct{})
	go func() {
		proxy.run(ctx)
		close(ran)
	}()

	server := &http.Server{Addr: *listen, Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	closed := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		close(closed)
	}()
	log.Printf("listening on %s, %d stations, hoppie %s", *listen, len(cfg.Stations), cfg.HoppieURL)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-closed
	<-ran
	if err := proxy.save(); err != nil { // after HTTP and the poller stopped: nothing changes it any more
		log.Printf("state %s: %v", *statePath, err)
	}
	log.Printf("stopped")
}
