// JNI implementation for the Android backend. See jni_android.h for the API.
//
// Everything runs Go -> C -> JNI -> Java (the GoBle class); Java never calls
// back into Go. Java's BluetoothGattCallback events are queued on the Java side
// and drained by Go through goble_poll(). That keeps this file free of native
// method registration and foreign-thread Go entry points.

#include "jni_android.h"

#include <android/log.h>
#include <jni.h>
#include <stdlib.h>
#include <string.h>

#define TAG "goble"
#define LOGI(...) __android_log_print(ANDROID_LOG_INFO, TAG, __VA_ARGS__)
#define LOGE(...) __android_log_print(ANDROID_LOG_ERROR, TAG, __VA_ARGS__)

void goble_log(const char *msg) {
    __android_log_write(ANDROID_LOG_INFO, TAG, msg);
}

static JavaVM *g_vm;
static jobject g_goble; // global ref to the GoBle instance
static jclass g_cls;    // global ref to the GoBle class

static jmethodID m_isEnabled;
static jmethodID m_pollEvent;
static jmethodID m_startScan;
static jmethodID m_stopScan;
static jmethodID m_connect;
static jmethodID m_disconnect;
static jmethodID m_discover;
static jmethodID m_serviceUuids;
static jmethodID m_charUuids;
static jmethodID m_write;
static jmethodID m_read;
static jmethodID m_setNotify;

// env_get returns a JNIEnv for the current thread, attaching it to the VM if
// needed. *attached reports whether we attached (caller must env_put to detach).
static JNIEnv *env_get(int *attached) {
    JNIEnv *env = NULL;
    *attached = 0;
    if (g_vm == NULL) {
        return NULL;
    }
    jint r = (*g_vm)->GetEnv(g_vm, (void **)&env, JNI_VERSION_1_6);
    if (r == JNI_OK) {
        return env;
    }
    if (r == JNI_EDETACHED &&
        (*g_vm)->AttachCurrentThread(g_vm, &env, NULL) == JNI_OK) {
        *attached = 1;
        return env;
    }
    return NULL;
}

static void env_put(int attached) {
    if (attached) {
        (*g_vm)->DetachCurrentThread(g_vm);
    }
}

// exc logs and clears a pending Java exception; returns 1 if one was pending.
static int exc(JNIEnv *env) {
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionDescribe(env); // -> logcat
        (*env)->ExceptionClear(env);
        return 1;
    }
    return 0;
}

int goble_init(uintptr_t vm, uintptr_t activity, const void *dex, int dex_len) {
    g_vm = (JavaVM *)vm;
    jobject act = (jobject)activity;
    int attached, step = 0;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -1;
    }

#define STEP(n, bad) do { if (exc(env) || (bad)) { step = -(n); goto out; } } while (0)

    // parentLoader = activity.getClassLoader()
    jclass actCls = (*env)->GetObjectClass(env, act);
    jmethodID getCL = (*env)->GetMethodID(env, actCls, "getClassLoader",
                                          "()Ljava/lang/ClassLoader;");
    STEP(2, !getCL);
    jobject parentLoader = (*env)->CallObjectMethod(env, act, getCL);
    STEP(3, !parentLoader);

    // Keep the dex bytes alive for the lifetime of the process (the direct
    // ByteBuffer does not own them). 11KB, freed never — intentional.
    void *dexCopy = malloc(dex_len);
    STEP(4, !dexCopy);
    memcpy(dexCopy, dex, dex_len);
    jobject bb = (*env)->NewDirectByteBuffer(env, dexCopy, dex_len);
    STEP(5, !bb);

    // loader = new InMemoryDexClassLoader(bb, parentLoader)  (API 26+)
    jclass imdcl = (*env)->FindClass(env, "dalvik/system/InMemoryDexClassLoader");
    STEP(6, !imdcl);
    jmethodID imdclCtor = (*env)->GetMethodID(env, imdcl, "<init>",
        "(Ljava/nio/ByteBuffer;Ljava/lang/ClassLoader;)V");
    STEP(7, !imdclCtor);
    jobject loader = (*env)->NewObject(env, imdcl, imdclCtor, bb, parentLoader);
    STEP(8, !loader);

    // cls = loader.loadClass("org.tinygo.bluetooth.GoBle")
    jclass loaderCls = (*env)->GetObjectClass(env, loader);
    jmethodID loadClass = (*env)->GetMethodID(env, loaderCls, "loadClass",
        "(Ljava/lang/String;)Ljava/lang/Class;");
    STEP(9, !loadClass);
    jstring cname = (*env)->NewStringUTF(env, "org.tinygo.bluetooth.GoBle");
    jobject clsObj = (*env)->CallObjectMethod(env, loader, loadClass, cname);
    STEP(10, !clsObj);
    g_cls = (jclass)(*env)->NewGlobalRef(env, clsObj);

    // goble = new GoBle(activity)
    jmethodID ctor = (*env)->GetMethodID(env, g_cls, "<init>",
        "(Landroid/content/Context;)V");
    STEP(11, !ctor);
    jobject goble = (*env)->NewObject(env, g_cls, ctor, act);
    STEP(12, !goble);
    g_goble = (*env)->NewGlobalRef(env, goble);

    // Cache the method IDs.
    m_isEnabled    = (*env)->GetMethodID(env, g_cls, "isEnabled", "()Z");
    m_pollEvent    = (*env)->GetMethodID(env, g_cls, "pollEvent", "(J)[B");
    m_startScan    = (*env)->GetMethodID(env, g_cls, "startScan", "()V");
    m_stopScan     = (*env)->GetMethodID(env, g_cls, "stopScan", "()V");
    m_connect      = (*env)->GetMethodID(env, g_cls, "connect", "(Ljava/lang/String;)Z");
    m_disconnect   = (*env)->GetMethodID(env, g_cls, "disconnect", "(Ljava/lang/String;)V");
    m_discover     = (*env)->GetMethodID(env, g_cls, "discoverServices", "(Ljava/lang/String;)Z");
    m_serviceUuids = (*env)->GetMethodID(env, g_cls, "getServiceUuids", "(Ljava/lang/String;)Ljava/lang/String;");
    m_charUuids    = (*env)->GetMethodID(env, g_cls, "getCharUuids", "(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;");
    m_write        = (*env)->GetMethodID(env, g_cls, "writeCharacteristic", "(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;[BZ)I");
    m_read         = (*env)->GetMethodID(env, g_cls, "readCharacteristic", "(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;)Z");
    m_setNotify    = (*env)->GetMethodID(env, g_cls, "setNotify", "(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;Z)I");
    STEP(13, !m_isEnabled || !m_pollEvent || !m_startScan || !m_stopScan ||
            !m_connect || !m_disconnect || !m_discover || !m_serviceUuids ||
            !m_charUuids || !m_write || !m_read || !m_setNotify);

    LOGI("goble_init ok");
    step = 0;
out:
    if (step != 0) {
        LOGE("goble_init failed at step %d", -step);
    }
    env_put(attached);
    return step;
}

// ---- permissions ----

static const char *const BLE_PERMS[] = {
    "android.permission.BLUETOOTH_SCAN",
    "android.permission.BLUETOOTH_CONNECT",
};
static const int BLE_PERMS_N = 2;

int goble_permissions_granted(uintptr_t activity) {
    jobject act = (jobject)activity;
    int attached, ok = 1;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return 0;
    }
    jclass cls = (*env)->GetObjectClass(env, act);
    jmethodID check = (*env)->GetMethodID(env, cls, "checkSelfPermission",
                                          "(Ljava/lang/String;)I");
    if (exc(env) || !check) {
        env_put(attached);
        return 0;
    }
    for (int i = 0; i < BLE_PERMS_N; i++) {
        jstring p = (*env)->NewStringUTF(env, BLE_PERMS[i]);
        jint st = (*env)->CallIntMethod(env, act, check, p);
        (*env)->DeleteLocalRef(env, p);
        if (exc(env) || st != 0) { // 0 == PERMISSION_GRANTED
            ok = 0;
            break;
        }
    }
    env_put(attached);
    return ok;
}

int goble_request_permissions(uintptr_t activity) {
    if (goble_permissions_granted(activity)) {
        return 1;
    }
    jobject act = (jobject)activity;
    int attached, ret = -1;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -1;
    }
    jclass cls = (*env)->GetObjectClass(env, act);
    jmethodID req = (*env)->GetMethodID(env, cls, "requestPermissions",
                                        "([Ljava/lang/String;I)V");
    if (exc(env) || !req) {
        env_put(attached);
        return -2;
    }
    jclass strCls = (*env)->FindClass(env, "java/lang/String");
    jobjectArray arr = (*env)->NewObjectArray(env, BLE_PERMS_N, strCls, NULL);
    if (exc(env) || !arr) {
        env_put(attached);
        return -3;
    }
    for (int i = 0; i < BLE_PERMS_N; i++) {
        jstring p = (*env)->NewStringUTF(env, BLE_PERMS[i]);
        (*env)->SetObjectArrayElement(env, arr, i, p);
        (*env)->DeleteLocalRef(env, p);
    }
    (*env)->CallVoidMethod(env, act, req, arr, 9931 /* request code */);
    ret = exc(env) ? -4 : 0; // 0 == dialog shown, caller retries
    env_put(attached);
    return ret;
}

// ---- simple call wrappers ----

int goble_is_enabled(void) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env || !g_goble) {
        return 0;
    }
    jboolean b = (*env)->CallBooleanMethod(env, g_goble, m_isEnabled);
    int r = exc(env) ? 0 : (b ? 1 : 0);
    env_put(attached);
    return r;
}

int goble_start_scan(void) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -1;
    }
    (*env)->CallVoidMethod(env, g_goble, m_startScan);
    int r = exc(env) ? -1 : 0;
    env_put(attached);
    return r;
}

int goble_stop_scan(void) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -1;
    }
    (*env)->CallVoidMethod(env, g_goble, m_stopScan);
    int r = exc(env) ? -1 : 0;
    env_put(attached);
    return r;
}

// call_str_bool invokes a (String)Z method with one C-string argument.
static int call_str_bool(jmethodID m, const char *arg) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -1;
    }
    jstring s = (*env)->NewStringUTF(env, arg);
    jboolean b = (*env)->CallBooleanMethod(env, g_goble, m, s);
    (*env)->DeleteLocalRef(env, s);
    int r = exc(env) ? -1 : (b ? 0 : -2);
    env_put(attached);
    return r;
}

int goble_connect(const char *addr) {
    return call_str_bool(m_connect, addr);
}

int goble_discover_services(const char *addr) {
    return call_str_bool(m_discover, addr);
}

int goble_disconnect(const char *addr) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -1;
    }
    jstring s = (*env)->NewStringUTF(env, addr);
    (*env)->CallVoidMethod(env, g_goble, m_disconnect, s);
    (*env)->DeleteLocalRef(env, s);
    int r = exc(env) ? -1 : 0;
    env_put(attached);
    return r;
}

// call_string invokes a method returning a Java String, copying it to a fresh
// C string (caller frees). NULL on error.
static char *call_string(jmethodID m, const char *a1, const char *a2) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return NULL;
    }
    jstring s1 = (*env)->NewStringUTF(env, a1);
    jstring s2 = a2 ? (*env)->NewStringUTF(env, a2) : NULL;
    jstring js = a2 ? (jstring)(*env)->CallObjectMethod(env, g_goble, m, s1, s2)
                    : (jstring)(*env)->CallObjectMethod(env, g_goble, m, s1);
    char *out = NULL;
    if (!exc(env) && js) {
        const char *utf = (*env)->GetStringUTFChars(env, js, NULL);
        if (utf) {
            out = strdup(utf);
            (*env)->ReleaseStringUTFChars(env, js, utf);
        }
        (*env)->DeleteLocalRef(env, js);
    }
    (*env)->DeleteLocalRef(env, s1);
    if (s2) {
        (*env)->DeleteLocalRef(env, s2);
    }
    env_put(attached);
    return out;
}

char *goble_service_uuids(const char *addr) {
    return call_string(m_serviceUuids, addr, NULL);
}

char *goble_char_uuids(const char *addr, const char *service) {
    return call_string(m_charUuids, addr, service);
}

int goble_write(const char *addr, const char *service, const char *chr,
                const void *data, int len, int with_response) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -100;
    }
    jstring ja = (*env)->NewStringUTF(env, addr);
    jstring js = (*env)->NewStringUTF(env, service);
    jstring jc = (*env)->NewStringUTF(env, chr);
    jbyteArray arr = (*env)->NewByteArray(env, len);
    (*env)->SetByteArrayRegion(env, arr, 0, len, (const jbyte *)data);
    jint r = (*env)->CallIntMethod(env, g_goble, m_write, ja, js, jc, arr,
                                   (jboolean)(with_response ? 1 : 0));
    if (exc(env)) {
        r = -100;
    }
    (*env)->DeleteLocalRef(env, ja);
    (*env)->DeleteLocalRef(env, js);
    (*env)->DeleteLocalRef(env, jc);
    (*env)->DeleteLocalRef(env, arr);
    env_put(attached);
    return r;
}

int goble_read(const char *addr, const char *service, const char *chr) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -1;
    }
    jstring ja = (*env)->NewStringUTF(env, addr);
    jstring js = (*env)->NewStringUTF(env, service);
    jstring jc = (*env)->NewStringUTF(env, chr);
    jboolean b = (*env)->CallBooleanMethod(env, g_goble, m_read, ja, js, jc);
    int r = exc(env) ? -1 : (b ? 0 : -2);
    (*env)->DeleteLocalRef(env, ja);
    (*env)->DeleteLocalRef(env, js);
    (*env)->DeleteLocalRef(env, jc);
    env_put(attached);
    return r;
}

int goble_set_notify(const char *addr, const char *service, const char *chr, int enable) {
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env) {
        return -100;
    }
    jstring ja = (*env)->NewStringUTF(env, addr);
    jstring js = (*env)->NewStringUTF(env, service);
    jstring jc = (*env)->NewStringUTF(env, chr);
    jint r = (*env)->CallIntMethod(env, g_goble, m_setNotify, ja, js, jc,
                                   (jboolean)(enable ? 1 : 0));
    if (exc(env)) {
        r = -100;
    }
    (*env)->DeleteLocalRef(env, ja);
    (*env)->DeleteLocalRef(env, js);
    (*env)->DeleteLocalRef(env, jc);
    env_put(attached);
    return r;
}

void *goble_poll(long timeout_ms, int *out_len) {
    *out_len = 0;
    int attached;
    JNIEnv *env = env_get(&attached);
    if (!env || !g_goble) {
        return NULL;
    }
    jbyteArray arr = (jbyteArray)(*env)->CallObjectMethod(env, g_goble,
                                                          m_pollEvent, (jlong)timeout_ms);
    void *buf = NULL;
    if (!exc(env) && arr) {
        jsize n = (*env)->GetArrayLength(env, arr);
        if (n > 0) {
            buf = malloc(n);
            if (buf) {
                (*env)->GetByteArrayRegion(env, arr, 0, n, (jbyte *)buf);
                *out_len = (int)n;
            }
        }
        (*env)->DeleteLocalRef(env, arr);
    }
    env_put(attached);
    return buf;
}
