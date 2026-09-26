package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// InstallIDFileName es el archivo de identidad por instalación. Vive JUNTO AL archivo
// de la base de datos (nunca dentro de él) y contiene un ULID aleatorio generado en el
// primer uso. Pasarlo como Options.MachineID liga la KEK del archivo a esta instalación,
// así que un .mlstore copiado no se puede descifrar en otro sitio aunque se disponga del
// material de la clave maestra.
const InstallIDFileName = "mlstoredb.machineid"

const maxMachineIDLen = 256

// validMachineID acepta letras, dígitos y - _ . (los ULID canónicos son válidos).
func validMachineID(s string) bool {
	if s == "" || len(s) > maxMachineIDLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func installIDPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), InstallIDFileName)
}

// ResolveMachineID devuelve la identidad de máquina que hay que usar como
// Options.MachineID para la base de datos en dbPath:
//
//  1. si se indica una explícita no vacía → se valida y se devuelve tal cual (tests /
//     anulación explícita; no se lee ni se escribe el archivo de id de instalación).
//  2. en caso contrario, el archivo de id de instalación junto a dbPath: si existe, se
//     valida y se devuelve (estable entre reinicios y actualizaciones); si no, se genera
//     un ULID aleatorio nuevo, se persiste de forma atómica y se devuelve.
//
// Llámalo una vez en la instalación/configuración, pasa el resultado como
// Options.MachineID en todos los Open/OpenWithLock posteriores de esa base de datos y
// registra el valor en el servidor de licencias (es a la vez el vínculo por cliente y la
// copia de recuperación si alguna vez se pierde el archivo de identidad).
//
// Las bases de datos existentes creadas con el vínculo MachineGuid por defecto siguen
// abriéndose con Options.MachineID vacío — no cambies un archivo existente a un id de
// instalación sin un plan de rotación de claves o reescritura.
//
// Varios archivos de base de datos en el mismo directorio comparten intencionadamente
// una única identidad de instalación (una identidad por directorio de datos).
func ResolveMachineID(dbPath, explicit string) (string, error) {
	if explicit != "" {
		if !validMachineID(explicit) {
			return "", errors.New("db: invalid explicit MachineID (allowed: letters, digits, - _ .)")
		}
		return explicit, nil
	}
	p := installIDPath(dbPath)
	data, err := os.ReadFile(p)
	if err == nil {
		id := strings.TrimSpace(string(data))
		switch {
		case id == "":
			// Vacío puede significar (a) un creador concurrente aún entre el O_EXCL y el
			// WriteString, o (b) un archivo truncado o corrupto. Se reintenta brevemente
			// para (a); se relee cuando el create→write se estabiliza.
			for attempt := 0; attempt < 8; attempt++ {
				time.Sleep(2 * time.Millisecond)
				data, err = os.ReadFile(p)
				if err != nil {
					break
				}
				id = strings.TrimSpace(string(data))
				if id != "" {
					break
				}
			}
			if id == "" {
				return "", errors.New("db: install id file is empty: " + p)
			}
			if !validMachineID(id) {
				return "", errors.New("db: install id file has invalid characters: " + p)
			}
			return id, nil
		case !validMachineID(id):
			return "", errors.New("db: install id file has invalid characters: " + p)
		}
		return id, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// Primer uso de esta instalación: crear de forma exclusiva para que se genere
	// exactamente una identidad, incluso con creadores concurrentes. Los que pierden
	// adoptan el id del ganador (un breve bucle de lectura cubre la ventana create→write).
	id := ulid.Make().String()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, werr := f.WriteString(id + "\n")
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return "", errors.New("db: cannot write install id file: " + p)
		}
		return id, nil
	}
	if os.IsExist(err) {
		for attempt := 0; attempt < 5; attempt++ {
			data, rerr := os.ReadFile(p)
			if rerr == nil {
				if v := strings.TrimSpace(string(data)); validMachineID(v) {
					return v, nil
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		return "", errors.New("db: install id file created but unreadable: " + p)
	}
	// Sistema de archivos sin soporte de O_EXCL: reemplazo de último recurso + bucle de convergencia.
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := atomicReplace(p, []byte(id+"\n")); err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Millisecond)
			continue
		}
		if data, rerr := os.ReadFile(p); rerr == nil {
			if v := strings.TrimSpace(string(data)); validMachineID(v) {
				return v, nil
			}
		}
		return id, nil
	}
	return "", lastErr
}
