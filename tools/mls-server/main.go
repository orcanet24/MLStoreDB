package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"mlstoredb/adminweb"
	"mlstoredb/db"
	"mlstoredb/wire"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:28917", "listen address (host:port)")
	web := flag.String("web", "", "admin web console address (e.g. 127.0.0.1:28918; empty = off)")
	path := flag.String("path", "", "database file (.mlstore); empty = start empty, create DBs from web")
	dbName := flag.String("db", "", "database name exposed to clients (default: file base name or mlstoredb)")
	key := flag.String("key", "", "master key (KEK input; required for encrypted files)")
	machine := flag.String("machine", "", "machine id mixed into the KEK (empty = default)")
	user := flag.String("user", "", "wire auth user (SCRAM-SHA-256 gate; empty = no auth)")
	pass := flag.String("pass", "", "wire auth password")
	light := flag.Bool("light-kdf", false, "faster Argon2 params (first creation only)")
	dbDir := flag.String("dbdir", "databases", "directory to auto-discover / create databases")
	resetAuth := flag.Bool("reset-auth", false, "console-only: wipe all credentials (users/roles/sessions) WITHOUT touching data, then exit")
	flag.Parse()

	opts := db.Options{LightKDF: *light}
	if *key != "" {
		opts.MasterKey = []byte(*key)
	} else {
		// Clave por defecto para desarrollo (32 bytes). Cambia DBKEY en el .bat
		// para producción.
		opts.MasterKey = []byte("mlstoredb-default-32-byte-key!!")
	}
	if *machine != "" {
		opts.MachineID = *machine
	}

	// -db <nombre> sin -path: si existe <dbdir>/<nombre>/<nombre>.mlstore se sirve ese
	// archivo — es el que crea la consola web al crear una base de datos. Así el protocolo
	// Mongo y la consola trabajan sobre la MISMA base (y comparten sus credenciales).
	if *path == "" && *dbName != "" && *dbDir != "" {
		if candidate := filepath.Join(*dbDir, *dbName, *dbName+".mlstore"); fileExists(candidate) {
			*path = candidate
			fmt.Printf("db: serving %s (found in -dbdir via -db %s)\n", candidate, *dbName)
		}
	}

	var defStore *db.Store
	var wireDBName string
	if *path != "" {
		name := *dbName
		if name == "" {
			base := filepath.Base(*path)
			name = strings.TrimSuffix(base, filepath.Ext(base))
		}
		wireDBName = name
		store, err := db.OpenWithLock(*path, opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			os.Exit(1)
		}
		defStore = store
	} else {
		// Arrancar vacío: el store se crea en memoria; las colecciones
		// se crean por wire, consola web o comandos de mongosh.
		wireDBName = *dbName
		if wireDBName == "" {
			wireDBName = "mlstoredb"
		}
		defStore = db.New()
	}

	var srv *wire.Server
	if defStore != nil {
		wireDBName := wireDBName
		srv = wire.NewServer(defStore, wire.ServerOptions{
			DBName:   wireDBName,
			AuthUser: *user,
			AuthPass: *pass,
		})
	}

	var adminSrv *adminweb.MultiServer
	if *web != "" {
		// Solo registrar defStore si viene de un -path real (persistente).
		// Si es db.New() vacío, dejar que MultiServer descubra las BDs
		// del directorio databases/ sin contaminar la lista con "default".
		var adminStore *db.Store
		if *path != "" {
			adminStore = defStore
		}
		adminSrv = adminweb.NewMultiServer(adminStore, *dbDir, adminweb.ServerOptions{
			DBName:    wireDBName,
			MasterKey: opts.MasterKey,
		})
		go func() {
			if defStore != nil {
				fmt.Printf("admin console: http://%s (db=%s)\n", *web, wireDBName)
			} else {
				fmt.Printf("admin console: http://%s (empty — create a database)\n", *web)
			}
			if err := adminSrv.ListenAndServe(*web); err != nil && err != net.ErrClosed {
				fmt.Fprintln(os.Stderr, "web:", err)
			}
		}()
	}

	if !*resetAuth {
		printStartupDiagnostics(wireDBName, *path, *dbDir, *user, defStore)
	}

	if *resetAuth {
		if defStore == nil {
			fmt.Println("reset-auth: no database, nothing to do")
			os.Exit(0)
		}
		if err := defStore.ResetAuth(); err != nil {
			fmt.Fprintln(os.Stderr, "reset-auth:", err)
			os.Exit(1)
		}
		if err := defStore.FlushSync(); err != nil {
			fmt.Fprintln(os.Stderr, "flush:", err)
			os.Exit(1)
		}
		if err := defStore.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "close:", err)
			os.Exit(1)
		}
		fmt.Println("reset-auth OK")
		os.Exit(0)
	}

	if srv == nil {
		fmt.Fprintln(os.Stderr, "mls-server: no database. Use -path or create one from the web console.")
		if adminSrv != nil {
			fmt.Println("web console available — create a database to start the wire server.")
		}
		if adminSrv == nil {
			os.Exit(1)
		}
		select {}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("shutting down...")
		_ = srv.Close()
		if adminSrv != nil {
			_ = adminSrv.Close()
		}
	}()

	fmt.Printf("mls-server: listening on %s (db=%s path=%s auth=%v)\n",
		*addr, wireDBName, orDefault(*path, "<memory>"), *user != "")
	fmt.Println("connect with a MongoDB client (Navicat / Compass / mongosh) using mongodb://<host>:<port>")
	if err := srv.ListenAndServe(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		_ = srv.Close()
		os.Exit(1)
	}
	_ = srv.Close()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// dbdirNames lista las bases de -dbdir (subcarpeta <n> con <n>.mlstore dentro).
func dbdirNames(dbDir string) []string {
	if dbDir == "" {
		return nil
	}
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fileExists(filepath.Join(dbDir, e.Name(), e.Name()+".mlstore")) {
			out = append(out, e.Name())
		}
	}
	return out
}

// printStartupDiagnostics avisa de los dos errores de configuración que dejan a los clientes
// Mongo con "invalid credentials" y sin ninguna pista en la consola:
//   - el wire sirve un store distinto de la base que se quiere usar (las bases creadas en
//     -dbdir sólo las sirve la consola web: hay que reiniciar con -db <nombre> o -path);
//   - -user/-pass no coincide con ningún usuario del motor (_users) de esa base.
func printStartupDiagnostics(wireDBName, path, dbDir, user string, store *db.Store) {
	if path == "" {
		fmt.Printf("wire: IN-MEMORY store (%q) - data is not persisted on exit.\n", wireDBName)
	} else {
		fmt.Printf("wire: serving %q -> %s (engine users: %v)\n", wireDBName, path, store.AuthActive())
	}
	if others := otherDBs(dbDir, wireDBName); len(others) > 0 {
		fmt.Printf("dbdir: other databases in %q (web console only): %s\n", dbDir, strings.Join(others, ", "))
		fmt.Printf("dbdir: to serve them over the Mongo protocol, restart with -db <name> -dbdir %q\n", dbDir)
	}
	if !store.AuthActive() {
		return
	}
	users := store.ListUsers()
	names := make([]string, 0, len(users))
	found := false
	for _, u := range users {
		names = append(names, u.Username)
		if user != "" && u.Username == user {
			found = true
		}
	}
	switch {
	case user == "":
		fmt.Fprintln(os.Stderr, "warning: this database has users (RBAC active) and the wire started WITHOUT -user/-pass:")
		fmt.Fprintln(os.Stderr, "         Compass/Navicat/mongosh will not be able to authenticate (they show \"invalid credentials\").")
		fmt.Fprintln(os.Stderr, "         restart with -user <user> -pass <password> using an engine user (web console -> Users).")
	case !found:
		fmt.Fprintf(os.Stderr, "warning: -user %q does not exist in this database (RBAC active): wire logins will fail.\n", user)
		fmt.Fprintf(os.Stderr, "         existing users: %s\n", strings.Join(names, ", "))
	}
}

// otherDBs devuelve las bases de dbDir distintas de la que sirve el wire.
func otherDBs(dbDir, wireDBName string) []string {
	var out []string
	for _, n := range dbdirNames(dbDir) {
		if n != wireDBName {
			out = append(out, n)
		}
	}
	return out
}
