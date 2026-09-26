//go:build windows

package db

import "golang.org/x/sys/windows/registry"

// machineGuid devuelve el MachineGuid de Windows (estable por instalación).
// Devuelve cadena vacía ante cualquier error del registro (el llamador recurre a "default").
func machineGuid() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	val, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return val
}
