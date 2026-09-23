//go:build !windows

package db

// machineGuid is unavailable off Windows; KEK falls back to "default"
// unless Options.MachineID is set explicitly.
func machineGuid() string {
	return ""
}
