//go:build android

package bluetooth

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// uuidWrapper is an alias for UUID so DeviceService/DeviceCharacteristic can
// embed it without their UUID() method colliding with the field.
type uuidWrapper = UUID

// DeviceService is a BLE service on a connected device.
type DeviceService struct {
	uuidWrapper
	adapter *Adapter
	address string
}

// UUID returns the UUID for this DeviceService.
func (s DeviceService) UUID() UUID { return s.uuidWrapper }

// DiscoverServices discovers the services on the connected device. Passing a nil
// slice returns all services; otherwise only the requested UUIDs are returned,
// in the requested order.
func (d Device) DiscoverServices(uuids []UUID) ([]DeviceService, error) {
	dev := d.adapter.device(d.address)

	ch := make(chan int, 1)
	dev.mu.Lock()
	dev.servicesCh = ch
	dev.mu.Unlock()
	defer func() {
		dev.mu.Lock()
		dev.servicesCh = nil
		dev.mu.Unlock()
	}()

	if r := jniDiscoverServices(d.address); r != 0 {
		return nil, errors.New("bluetooth: could not start service discovery")
	}
	if status, ok := waitInt(ch, 12*time.Second); !ok {
		return nil, errors.New("bluetooth: timeout on DiscoverServices")
	} else if status != 0 {
		return nil, errors.New("bluetooth: service discovery failed")
	}

	found := parseUUIDList(jniServiceUUIDs(d.address))
	if len(uuids) == 0 {
		out := make([]DeviceService, len(found))
		for i, u := range found {
			out[i] = DeviceService{uuidWrapper: u, adapter: d.adapter, address: d.address}
		}
		return out, nil
	}

	out := make([]DeviceService, len(uuids))
	for i, want := range uuids {
		matched := false
		for _, u := range found {
			if u == want {
				out[i] = DeviceService{uuidWrapper: u, adapter: d.adapter, address: d.address}
				matched = true
				break
			}
		}
		if !matched {
			return nil, errors.New("bluetooth: could not find some services")
		}
	}
	return out, nil
}

// DeviceCharacteristic is a BLE characteristic on a connected device.
type DeviceCharacteristic struct {
	uuidWrapper
	adapter *Adapter
	address string
	service string // owning service UUID (canonical string), for JNI lookups
}

// UUID returns the UUID for this DeviceCharacteristic.
func (c DeviceCharacteristic) UUID() UUID { return c.uuidWrapper }

// DiscoverCharacteristics discovers the characteristics in this service. Passing
// a nil slice returns all; otherwise only the requested UUIDs, in order.
func (s DeviceService) DiscoverCharacteristics(uuids []UUID) ([]DeviceCharacteristic, error) {
	found := parseUUIDList(jniCharUUIDs(s.address, s.uuidWrapper.String()))

	mk := func(u UUID) DeviceCharacteristic {
		return DeviceCharacteristic{
			uuidWrapper: u,
			adapter:     s.adapter,
			address:     s.address,
			service:     s.uuidWrapper.String(),
		}
	}

	if len(uuids) == 0 {
		out := make([]DeviceCharacteristic, len(found))
		for i, u := range found {
			out[i] = mk(u)
		}
		return out, nil
	}

	out := make([]DeviceCharacteristic, len(uuids))
	for i, want := range uuids {
		matched := false
		for _, u := range found {
			if u == want {
				out[i] = mk(u)
				matched = true
				break
			}
		}
		if !matched {
			return nil, errors.New("bluetooth: could not find some characteristics")
		}
	}
	return out, nil
}

// WriteWithoutResponse writes a value using a write command (no response). It
// waits for the write to be dispatched so successive writes don't race the
// Android GATT queue.
func (c DeviceCharacteristic) WriteWithoutResponse(p []byte) (int, error) {
	return c.write(p, false)
}

// Write writes a value using a write request (with response).
func (c DeviceCharacteristic) Write(p []byte) (int, error) {
	return c.write(p, true)
}

func (c DeviceCharacteristic) write(p []byte, withResponse bool) (int, error) {
	dev := c.adapter.device(c.address)
	dev.opMu.Lock()
	defer dev.opMu.Unlock()

	ch := make(chan int, 1)
	dev.mu.Lock()
	dev.writeCh = ch
	dev.mu.Unlock()
	defer func() {
		dev.mu.Lock()
		dev.writeCh = nil
		dev.mu.Unlock()
	}()

	// Only 0 means the write was dispatched: negative codes are our own lookup
	// failures, positive ones are BluetoothStatusCodes errors (API 33+).
	if r := jniWrite(c.address, c.service, c.uuidWrapper.String(), p, withResponse); r != 0 {
		androidLog(fmt.Sprintf("write %s FAILED code=%d", shortUUID(c.uuidWrapper), r))
		return 0, fmt.Errorf("bluetooth: write failed (code %d)", r)
	}
	if debug {
		androidLog(fmt.Sprintf("write %s % x", shortUUID(c.uuidWrapper), p))
	}
	// Wait for onCharacteristicWrite (fires for no-response writes too on
	// Android). Best-effort: if it doesn't arrive we still report success, since
	// the data was accepted by the stack.
	waitInt(ch, 2*time.Second)
	return len(p), nil
}

// Read reads the current characteristic value into data, returning the number of
// bytes read.
func (c DeviceCharacteristic) Read(data []byte) (int, error) {
	dev := c.adapter.device(c.address)
	dev.opMu.Lock()
	defer dev.opMu.Unlock()

	ch := make(chan readResult, 1)
	dev.mu.Lock()
	dev.readCh = ch
	dev.mu.Unlock()
	defer func() {
		dev.mu.Lock()
		dev.readCh = nil
		dev.mu.Unlock()
	}()

	if r := jniRead(c.address, c.service, c.uuidWrapper.String()); r != 0 {
		return 0, errors.New("bluetooth: read failed to dispatch")
	}
	select {
	case rr := <-ch:
		if rr.status != 0 {
			return 0, errors.New("bluetooth: read failed")
		}
		return copy(data, rr.value), nil
	case <-time.After(3 * time.Second):
		return 0, errors.New("bluetooth: read timeout")
	}
}

// EnableNotifications enables notifications for this characteristic, invoking
// callback with each new value. Passing a nil callback disables them.
func (c *DeviceCharacteristic) EnableNotifications(callback func(buf []byte)) error {
	dev := c.adapter.device(c.address)
	key := strings.ToLower(c.uuidWrapper.String())

	enable := callback != nil
	dev.notifyMu.Lock()
	if enable {
		dev.notify[key] = callback
	} else {
		delete(dev.notify, key)
	}
	dev.notifyMu.Unlock()

	dev.opMu.Lock()
	defer dev.opMu.Unlock()

	ch := make(chan int, 1)
	dev.mu.Lock()
	dev.descCh = ch
	dev.mu.Unlock()
	defer func() {
		dev.mu.Lock()
		dev.descCh = nil
		dev.mu.Unlock()
	}()

	if r := jniSetNotify(c.address, c.service, c.uuidWrapper.String(), enable); r != 0 {
		androidLog(fmt.Sprintf("setNotify %s enable=%t FAILED code=%d", shortUUID(c.uuidWrapper), enable, r))
		return fmt.Errorf("bluetooth: could not set notifications (code %d)", r)
	}
	if debug {
		androidLog(fmt.Sprintf("setNotify %s enable=%t ok", shortUUID(c.uuidWrapper), enable))
	}
	// Wait for the CCCD descriptor write to complete (if the characteristic has
	// one). setNotify returns 0 immediately when there is no CCCD, in which case
	// no EV_DESC_WRITE arrives and we just time out harmlessly.
	waitInt(ch, 3*time.Second)
	return nil
}

// deliverNotify routes a characteristic-changed event to its callback.
func (a *Adapter) deliverNotify(addr, chr string, val []byte) {
	key := strings.ToLower(chr)
	d := a.device(addr)
	d.notifyMu.Lock()
	cb := d.notify[key]
	d.notifySeen[key]++
	n := d.notifySeen[key]
	d.notifyMu.Unlock()
	// Log the first notification per characteristic (proves the subscription
	// works) and then every 200th (proves a stream keeps flowing) — without
	// letting a chatty sensor spam logcat.
	if debug && (n == 1 || n%200 == 0) {
		androidLog(fmt.Sprintf("notify#%d %s % x", n, shortUUIDStr(chr), val))
	}
	if cb != nil {
		a.enqueue(func() { cb(val) })
	}
}

// shortUUID returns the distinguishing hex quartet of a 128-bit UUID string
// (chars 4..8, e.g. "1565" in 00001565-1212-…), for compact logging.
func shortUUID(u UUID) string { return shortUUIDStr(u.String()) }

func shortUUIDStr(s string) string {
	if len(s) >= 8 {
		return s[4:8]
	}
	return s
}

// parseUUIDList parses a comma-separated list of UUID strings, skipping any that
// don't parse.
func parseUUIDList(s string) []UUID {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]UUID, 0, len(parts))
	for _, p := range parts {
		if u, err := ParseUUID(strings.TrimSpace(p)); err == nil {
			out = append(out, u)
		}
	}
	return out
}
