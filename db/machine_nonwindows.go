//go:build !windows

package db

// machineGuid no está disponible fuera de Windows; la KEK recurre a "default"
// salvo que Options.MachineID se indique explícitamente.
func machineGuid() string {
	return ""
}
