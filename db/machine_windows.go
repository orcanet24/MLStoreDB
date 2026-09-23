//go:build windows

package db

import "golang.org/x/sys/windows/registry"

// machineGuid returns the Windows MachineGuid (stable per install).
// Empty string on any registry error (caller falls back to "default").
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
