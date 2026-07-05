//go:build android

package bluetooth

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Address contains a Bluetooth MAC address.
type Address struct {
	MACAddress
}

// Scan starts a BLE scan. It blocks until StopScan is called (a common pattern
// is to stop from within the callback once the wanted device is found). The
// callback is invoked for every advertisement; filtering by service UUID is up
// to the caller, matching the other backends.
func (a *Adapter) Scan(callback func(*Adapter, ScanResult)) error {
	a.scanMu.Lock()
	if a.scanCancel != nil {
		a.scanMu.Unlock()
		return errScanning
	}
	cancel := make(chan struct{})
	a.scanCancel = cancel
	a.scanCallback = callback
	a.scanMu.Unlock()

	if r := jniStartScan(); r != 0 {
		a.scanMu.Lock()
		a.scanCancel = nil
		a.scanCallback = nil
		a.scanMu.Unlock()
		return fmt.Errorf("bluetooth: could not start scan (%d)", r)
	}

	if debug {
		androidLog("scan: startScan issued")
	}
	<-cancel // wait for StopScan

	jniStopScan()
	a.scanMu.Lock()
	a.scanCallback = nil
	a.scanMu.Unlock()
	return nil
}

// StopScan stops any in-progress scan. It can be called from within a Scan
// callback.
func (a *Adapter) StopScan() error {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	if a.scanCancel == nil {
		return errNotScanning
	}
	close(a.scanCancel)
	a.scanCancel = nil
	return nil
}

// deliverScan builds a ScanResult and hands it to the active scan callback.
func (a *Adapter) deliverScan(addr string, rssi int16, name string, uuids []UUID) {
	a.scanMu.Lock()
	cb := a.scanCallback
	a.scanMu.Unlock()
	if cb == nil {
		return
	}
	mac, err := ParseMAC(addr)
	if err != nil {
		return
	}
	if debug {
		us := make([]string, len(uuids))
		for i, u := range uuids {
			us[i] = u.String()
		}
		androidLog(fmt.Sprintf("go-scan: %s rssi=%d name=%q uuids=[%s]", addr, rssi, name, strings.Join(us, " ")))
	}
	sr := ScanResult{
		Address: Address{MACAddress{MAC: mac}},
		RSSI:    rssi,
		AdvertisementPayload: &advertisementFields{
			AdvertisementFields{
				LocalName:    name,
				ServiceUUIDs: uuids,
			},
		},
	}
	a.enqueue(func() { cb(a, sr) })
}

// Device is a connection to a remote Bluetooth device.
type Device struct {
	Address Address

	adapter *Adapter
	address string // MAC string, for JNI calls
}

var _ GAPDevice = Device{}

// Connected returns whether the device is currently connected.
func (d Device) Connected() (bool, error) {
	if d.adapter == nil {
		return false, nil
	}
	dev := d.adapter.device(d.address)
	dev.mu.Lock()
	defer dev.mu.Unlock()
	return dev.connected, nil
}

// RequestConnectionParams requests a different connection latency and timeout
// for this connection. The Android stack chooses its own parameters, so this
// is a no-op — matching the Linux backend.
func (d Device) RequestConnectionParams(params ConnectionParams) error {
	return nil
}

// Connect starts a connection attempt to the given peripheral and blocks until
// it is connected (or the attempt fails / times out).
func (a *Adapter) Connect(address Address, params ConnectionParams) (Device, error) {
	addr := address.MAC.String()
	d := a.device(addr)

	ch := make(chan int, 1)
	d.mu.Lock()
	d.connectCh = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.connectCh = nil
		d.mu.Unlock()
	}()

	if r := jniConnect(addr); r != 0 {
		return Device{}, fmt.Errorf("bluetooth: failed to start connection to %s (%d)", addr, r)
	}

	timeout := 15 * time.Second
	if params.ConnectionTimeout != 0 {
		timeout = time.Duration(params.ConnectionTimeout) * 625 * time.Microsecond
	}
	status, ok := waitInt(ch, timeout)
	if !ok {
		jniDisconnect(addr)
		return Device{}, errors.New("bluetooth: connection timeout")
	}
	if status != 0 {
		return Device{}, fmt.Errorf("bluetooth: connection failed (status %d)", status)
	}

	d.mu.Lock()
	d.connected = true
	d.mu.Unlock()

	dev := Device{Address: address, adapter: a, address: addr}
	if a.connectHandler != nil {
		a.connectHandler(dev, true)
	}
	return dev, nil
}

// signalDisconnect routes a disconnect event: it fails a pending connect, or
// otherwise notifies the connect handler.
func (a *Adapter) signalDisconnect(addr string, status int) {
	d := a.device(addr)
	d.mu.Lock()
	d.connected = false
	pending := d.connectCh
	d.mu.Unlock()
	if pending != nil {
		// Disconnected while a Connect was still waiting: report failure.
		trySendInt(pending, 0x10000|status)
		return
	}
	if a.connectHandler != nil {
		mac, err := ParseMAC(addr)
		if err == nil {
			dev := Device{Address: Address{MACAddress{MAC: mac}}, adapter: a, address: addr}
			a.enqueue(func() { a.connectHandler(dev, false) })
		}
	}
}

// Disconnect closes the connection to the device. It is non-blocking.
func (d Device) Disconnect() error {
	if d.adapter != nil {
		dev := d.adapter.device(d.address)
		dev.mu.Lock()
		dev.connected = false
		dev.mu.Unlock()
		if d.adapter.connectHandler != nil {
			d.adapter.connectHandler(d, false)
		}
	}
	jniDisconnect(d.address)
	return nil
}
