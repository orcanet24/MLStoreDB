package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveMachineIDGeneratesAndStaysStable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.mlstore")

	id1, err := ResolveMachineID(dbPath, "")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if len(id1) != 26 {
		t.Fatalf("generated id %q is not a canonical 26-char ULID", id1)
	}

	// La segunda llamada debe devolver el mismo id — sin regenerarlo.
	id2, err := ResolveMachineID(dbPath, "")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("identity not stable: %q vs %q", id1, id2)
	}

	// Y el archivo debe existir junto a la ruta de la base de datos con el mismo contenido.
	data, err := os.ReadFile(filepath.Join(dir, InstallIDFileName))
	if err != nil {
		t.Fatalf("install id file missing: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != id1 {
		t.Fatalf("file content %q != resolved id %q", got, id1)
	}
}

func TestResolveMachineIDExplicitWinsWithoutFileIO(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.mlstore")

	id, err := ResolveMachineID(dbPath, "Mi-Instalar_ID.01")
	if err != nil {
		t.Fatalf("explicit resolve: %v", err)
	}
	if id != "Mi-Instalar_ID.01" {
		t.Fatalf("explicit id not honored: %q", id)
	}
	// El modo explícito no debe crear el archivo de id de instalación.
	if _, err := os.Stat(filepath.Join(dir, InstallIDFileName)); !os.IsNotExist(err) {
		t.Fatalf("explicit mode created install id file (err=%v)", err)
	}
	// Se rechazan los caracteres inválidos.
	if _, err := ResolveMachineID(dbPath, "bad id con espacios!"); err == nil {
		t.Fatal("expected error for invalid explicit MachineID")
	}
}

func TestResolveMachineIDSharedPerDirectory(t *testing.T) {
	dir := t.TempDir()
	id1, err := ResolveMachineID(filepath.Join(dir, "a.mlstore"), "")
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	id2, err := ResolveMachineID(filepath.Join(sub, "b.mlstore"), "")
	if err != nil {
		t.Fatal(err)
	}
	if id2 == id1 {
		t.Fatalf("different directories must not share identity")
	}

	id3, err := ResolveMachineID(filepath.Join(dir, "other.mlstore"), "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id3 {
		t.Fatalf("same-directory databases must share identity: %q vs %q", id1, id3)
	}
}

func TestResolveMachineIDEmptyFileRegenerates(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.mlstore")
	p := filepath.Join(dir, InstallIDFileName)

	if err := os.WriteFile(p, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveMachineID(dbPath, ""); err == nil {
		t.Fatal("empty install id file must be an error, not silently regenerated")
	}

	// Los caracteres corruptos también son un error (nunca se re-vincula la KEK en silencio).
	if err := os.WriteFile(p, []byte("no valid chars !!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveMachineID(dbPath, ""); err == nil {
		t.Fatal("corrupt install id file must error")
	}
}

func TestResolveMachineIDConcurrentCreation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.mlstore")

	const n = 8
	ids := make(chan string, n)
	errs := make(chan error, n)
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			id, err := ResolveMachineID(dbPath, "")
			ids <- id
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		<-done
		if err := <-errs; err != nil {
			t.Fatalf("concurrent resolve: %v", err)
		}
	}
	close(ids)
	first := <-ids
	for id := range ids {
		if id != first {
			t.Fatalf("concurrent creators converged to different ids: %q vs %q", first, id)
		}
	}
}

func TestOpenWithInstallIDRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.mlstore")

	// Momento de configuración (instalador): resolver la identidad de instalación una vez.
	installID, err := ResolveMachineID(path, "")
	if err != nil {
		t.Fatal(err)
	}
	opts := func() Options {
		return Options{MasterKey: []byte("master-key-32-bytes-long!!!!!!!"), MachineID: installID, LightKDF: true}
	}

	s, err := OpenWithLock(path, opts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("cfg", Document{"_id": "k1", "v": "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// La misma instalación reabre sin problemas.
	s2, err := Open(path, opts())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Get("cfg", "k1"); err != nil || got["v"] != "hello" {
		t.Fatalf("reopen with install id: got=%v err=%v", got, err)
	}
	s2.Close()

	// Simulación de robo: el atacante copia el archivo de la base de datos al directorio
	// de datos de OTRA máquina. Esa máquina resuelve su propia identidad de instalación
	// (o su MachineGuid); la KEK nunca coincide, así que el descifrado es imposible.
	otherDir := t.TempDir()
	copied := filepath.Join(otherDir, "app.mlstore")
	if err := copyFile(path, copied); err != nil {
		t.Fatal(err)
	}
	// Resolver la identidad de la otra máquina (generada allí, no aquí).
	otherID, err := ResolveMachineID(copied, "")
	if err != nil {
		t.Fatal(err)
	}
	stolen := opts()
	stolen.MachineID = otherID
	if _, err := Open(copied, stolen); err == nil {
		t.Fatal("db stolen to another installation opened — binding broken")
	}
	// Incluso el respaldo del atacante con su máster conocido + su propio MachineGuid falla.
	if _, err := Open(copied, Options{MasterKey: []byte("master-key-32-bytes-long!!!!!!!"), LightKDF: true}); err == nil {
		t.Fatal("stolen db opened with master-only options — binding broken")
	}
	// ...y el mismo archivo sigue abriéndose en su instalación original.
	if _, err := Open(path, opts()); err != nil {
		t.Fatalf("original installation must keep opening the db: %v", err)
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

func TestOpenWithEmptyMachineIDFallsBackToInstallIDFile(t *testing.T) {
	// Documenta el conexionado previsto: Options.MachineID vacío significa "derivar del
	// MachineGuid"; el valor por defecto a nivel de aplicación puede resolver antes el id
	// de instalación. Aquí solo comprobamos que la salida de ResolveMachineID se acepta tal cual.
	dir := t.TempDir()
	path := filepath.Join(dir, "app.mlstore")
	id, err := ResolveMachineID(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenWithLock(path, Options{MasterKey: []byte("master-key-32-bytes-long!!!!!!!"), MachineID: id, LightKDF: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("t", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
