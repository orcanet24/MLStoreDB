package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"mlstoredb/db"
	"mlstoredb/wire"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:28917", "listen address (host:port)")
	path := flag.String("path", "", "database file (.mlstore); empty = in-memory")
	dbName := flag.String("db", "", "database name exposed to clients (default: file base name or mlstoredb)")
	key := flag.String("key", "", "master key (KEK input; required for encrypted files)")
	machine := flag.String("machine", "", "machine id mixed into the KEK (empty = default)")
	user := flag.String("user", "", "wire auth user (SCRAM-SHA-256 gate; empty = no auth)")
	pass := flag.String("pass", "", "wire auth password")
	light := flag.Bool("light-kdf", false, "faster Argon2 params (first creation only)")
	flag.Parse()

	name := *dbName
	if name == "" {
		if *path != "" {
			base := filepath.Base(*path)
			name = strings.TrimSuffix(base, filepath.Ext(base))
		} else {
			name = "mlstoredb"
		}
	}

	opts := db.Options{LightKDF: *light}
	if *key != "" {
		opts.MasterKey = []byte(*key)
	}
	if *machine != "" {
		opts.MachineID = *machine
	}

	var store *db.Store
	var err error
	if *path != "" {
		store, err = db.OpenWithLock(*path, opts)
	} else {
		store = db.New()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	srv := wire.NewServer(store, wire.ServerOptions{
		DBName:   name,
		AuthUser: *user,
		AuthPass: *pass,
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("shutting down...")
		_ = srv.Close()
	}()

	fmt.Printf("mls-server: listening on %s (db=%s path=%s auth=%v)\n",
		*addr, name, orDefault(*path, "<memory>"), *user != "")
	fmt.Println("connect with a MongoDB client (Navicat / Compass / mongosh) using mongodb://<host>:<port>")
	if err := srv.ListenAndServe(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		_ = store.Close()
		os.Exit(1)
	}
	if err := store.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close:", err)
		os.Exit(1)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
