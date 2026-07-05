#!/usr/bin/env bash
# Regenerate GoBle.dex from GoBle.java. The dex is checked in and embedded into
# the Go package (see goble_dex.go), so run this whenever GoBle.java changes.
#
# Needs a JDK (javac) and the Android SDK build-tools (d8) + a platform android.jar.
# Override locations with ANDROID_SDK / ANDROID_JAR / D8 env vars.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)

SDK=${ANDROID_SDK:-${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/android-sdk}}}
ANDROID_JAR=${ANDROID_JAR:-$(ls -d "$SDK"/platforms/android-*/android.jar 2>/dev/null | sort -V | tail -1)}
D8=${D8:-$(ls "$SDK"/build-tools/*/d8 2>/dev/null | sort -V | tail -1)}

[ -f "$ANDROID_JAR" ] || { echo "android.jar not found (set ANDROID_JAR)"; exit 1; }
[ -x "$D8" ] || { echo "d8 not found (set D8)"; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

javac --release 8 -classpath "$ANDROID_JAR" -d "$work" "$here/GoBle.java"
"$D8" --min-api 26 --lib "$ANDROID_JAR" --output "$work" "$work"/org/tinygo/bluetooth/*.class
cp "$work/classes.dex" "$here/GoBle.dex"
echo "wrote $here/GoBle.dex ($(wc -c < "$here/GoBle.dex") bytes)"
