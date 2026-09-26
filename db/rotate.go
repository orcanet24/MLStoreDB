package db

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// RotateKeys vuelve a clavar el archivo cifrado en s.path con una nueva clave maestra
// y/o id de máquina SIN volver a cifrar la carga útil (operación de cabecera estilo D16).
//
// Por qué funciona: la carga útil va sellada con la DEK del archivo, que nunca cambia;
// solo el *envolvimiento* de la DEK depende de la KEK (master ⊕ machineID ⊕ salt). Así que
// rotar = rederivar la KEK con el material nuevo, volver a envolver la MISMA DEK y reescribir
// los bytes de cabecera+envoltorio copiando el criptograma de la carga útil sin tocarlo.
// El coste es O(1) independientemente del tamaño de la base de datos (~µs, una reescritura
// del archivo para la cabecera de 152B más la copia de la carga útil que hace el sistema de archivos).
//
// Seguridad:
//   - El almacén debe estar abierto sobre s.path, limpio (haz Flush antes) y no cerrado.
//   - Rotar con datos sucios dejaría el archivo en disco desfasado respecto a la RAM; se rechaza.
//   - Se serializa con Flush/Snapshot mediante flushMu; los lectores/escritores siguen
//     trabajando con la DEK que está en RAM, que no cambia.
//   - Nonce GCM nuevo para el envoltorio; el salt nuevo evita reutilizar el nonce del
//     AEAD del envoltorio (wrapNonce = salt[:12]).
//   - Se verifica primero contra la KEK ANTIGUA; una clave actual incorrecta falla con
//     ErrCorrupt y el archivo queda intacto.
//   - El archivo rotado se sincroniza de forma duradera (fsync) antes de retornar.
//
// Pasa la cadena vacía en un campo para conservar el valor actual. El almacén en RAM
// adopta las nuevas credenciales, así que los Flush posteriores siguen usándolas.
//
// Usos típicos: rotación de la clave maestra del proveedor (plan §3), mover la base de
// datos a otra máquina (junto con ResolveMachineID) y re-clavado tras un compromiso.
func (s *Store) RotateKeys(newMaster []byte, newMachineID string) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.RLock()
	path, closed, dirty := s.path, s.closed, s.dirty
	s.mu.RUnlock()
	if path == "" {
		return errors.New("db: RotateKeys requires a file-backed store")
	}
	if closed {
		return errors.New("db: RotateKeys on closed store")
	}
	if dirty {
		return errors.New("db: RotateKeys requires a clean store (Flush first)")
	}

	// Instantánea del material criptográfico actual + id de máquina efectivo.
	s.mu.RLock()
	curMaster := append([]byte(nil), s.opts.MasterKey...)
	curMachine := append([]byte(nil), s.machine...)
	t, m, p := s.kdfTime, s.kdfMem, s.kdfPar
	dek := append([]byte(nil), s.dek...)
	created := s.created
	schemaVersion := s.schemaVersion
	s.mu.RUnlock()
	if len(curMaster) == 0 || len(dek) == 0 || t == 0 {
		return errors.New("db: RotateKeys on uninitialized crypto material")
	}
	if len(newMaster) == 0 {
		newMaster = curMaster
	}
	if newMachineID == "" {
		newMachineID = string(curMachine)
	}

	// Leer, interpretar y autenticar el archivo actual (la KEK antigua debe verificar).
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) < headerSize+16 {
		return ErrCorrupt
	}
	h, err := parseHeader(data)
	if err != nil {
		return err
	}
	oldKEK := deriveKEK(curMaster, curMachine, h.KDFSalt[:], t, m, p)
	if !h.verifyHMAC(oldKEK) {
		return ErrCorrupt
	}
	oldDEK, err := openGCM(oldKEK, h.KDFSalt[:12], h.DEKWrapped[:], nil)
	if err != nil || len(oldDEK) != 32 {
		return ErrCorrupt
	}

	// Salt nuevo (nonce del nuevo envoltorio = salt[:12]; el nonce de la carga sigue en la cabecera).
	newSalt, err := randomBytes(16)
	if err != nil {
		return err
	}
	newKEK := deriveKEK(newMaster, []byte(newMachineID), newSalt, t, m, p)
	wrapped, err := sealGCM(newKEK, newSalt[:12], oldDEK, nil)
	if err != nil {
		return err
	}
	if len(wrapped) != 48 {
		return fmt.Errorf("db: unexpected wrapped DEK size %d", len(wrapped))
	}

	nh := &header{
		FormatVer:     formatVersion,
		Flags:         h.Flags, // los bytes de la carga se copian tal cual
		SchemaVersion: schemaVersion,
		CreatedAt:     created,
		UpdatedAt:     uint64(time.Now().Unix()),
		KDFTime:       t,
		KDFMemKiB:     m,
		KDFPar:        p,
	}
	copy(nh.KDFSalt[:], newSalt)
	copy(nh.NonceBase[:], h.NonceBase[:]) // el nonce GCM de la carga no cambia
	copy(nh.DEKWrapped[:], wrapped)
	nh.setHMAC(newKEK)

	out := make([]byte, 0, len(data))
	out = append(out, nh.marshalBody()...)
	out = append(out, nh.HeaderHMAC[:]...)
	out = append(out, data[headerSize:]...) // criptograma copiado tal cual: rotación O(1)
	if err := atomicReplace(path, out); err != nil {
		return err
	}
	if err := syncPath(path); err != nil {
		return err
	}

	// Adoptar las nuevas credenciales en RAM (opts.MasterKey + máquina) para que el próximo
	// Flush vuelva a envolver con la nueva KEK y Open() desde un proceso nuevo coincida.
	// La reescritura de la cabecera es O(1); los registros del cuerpo van sellados con la DEK y siguen válidos.
	s.mu.Lock()
	s.opts.MasterKey = append([]byte(nil), newMaster...)
	s.opts.MachineID = newMachineID
	s.machine = append([]byte(nil), newMachineID...)
	copy(s.kdfSalt[:], newSalt)
	s.kek = append([]byte(nil), newKEK...)
	s.mu.Unlock()
	return nil
}
