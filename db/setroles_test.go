package db

import (
	"errors"
	"testing"
)

// TestSetUserRoles verifica el reemplazo de roles in situ (API de administración M9):
// se conservan el hash de contraseña y los campos extra, se aplica la validación de roles
// y el cambio surte efecto en el siguiente Authenticate.
func TestSetUserRoles(t *testing.T) {
	s := bootstrap(t)

	// camino feliz: bob reader -> admin+reader
	if err := s.SetUserRoles("bob", []string{"admin", "reader"}); err != nil {
		t.Fatal(err)
	}
	users := s.ListUsers()
	var bob *UserInfo
	for i := range users {
		if users[i].Username == "bob" {
			bob = &users[i]
		}
	}
	if bob == nil {
		t.Fatal("bob missing after SetUserRoles")
	}
	if len(bob.Roles) != 2 || bob.Roles[0] != "admin" || bob.Roles[1] != "reader" {
		t.Errorf("bob roles = %v, want [admin reader]", bob.Roles)
	}

	// regresión (bug de M9d): el cambio de rol NO debe reiniciar la contraseña
	if _, err := s.Authenticate("bob", "secret-reader-1"); err != nil {
		t.Errorf("original password must keep working: %v", err)
	}
	if _, err := s.Authenticate("bob", "placeholder1"); err == nil {
		t.Error("placeholder1 must not authenticate after SetUserRoles")
	}

	// los nuevos roles se respetan al iniciar sesión: bob ya puede escribir en "q" por el comodín de admin
	sess, err := s.Authenticate("bob", "secret-reader-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Insert("q", Document{"_id": "x1"}); err != nil {
		t.Errorf("admin role must allow write on q: %v", err)
	}

	// usuario desconocido
	if err := s.SetUserRoles("nobody", []string{"admin"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user = %v, want ErrNotFound", err)
	}
	// rol desconocido
	if err := s.SetUserRoles("bob", []string{"ghost"}); err == nil {
		t.Error("unknown role must error")
	}
	// se permiten roles vacíos (usuario sin permisos)
	if err := s.SetUserRoles("bob", nil); err != nil {
		t.Fatal(err)
	}
	users = s.ListUsers() // ListUsers no tiene orden determinista: buscar por nombre
	for i := range users {
		if users[i].Username == "bob" {
			if got := users[i].Roles; len(got) != 0 {
				t.Errorf("roles after clearing = %v, want empty", got)
			}
		}
	}
}
