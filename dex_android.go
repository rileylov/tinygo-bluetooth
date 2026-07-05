//go:build android

package bluetooth

import _ "embed"

// gobleDex is the compiled GoBle.java class, loaded at runtime with an
// InMemoryDexClassLoader (see jni_android.c). Regenerate with
// android/build-dex.sh whenever GoBle.java changes.
//
//go:embed android/GoBle.dex
var gobleDex []byte
