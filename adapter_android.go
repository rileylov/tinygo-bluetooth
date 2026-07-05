//go:build android

// Android backend for tinygo-bluetooth. Central role (scan/connect/GATT client)
// only. All Android BLE work is done in the embedded GoBle.java class over JNI;
// this file drives it and pumps its event queue. See jni_android.c / GoBle.java.
//
// The design is deliberately one-directional: Go calls into Java for every
// action, and a single goroutine polls Java's event queue. Nothing calls back
// into Go from a Java/binder thread, which keeps the JNI simple and correct.

package bluetooth

/*
#cgo LDFLAGS: -llog
#include <stdlib.h>
#include "jni_android.h"
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// ---- JNI pointers, supplied by the host app (e.g. via go-slint) ----

var (
	androidVM       uintptr
	androidActivity uintptr
)

// SetAndroidJNI provides the JavaVM and Activity (Context) the backend needs to
// reach Android's Bluetooth APIs. On Android this MUST be called before
// Adapter.Enable(). With go-slint:
//
//	bluetooth.SetAndroidJNI(slint.AndroidJavaVM(), slint.AndroidActivity())
func SetAndroidJNI(javaVM, activity uintptr) {
	androidVM = javaVM
	androidActivity = activity
}

// ---- cgo wrappers (the only file that touches C) ----

func jniInit(vm, activity uintptr) int {
	if len(gobleDex) == 0 {
		return -999
	}
	return int(C.goble_init(C.uintptr_t(vm), C.uintptr_t(activity),
		unsafe.Pointer(&gobleDex[0]), C.int(len(gobleDex))))
}

func jniRequestPermissions(activity uintptr) int {
	return int(C.goble_request_permissions(C.uintptr_t(activity)))
}

func jniPermissionsGranted() bool {
	return C.goble_permissions_granted(C.uintptr_t(androidActivity)) == 1
}

// androidLog writes a line to logcat under the "goble" tag (adb logcat -s goble).
func androidLog(s string) {
	c := C.CString(s)
	C.goble_log(c)
	C.free(unsafe.Pointer(c))
}

func jniStartScan() int { return int(C.goble_start_scan()) }
func jniStopScan() int  { return int(C.goble_stop_scan()) }

func jniConnect(addr string) int {
	c := C.CString(addr)
	defer C.free(unsafe.Pointer(c))
	return int(C.goble_connect(c))
}

func jniDisconnect(addr string) {
	c := C.CString(addr)
	defer C.free(unsafe.Pointer(c))
	C.goble_disconnect(c)
}

func jniDiscoverServices(addr string) int {
	c := C.CString(addr)
	defer C.free(unsafe.Pointer(c))
	return int(C.goble_discover_services(c))
}

func jniServiceUUIDs(addr string) string {
	c := C.CString(addr)
	defer C.free(unsafe.Pointer(c))
	p := C.goble_service_uuids(c)
	if p == nil {
		return ""
	}
	s := C.GoString(p)
	C.free(unsafe.Pointer(p))
	return s
}

func jniCharUUIDs(addr, service string) string {
	ca := C.CString(addr)
	cs := C.CString(service)
	defer C.free(unsafe.Pointer(ca))
	defer C.free(unsafe.Pointer(cs))
	p := C.goble_char_uuids(ca, cs)
	if p == nil {
		return ""
	}
	s := C.GoString(p)
	C.free(unsafe.Pointer(p))
	return s
}

func jniWrite(addr, service, chr string, data []byte, withResponse bool) int {
	ca := C.CString(addr)
	cs := C.CString(service)
	cc := C.CString(chr)
	defer C.free(unsafe.Pointer(ca))
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cc))
	var p unsafe.Pointer
	if len(data) > 0 {
		p = unsafe.Pointer(&data[0])
	}
	wr := C.int(0)
	if withResponse {
		wr = 1
	}
	return int(C.goble_write(ca, cs, cc, p, C.int(len(data)), wr))
}

func jniRead(addr, service, chr string) int {
	ca := C.CString(addr)
	cs := C.CString(service)
	cc := C.CString(chr)
	defer C.free(unsafe.Pointer(ca))
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cc))
	return int(C.goble_read(ca, cs, cc))
}

func jniSetNotify(addr, service, chr string, enable bool) int {
	ca := C.CString(addr)
	cs := C.CString(service)
	cc := C.CString(chr)
	defer C.free(unsafe.Pointer(ca))
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cc))
	en := C.int(0)
	if enable {
		en = 1
	}
	return int(C.goble_set_notify(ca, cs, cc, en))
}

func jniPoll(timeoutMs int64) []byte {
	var n C.int
	p := C.goble_poll(C.long(timeoutMs), &n)
	if p == nil {
		return nil
	}
	b := C.GoBytes(p, n)
	C.free(p)
	return b
}

// ---- adapter ----

type Adapter struct {
	initMu  sync.Mutex
	enabled bool

	// scanning state
	scanMu       sync.Mutex
	scanCallback func(*Adapter, ScanResult)
	scanCancel   chan struct{}

	// dispatchQ runs user callbacks (scan results, notifications, connect
	// handlers) on a dedicated goroutine, in order. The pump must never run
	// user code: a callback that issues a GATT operation would block the pump,
	// and the operation's completion event — which only the pump can deliver —
	// would never arrive (deadlock until timeout).
	dispatchQ chan func()

	// connected devices, keyed by MAC string
	devMu   sync.Mutex
	devices map[string]*androidDevice

	connectHandler func(device Device, connected bool)
}

// DefaultAdapter is the default adapter on the system.
//
// Make sure to call Enable() before using it to initialize the adapter.
var DefaultAdapter = &Adapter{
	devices:        map[string]*androidDevice{},
	connectHandler: func(Device, bool) {},
}

// Enable configures the BLE stack. On Android it loads the Java shim, requests
// the BLE runtime permissions, and starts the event pump. It must be called
// after SetAndroidJNI and before any other Bluetooth call. It is idempotent.
func (a *Adapter) Enable() error {
	a.initMu.Lock()
	defer a.initMu.Unlock()
	if a.enabled {
		return nil
	}
	if androidVM == 0 || androidActivity == 0 {
		return errors.New("bluetooth: SetAndroidJNI must be called before Enable (need the JavaVM + Activity)")
	}
	if a.devices == nil {
		a.devices = map[string]*androidDevice{}
	}
	if step := jniInit(androidVM, androidActivity); step != 0 {
		return fmt.Errorf("bluetooth: android init failed at JNI step %d (see: adb logcat -s goble)", -step)
	}
	// Fire the runtime permission prompt. It is asynchronous, and callers (e.g.
	// wedo2) start scanning the moment Enable returns — so wait, bounded, for the
	// grant. Otherwise the first startScan is rejected with a SecurityException
	// and the caller typically never retries.
	if jniRequestPermissions(androidActivity) != 1 {
		for i := 0; i < 120 && !jniPermissionsGranted(); i++ {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if jniPermissionsGranted() {
		if debug {
			androidLog("Enable: BLE permissions granted")
		}
	} else {
		androidLog("Enable: BLE permissions NOT granted after 30s — scan will fail")
	}
	a.dispatchQ = make(chan func(), 512)
	go func() { // user-callback dispatcher; see dispatchQ doc
		for f := range a.dispatchQ {
			f()
		}
	}()
	go a.pump()
	a.enabled = true
	return nil
}

// enqueue hands a user callback to the dispatcher goroutine. Non-blocking: if
// the queue is full (a callback is stuck), the event is dropped with a log line
// rather than stalling the pump.
func (a *Adapter) enqueue(f func()) {
	select {
	case a.dispatchQ <- f:
	default:
		androidLog("dispatch queue full — dropping event")
	}
}

// device returns the per-connection state for addr, creating it on first use.
func (a *Adapter) device(addr string) *androidDevice {
	a.devMu.Lock()
	defer a.devMu.Unlock()
	d := a.devices[addr]
	if d == nil {
		d = &androidDevice{
			address:    addr,
			notify:     map[string]func([]byte){},
			notifySeen: map[string]int{},
		}
		a.devices[addr] = d
	}
	return d
}

// ---- per-connection state ----

type readResult struct {
	value  []byte
	status int
}

type androidDevice struct {
	address string

	mu         sync.Mutex // guards the one-shot waiter channels below
	connectCh  chan int
	servicesCh chan int
	writeCh    chan int
	descCh     chan int
	readCh     chan readResult

	opMu sync.Mutex // serializes GATT operations (Android allows one at a time)

	notifyMu   sync.Mutex
	notify     map[string]func([]byte) // lower-case char UUID -> callback
	notifySeen map[string]int          // lower-case char UUID -> notification count (logging)
}

func trySendInt(ch chan int, v int) {
	if ch != nil {
		select {
		case ch <- v:
		default:
		}
	}
}

func (d *androidDevice) sigServices(st int) {
	d.mu.Lock()
	ch := d.servicesCh
	d.mu.Unlock()
	trySendInt(ch, st)
}

func (d *androidDevice) sigWrite(st int) {
	d.mu.Lock()
	ch := d.writeCh
	d.mu.Unlock()
	trySendInt(ch, st)
}

func (d *androidDevice) sigDesc(st int) {
	d.mu.Lock()
	ch := d.descCh
	d.mu.Unlock()
	trySendInt(ch, st)
}

func (d *androidDevice) sigRead(v []byte, st int) {
	d.mu.Lock()
	ch := d.readCh
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- readResult{value: v, status: st}:
		default:
		}
	}
}

// ---- event pump ----

// event type tags — must match GoBle.java.
const (
	evScan         = 1
	evConnected    = 2
	evDisconnected = 3
	evServices     = 4
	evNotify       = 5
	evWrite        = 6
	evRead         = 7
	evDescWrite    = 8
	evScanFailed   = 9
)

func (a *Adapter) pump() {
	runtime.LockOSThread()
	for {
		b := jniPoll(500)
		if b != nil {
			a.dispatch(b)
		}
	}
}

func (a *Adapter) dispatch(b []byte) {
	r := &evReader{b: b}
	switch r.i32() {
	case evScan:
		addr := r.str()
		rssi := int16(r.i32())
		name := r.str()
		n := int(r.i32())
		uuids := make([]UUID, 0, n)
		for i := 0; i < n; i++ {
			if u, err := ParseUUID(r.str()); err == nil {
				uuids = append(uuids, u)
			}
		}
		if !r.bad {
			a.deliverScan(addr, rssi, name, uuids)
		}
	case evConnected:
		addr := r.str()
		st := int(r.i32())
		if !r.bad {
			if debug {
				androidLog(fmt.Sprintf("event: connected %s status=%d", addr, st))
			}
			d := a.device(addr)
			d.mu.Lock()
			ch := d.connectCh
			d.mu.Unlock()
			trySendInt(ch, st)
		}
	case evDisconnected:
		addr := r.str()
		st := int(r.i32())
		if !r.bad {
			if debug {
				androidLog(fmt.Sprintf("event: disconnected %s status=%d", addr, st))
			}
			a.signalDisconnect(addr, st)
		}
	case evServices:
		addr := r.str()
		st := int(r.i32())
		if !r.bad {
			if debug {
				androidLog(fmt.Sprintf("event: servicesDiscovered %s status=%d", addr, st))
			}
			a.device(addr).sigServices(st)
		}
	case evNotify:
		addr := r.str()
		chr := r.str()
		val := r.bytes()
		if !r.bad {
			a.deliverNotify(addr, chr, val)
		}
	case evWrite:
		addr := r.str()
		_ = r.str() // char uuid (only one write outstanding per device)
		st := int(r.i32())
		if !r.bad {
			a.device(addr).sigWrite(st)
		}
	case evRead:
		addr := r.str()
		_ = r.str()
		st := int(r.i32())
		val := r.bytes()
		if !r.bad {
			a.device(addr).sigRead(val, st)
		}
	case evDescWrite:
		addr := r.str()
		st := int(r.i32())
		if !r.bad {
			a.device(addr).sigDesc(st)
		}
	case evScanFailed:
		_ = r.str()
		code := int(r.i32())
		log.Printf("bluetooth: android scan failed (code %d)", code)
	}
}

// evReader decodes the big-endian event format produced by GoBle.java. Reads
// past the end set r.bad and yield zero values, so a truncated event can't panic
// the pump.
type evReader struct {
	b   []byte
	i   int
	bad bool
}

func (r *evReader) i32() int32 {
	if r.bad || r.i+4 > len(r.b) {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b[r.i:])
	r.i += 4
	return int32(v)
}

func (r *evReader) str() string {
	n := int(r.i32())
	if r.bad || n < 0 || r.i+n > len(r.b) {
		r.bad = true
		return ""
	}
	s := string(r.b[r.i : r.i+n])
	r.i += n
	return s
}

func (r *evReader) bytes() []byte {
	n := int(r.i32())
	if r.bad || n < 0 || r.i+n > len(r.b) {
		r.bad = true
		return nil
	}
	v := make([]byte, n)
	copy(v, r.b[r.i:r.i+n])
	r.i += n
	return v
}

// waitInt waits for a one-shot int signal on ch (which the caller stored in the
// device) up to timeout. It returns the status and whether it arrived.
func waitInt(ch chan int, timeout time.Duration) (int, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-time.After(timeout):
		return 0, false
	}
}
