package main

import (
	"fmt"
	"os"
	"path/filepath"

	"mlstoredb/db"
)

func main() {
	dir, err := os.MkdirTemp("", "mlstoredb-smoke-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "smoke.db")

	s, err := db.OpenWithLock(path, db.Options{
		MasterKey: []byte("smoke-master-key-32-bytes-long!!"),
		MachineID: "smoke",
		LightKDF:  true,
	})
	if err != nil {
		panic(err)
	}

	if err := s.EnsureIndex("usuarios", []string{"email"}, true); err != nil {
		panic(err)
	}
	if err := s.Insert("usuarios", db.Document{
		"_id":   "u1",
		"email": "ana@ejemplo.com",
		"perfil": db.Document{"nombre": "Ana"},
	}); err != nil {
		panic(err)
	}

	docs, err := s.Find("usuarios", db.Document{"email": "ana@ejemplo.com"}, nil)
	if err != nil {
		panic(err)
	}
	if len(docs) != 1 {
		panic(fmt.Sprintf("expected 1 doc, got %d", len(docs)))
	}
	if err := s.Close(); err != nil {
		panic(err)
	}

	s2, err := db.OpenWithLock(path, db.Options{
		MasterKey: []byte("smoke-master-key-32-bytes-long!!"),
		MachineID: "smoke",
		LightKDF:  true,
	})
	if err != nil {
		panic(err)
	}
	defer s2.Close()
	got, err := s2.Get("usuarios", "u1")
	if err != nil {
		panic(err)
	}
	fmt.Println("smoke OK:", got["perfil"].(map[string]any)["nombre"])
}
