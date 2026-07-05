package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth/advertisement"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth/genericattributeprofile"
	"github.com/saltosystems/winrt-go/windows/foundation"
	"github.com/saltosystems/winrt-go/windows/storage/streams"
)

// Address contains a Bluetooth MAC address.
type Address struct {
	MACAddress
}

type Advertisement struct {
	advertisement *advertisement.BluetoothLEAdvertisement
	publisher     *advertisement.BluetoothLEAdvertisementPublisher
}

// DefaultAdvertisement returns the default advertisement instance but does not
// configure it.
func (a *Adapter) DefaultAdvertisement() *Advertisement {
	if a.defaultAdvertisement == nil {
		a.defaultAdvertisement = &Advertisement{}
	}

	return a.defaultAdvertisement
}

// Configure this advertisement.
// on Windows we're only able to set "Manufacturer Data" for advertisements.
// https://learn.microsoft.com/en-us/uwp/api/windows.devices.bluetooth.advertisement.bluetoothleadvertisementpublisher?view=winrt-22621#remarks
// following this c# source for this implementation: https://github.com/microsoft/Windows-universal-samples/blob/main/Samples/BluetoothAdvertisement/cs/Scenario2_Publisher.xaml.cs
// adding service data / localname leads to errors when starting the advertisement.
func (a *Advertisement) Configure(options AdvertisementOptions) error {
	// we can only advertise manufacturer / company data on windows, so no need to continue if we have none
	if len(options.ManufacturerData) == 0 {
		return nil
	}

	if a.publisher != nil {
		a.publisher.Release()
	}

	if a.advertisement != nil {
		a.advertisement.Release()
	}

	pub, err := advertisement.NewBluetoothLEAdvertisementPublisher()
	if err != nil {
		return err
	}

	a.publisher = pub

	ad, err := a.publisher.GetAdvertisement()
	if err != nil {
		return err
	}

	a.advertisement = ad

	vec, err := ad.GetManufacturerData()
	if err != nil {
		return err
	}

	for _, optManData := range options.ManufacturerData {
		writer, err := streams.NewDataWriter()
		if err != nil {
			return err
		}
		defer writer.Release()

		err = writer.WriteBytes(uint32(len(optManData.Data)), optManData.Data)
		if err != nil {
			return err
		}

		buf, err := writer.DetachBuffer()
		if err != nil {
			return err
		}

		manData, err := advertisement.BluetoothLEManufacturerDataCreate(optManData.CompanyID, buf)
		if err != nil {
			return err
		}

		if err = vec.Append(unsafe.Pointer(&manData.IUnknown.RawVTable)); err != nil {
			return err
		}
	}

	return nil
}

// Start advertisement. May only be called after it has been configured.
func (a *Advertisement) Start() error {
	// publisher will be present if we actually have manufacturer data to advertise.
	if a.publisher != nil {
		return a.publisher.Start()
	}

	return nil
}

// Stop advertisement. May only be called after it has been started.
func (a *Advertisement) Stop() error {
	if a.publisher != nil {
		return a.publisher.Stop()
	}

	return nil
}

// Scan starts a BLE scan. It is stopped by a call to StopScan. A common pattern
// is to cancel the scan when a particular device has been found.
func (a *Adapter) Scan(callback func(*Adapter, ScanResult)) (err error) {
	if a.watcher != nil {
		// Cannot scan more than once: which one should ScanStop()
		// stop?
		return errScanning
	}

	a.watcher, err = advertisement.NewBluetoothLEAdvertisementWatcher()
	if err != nil {
		return
	}
	defer func() {
		// Same grace period as the event handlers below: a Received event can
		// still be running on a WinRT thread as the scan winds down.
		w := a.watcher
		time.AfterFunc(2*time.Second, func() { _ = w.Release() })
		a.watcher = nil
	}()

	// Set scanning mode to active so we receive scan responses
	// from devices in advertising mode
	err = a.watcher.SetScanningMode(advertisement.BluetoothLEScanningModeActive)
	if err != nil {
		return
	}

	// Listen for incoming BLE advertisement packets.
	// We need a TypedEventHandler<TSender, TResult> to listen to events, but since this is a parameterized delegate
	// its GUID depends on the classes used as sender and result, so we need to compute it:
	// TypedEventHandler<BluetoothLEAdvertisementWatcher, BluetoothLEAdvertisementReceivedEventArgs>
	eventReceivedGuid := winrt.ParameterizedInstanceGUID(
		foundation.GUIDTypedEventHandler,
		advertisement.SignatureBluetoothLEAdvertisementWatcher,
		advertisement.SignatureBluetoothLEAdvertisementReceivedEventArgs,
	)
	// Windows delivers a device's advertisement and its scan response as two
	// separate Received events and never combines them — but the fields are
	// split across the two (LEGO hubs, for example, put the service UUIDs in
	// the advertisement and the custom device name in the scan response).
	// BlueZ and Android merge the pair in the OS; do the same here so every
	// ScanResult carries the union of what the device sent. The cache holds
	// one small entry per unique address and lives only for this scan.
	type advCache struct {
		name             string
		serviceUUIDs     []UUID
		manufacturerData []ManufacturerDataElement
	}
	var mergeMu sync.Mutex
	merged := make(map[Address]advCache)

	handler := foundation.NewTypedEventHandler(ole.NewGUID(eventReceivedGuid), func(instance *foundation.TypedEventHandler, sender, arg unsafe.Pointer) {
		args := (*advertisement.BluetoothLEAdvertisementReceivedEventArgs)(arg)
		result := getScanResultFromArgs(args)
		if fields, ok := result.AdvertisementPayload.(*advertisementFields); ok {
			mergeMu.Lock()
			c := merged[result.Address]
			if fields.AdvertisementFields.LocalName == "" {
				fields.AdvertisementFields.LocalName = c.name
			} else {
				c.name = fields.AdvertisementFields.LocalName
			}
			if len(fields.AdvertisementFields.ServiceUUIDs) == 0 {
				fields.AdvertisementFields.ServiceUUIDs = c.serviceUUIDs
			} else {
				c.serviceUUIDs = fields.AdvertisementFields.ServiceUUIDs
			}
			if len(fields.AdvertisementFields.ManufacturerData) == 0 {
				fields.AdvertisementFields.ManufacturerData = c.manufacturerData
			} else {
				c.manufacturerData = fields.AdvertisementFields.ManufacturerData
			}
			merged[result.Address] = c
			mergeMu.Unlock()
		}
		callback(a, result)
	})
	// Grace period before releasing: WinRT can still be dispatching a
	// Received event on another thread when this scan returns (advertisement
	// traffic is continuous), and freeing the delegate under a running
	// callback crashes the process. Removal below stops new dispatches.
	defer time.AfterFunc(2*time.Second, func() { handler.Release() })

	token, err := a.watcher.AddReceived(handler)
	if err != nil {
		return
	}
	defer a.watcher.RemoveReceived(token)

	// Wait for when advertisement has stopped by a call to StopScan().
	// Advertisement doesn't seem to stop right away, there is an
	// intermediate Stopping state.
	stoppingChan := make(chan error)
	// TypedEventHandler<BluetoothLEAdvertisementWatcher, BluetoothLEAdvertisementWatcherStoppedEventArgs>
	eventStoppedGuid := winrt.ParameterizedInstanceGUID(
		foundation.GUIDTypedEventHandler,
		advertisement.SignatureBluetoothLEAdvertisementWatcher,
		advertisement.SignatureBluetoothLEAdvertisementWatcherStoppedEventArgs,
	)
	stoppedHandler := foundation.NewTypedEventHandler(ole.NewGUID(eventStoppedGuid), func(_ *foundation.TypedEventHandler, _, arg unsafe.Pointer) {
		args := (*advertisement.BluetoothLEAdvertisementWatcherStoppedEventArgs)(arg)
		errCode, err := args.GetError()
		if err != nil {
			// Got an error while getting the error value, that shouldn't
			// happen.
			stoppingChan <- fmt.Errorf("failed to get stopping error value: %w", err)
		} else if errCode != bluetooth.BluetoothErrorSuccess {
			// Could not stop the scan? I'm not sure when this would actually
			// happen.
			stoppingChan <- fmt.Errorf("failed to stop scanning (error code %d)", errCode)
		}
		close(stoppingChan)
	})
	defer time.AfterFunc(2*time.Second, func() { stoppedHandler.Release() })

	token, err = a.watcher.AddStopped(stoppedHandler)
	if err != nil {
		return
	}
	defer a.watcher.RemoveStopped(token)

	err = a.watcher.Start()
	if err != nil {
		return err
	}

	// Wait until advertisement has stopped, and finish.
	return <-stoppingChan
}

func getScanResultFromArgs(args *advertisement.BluetoothLEAdvertisementReceivedEventArgs) ScanResult {
	// parse bluetooth address
	addr, _ := args.GetBluetoothAddress()
	adr := Address{}
	for i := range adr.MAC {
		adr.MAC[i] = byte(addr)
		addr >>= 8
	}
	sigStrength, _ := args.GetRawSignalStrengthInDBm()
	result := ScanResult{
		RSSI:    sigStrength,
		Address: adr,
	}

	winAdv, err := args.GetAdvertisement()
	if err != nil {
		return result
	}
	defer winAdv.Release()

	var manufacturerData []ManufacturerDataElement
	var serviceUUIDs []UUID

	// Extract manufacturer data
	manDataVector, _ := winAdv.GetManufacturerData()
	if manDataVector != nil {
		defer manDataVector.Release()
		size, _ := manDataVector.GetSize()
		for i := uint32(0); i < size; i++ {
			element, _ := manDataVector.GetAt(i)
			manData := (*advertisement.BluetoothLEManufacturerData)(element)

			companyID, _ := manData.GetCompanyId()
			buffer, _ := manData.GetData()
			if buffer != nil {
				manufacturerData = append(manufacturerData, ManufacturerDataElement{
					CompanyID: companyID,
					Data:      bufferToSlice(buffer),
				})
				buffer.Release()
			}
			manData.Release()
		}
	}

	// Extract service UUIDs. The generated IVector.GetAt cannot return GUID
	// elements: it passes a pointer-sized out slot, but the native side writes
	// a full 16-byte GUID through it, corrupting the caller's stack frame and
	// truncating the value. Call the vtable slot directly with a properly
	// sized out parameter instead.
	uVector, _ := winAdv.GetServiceUuids()
	if uVector != nil {
		defer uVector.Release()
		uSize, _ := uVector.GetSize()
		for i := uint32(0); i < uSize; i++ {
			var guid syscall.GUID
			hr, _, _ := syscall.SyscallN(
				uVector.VTable().GetAt,
				uintptr(unsafe.Pointer(uVector)),
				uintptr(i),
				uintptr(unsafe.Pointer(&guid)),
			)
			if hr == 0 {
				serviceUUIDs = append(serviceUUIDs, winRTUuidToUuid(guid))
			}
		}
	}

	// Note: the IsRandom bit is never set.
	localName, _ := winAdv.GetLocalName()
	result.AdvertisementPayload = &advertisementFields{
		AdvertisementFields{
			LocalName:        localName,
			ServiceUUIDs:     serviceUUIDs,
			ManufacturerData: manufacturerData,
		},
	}

	return result
}

func GUIDToUUID(guid syscall.GUID) UUID {
	return NewUUID([16]byte{
		byte(guid.Data1 >> 24),
		byte(guid.Data1 >> 16),
		byte(guid.Data1 >> 8),
		byte(guid.Data1),
		byte(guid.Data2 >> 8),
		byte(guid.Data2),
		byte(guid.Data3 >> 8),
		byte(guid.Data3),
		guid.Data4[0], guid.Data4[1],
		guid.Data4[2], guid.Data4[3],
		guid.Data4[4], guid.Data4[5],
		guid.Data4[6], guid.Data4[7],
	})
}

func bufferToSlice(buffer *streams.IBuffer) []byte {
	dataReader, _ := streams.DataReaderFromBuffer(buffer)
	defer dataReader.Release()
	bufferSize, _ := buffer.GetLength()
	if bufferSize == 0 {
		return nil
	}
	data, _ := dataReader.ReadBytes(bufferSize)
	return data
}

// StopScan stops any in-progress scan. It can be called from within a Scan
// callback to stop the current scan. If no scan is in progress, an error will
// be returned.
func (a *Adapter) StopScan() error {
	if a.watcher == nil {
		return errNotScanning
	}
	return a.watcher.Stop()
}

var _ GAPDevice = Device{}

// Device is a connection to a remote peripheral.
type Device struct {
	ctx    context.Context
	cancel context.CancelFunc

	Address Address // the MAC address of the device

	device                        *bluetooth.BluetoothLEDevice
	session                       *genericattributeprofile.GattSession
	connectionStatusListenerToken foundation.EventRegistrationToken
	connectionStatusListener      *foundation.TypedEventHandler

	// resources tracks the per-connection WinRT objects (services,
	// characteristics, notification handlers) so Disconnect can release them.
	// Without this, every discovery and every notification subscription leaks
	// native objects — and each leaked notification handler additionally pins
	// a keepalive goroutine with a running timer inside winrt-go.
	resources *deviceResources

	// closeOnce makes Disconnect idempotent: both the application and the
	// ConnectionStatusChanged handler call it (the handler on remote drops),
	// and releasing the COM references twice corrupts their refcounts.
	closeOnce *sync.Once

	// closed tells the ConnectionStatusChanged handler that teardown has
	// begun, so a status event racing Disconnect doesn't touch objects that
	// are about to be (or already are) released.
	closed *atomic.Bool
}

// deviceResources collects the WinRT objects that belong to one connection.
type deviceResources struct {
	mu         sync.Mutex
	services   []*genericattributeprofile.GattDeviceService
	notifChars []*deviceCharacteristic // wrappers holding an active ValueChanged handler
}

func (r *deviceResources) addService(s *genericattributeprofile.GattDeviceService) {
	r.mu.Lock()
	r.services = append(r.services, s)
	r.mu.Unlock()
}

func (r *deviceResources) addNotifChar(dc *deviceCharacteristic) {
	r.mu.Lock()
	for _, existing := range r.notifChars {
		if existing == dc {
			r.mu.Unlock()
			return
		}
	}
	r.notifChars = append(r.notifChars, dc)
	r.mu.Unlock()
}

// releaseAll removes still-registered notification handlers and closes every
// tracked service. Called once, from Disconnect.
//
// Only backend-owned objects are fully Released here. The service and
// characteristic wrappers handed out to applications are left alive: an
// application goroutine can still be inside a Read or Write on them when a
// remote disconnect triggers this cleanup (a heartbeat read racing the drop
// is the classic case), and releasing a COM object under an in-flight call
// crashes the process. With the session closed those calls fail cleanly
// instead. The unreleased wrappers are small, and this matches upstream's
// lifetime behavior.
func (r *deviceResources) releaseAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, dc := range r.notifChars {
		if dc.valueChangedEventHandler != nil {
			_ = dc.characteristic.RemoveValueChanged(dc.valueChangedEventHandlerToken)
			handler := dc.valueChangedEventHandler
			char := dc.characteristic
			dc.valueChangedEventHandler = nil
			// The characteristic must be released along with the handler: if
			// the link is already down (a graceful disconnect command from
			// the app commonly beats this cleanup), the Remove above fails
			// silently and WinRT keeps its reference on the delegate — only
			// destroying the characteristic, and with it the event source,
			// drops that reference. A delegate that never reaches refcount
			// zero pins native memory plus a keepalive goroutine with a
			// running timer, forever, per subscription per connection.
			//
			// The grace period lets callbacks and calls already in flight
			// finish, and the connectionClosed guards on the characteristic
			// methods turn any later application call into a clean error
			// instead of a use-after-free.
			time.AfterFunc(2*time.Second, func() {
				handler.Release()
				char.Release()
			})
		}
	}
	r.notifChars = nil
	for _, s := range r.services {
		_ = s.Close()
	}
	r.services = nil
}

// Connect starts a connection attempt to the given peripheral device address.
//
// On Linux and Windows, the IsRandom part of the address is ignored.
func (a *Adapter) Connect(address Address, params ConnectionParams) (Device, error) {
	var winAddr uint64
	for i := range address.MAC {
		winAddr += uint64(address.MAC[i]) << (8 * i)
	}

	// IAsyncOperation<BluetoothLEDevice>
	bleDeviceOp, err := bluetooth.BluetoothLEDeviceFromBluetoothAddressAsync(winAddr)
	if err != nil {
		return Device{}, err
	}
	defer bleDeviceOp.Release()

	// We need to pass the signature of the parameter returned by the async operation:
	// IAsyncOperation<BluetoothLEDevice>
	if err := awaitAsyncOperation(bleDeviceOp, bluetooth.SignatureBluetoothLEDevice); err != nil {
		return Device{}, fmt.Errorf("error connecting to device: %w", err)
	}

	res, err := bleDeviceOp.GetResults()
	if err != nil {
		return Device{}, err
	}

	// The returned BluetoothLEDevice is set to null if FromBluetoothAddressAsync can't find the device identified by bluetoothAddress
	if uintptr(res) == 0x0 {
		return Device{}, fmt.Errorf("device with the given address was not found")
	}

	bleDevice := (*bluetooth.BluetoothLEDevice)(res)

	// Creating a BluetoothLEDevice object by calling this method alone doesn't (necessarily) initiate a connection.
	// To initiate a connection, we need to set GattSession.MaintainConnection to true.
	dID, err := bleDevice.GetBluetoothDeviceId()
	if err != nil {
		return Device{}, err
	}
	defer dID.Release()

	// Windows does not support explicitly connecting to a device.
	// Instead it has the concept of a GATT session that is owned
	// by the calling program.
	gattSessionOp, err := genericattributeprofile.GattSessionFromDeviceIdAsync(dID) // IAsyncOperation<GattSession>
	if err != nil {
		return Device{}, err
	}
	defer gattSessionOp.Release()

	if err := awaitAsyncOperation(gattSessionOp, genericattributeprofile.SignatureGattSession); err != nil {
		return Device{}, fmt.Errorf("error getting gatt session: %w", err)
	}

	gattRes, err := gattSessionOp.GetResults()
	if err != nil {
		return Device{}, err
	}
	newSession := (*genericattributeprofile.GattSession)(gattRes)
	// This keeps the device connected until we set maintain_connection = False.
	if err := newSession.SetMaintainConnection(true); err != nil {
		return Device{}, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	device := Device{
		ctx:    ctx,
		cancel: cancel,

		Address: address,

		device:  bleDevice,
		session: newSession,

		resources: &deviceResources{},
		closeOnce: &sync.Once{},
		closed:    &atomic.Bool{},
	}

	// https://learn.microsoft.com/es-es/uwp/api/windows.devices.bluetooth.bluetoothledevice.connectionstatuschanged?view=winrt-26100
	// TypedEventHandler<BluetoothLEDevice,object>
	connectionStatusChangedGUID := winrt.ParameterizedInstanceGUID(
		foundation.GUIDTypedEventHandler,
		bluetooth.SignatureBluetoothLEDevice,
		"cinterface(IInspectable)", // object
	)

	handler := foundation.NewTypedEventHandler(ole.NewGUID(connectionStatusChangedGUID), func(instance *foundation.TypedEventHandler, sender, arg unsafe.Pointer) {
		if device.closed.Load() {
			// Teardown already started; the device object may be released.
			return
		}
		status, err := bleDevice.GetConnectionStatus()
		if err != nil {
			return
		}
		if status == bluetooth.BluetoothConnectionStatusDisconnected {
			device.Disconnect()
		}

		if a.connectHandler != nil {
			a.connectHandler(device, status == bluetooth.BluetoothConnectionStatusConnected)
		}
	})

	token, err := device.device.AddConnectionStatusChanged(handler)

	device.connectionStatusListenerToken = token
	device.connectionStatusListener = handler

	if err != nil {
		_ = handler.Release()
		return device, err
	}

	return device, nil
}

// Disconnect from the BLE device. This method is non-blocking and does not
// wait until the connection is fully gone.
//
// Disconnect is idempotent: it is called both by applications and by the
// ConnectionStatusChanged handler when the peripheral drops the connection,
// and the teardown must run exactly once.
func (d Device) Disconnect() error {
	var err error
	if d.closeOnce != nil {
		d.closeOnce.Do(func() { err = d.disconnect() })
		return err
	}
	return d.disconnect()
}

func (d Device) disconnect() error {
	// Flag first: a ConnectionStatusChanged event racing this teardown checks
	// it and bails instead of touching soon-to-be-released objects.
	if d.closed != nil {
		d.closed.Store(true)
	}

	d.cancel()

	// Stop future status callbacks, then remove notification handlers and
	// close the discovered services.
	_ = d.device.RemoveConnectionStatusChanged(d.connectionStatusListenerToken)
	d.resources.releaseAll()

	sessErr := d.session.Close()
	devErr := d.device.Close()

	// Release the COM references after a grace period: WinRT event removal
	// does not wait for callbacks already running on its threads, and
	// releasing an object out from under an in-flight callback crashes the
	// process.
	dev, session, listener := d.device, d.session, d.connectionStatusListener
	time.AfterFunc(2*time.Second, func() {
		dev.Release()
		session.Release()
		if listener != nil {
			listener.Release()
		}
	})

	if sessErr != nil {
		return sessErr
	}
	return devErr
}

// Connected returns whether the device is currently connected.
func (d Device) Connected() (bool, error) {
	if d.device == nil {
		return false, nil
	}
	status, err := d.device.GetConnectionStatus()
	if err != nil {
		return false, err
	}
	return status == bluetooth.BluetoothConnectionStatusConnected, nil
}

// RequestConnectionParams requests a different connection latency and timeout
// of the given device connection. Fields that are unset will be left alone.
// Whether or not the device will actually honor this, depends on the device and
// on the specific parameters.
//
// On Windows, this call doesn't do anything.
func (d Device) RequestConnectionParams(params ConnectionParams) error {
	// TODO: implement this using
	// BluetoothLEDevice.RequestPreferredConnectionParameters.
	return nil
}

// SetRandomAddress sets the random address to be used for advertising.
func (a *Adapter) SetRandomAddress(mac MAC) error {
	return errors.ErrUnsupported
}
