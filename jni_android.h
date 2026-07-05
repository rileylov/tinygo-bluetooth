// JNI glue for the Android backend. Every function here is called from Go (cgo)
// and does its own thread attach/detach, so it is safe to call from any
// goroutine. All the Android BLE work lives in the embedded GoBle.java class;
// this file just loads it and forwards calls. See jni_android.c.
#ifndef GOBLE_JNI_ANDROID_H
#define GOBLE_JNI_ANDROID_H

#include <stdint.h>

// goble_init loads the embedded dex via InMemoryDexClassLoader, constructs
// GoBle(activity) and caches its method IDs. vm/activity are the pointers from
// slint.AndroidJavaVM()/AndroidActivity(). Returns 0 on success or a negative
// step code (see the STEP macros in jni_android.c) on failure.
int goble_init(uintptr_t vm, uintptr_t activity, const void *dex, int dex_len);

// goble_request_permissions asks the activity for the BLE runtime permissions.
// Returns 1 if all are already granted, 0 if the system dialog was shown (call
// again after the user responds), or a negative step code on failure.
int goble_request_permissions(uintptr_t activity);

// goble_permissions_granted returns 1 if all BLE permissions are currently
// granted, 0 otherwise.
int goble_permissions_granted(uintptr_t activity);

int goble_is_enabled(void);

// goble_log writes a line to logcat under the "goble" tag.
void goble_log(const char *msg);

int goble_start_scan(void);
int goble_stop_scan(void);
int goble_connect(const char *addr);
int goble_disconnect(const char *addr);
int goble_discover_services(const char *addr);

// The *_uuids functions return a malloc'd, comma-separated, NUL-terminated
// string (empty string if none). The caller must free() it. NULL on error.
char *goble_service_uuids(const char *addr);
char *goble_char_uuids(const char *addr, const char *service);

int goble_write(const char *addr, const char *service, const char *chr,
                const void *data, int len, int with_response);
int goble_read(const char *addr, const char *service, const char *chr);
int goble_set_notify(const char *addr, const char *service, const char *chr, int enable);

// goble_poll blocks up to timeout_ms for the next event. On an event it returns
// a malloc'd buffer (caller frees) and sets *out_len; on timeout it returns NULL
// with *out_len == 0.
void *goble_poll(long timeout_ms, int *out_len);

#endif // GOBLE_JNI_ANDROID_H
