//go:build android

package bluetooth

// Characteristic is a single characteristic in a local GATT service. The Android
// backend is central-role only, so this exists just to satisfy the shared
// peripheral (gatts.go) types; the GATT server is not implemented.
type Characteristic struct {
	permissions CharacteristicPermissions
}
