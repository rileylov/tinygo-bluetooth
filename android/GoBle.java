package org.tinygo.bluetooth;

import android.bluetooth.BluetoothAdapter;
import android.bluetooth.BluetoothDevice;
import android.bluetooth.BluetoothGatt;
import android.bluetooth.BluetoothGattCallback;
import android.bluetooth.BluetoothGattCharacteristic;
import android.bluetooth.BluetoothGattDescriptor;
import android.bluetooth.BluetoothGattService;
import android.bluetooth.BluetoothManager;
import android.bluetooth.le.BluetoothLeScanner;
import android.bluetooth.le.ScanCallback;
import android.bluetooth.le.ScanRecord;
import android.bluetooth.le.ScanResult;
import android.bluetooth.le.ScanSettings;
import android.content.Context;
import android.os.Build;
import android.os.ParcelUuid;
import android.util.Log;

import java.io.ByteArrayOutputStream;
import java.io.DataOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;

/**
 * GoBle is the Java half of the tinygo-bluetooth Android backend. It owns the
 * Android BLE objects (scanner + per-device GATT), forwards every asynchronous
 * BluetoothGattCallback / ScanCallback into a thread-safe queue, and exposes a
 * small imperative surface that the Go side drives entirely through JNI.
 *
 * The Go side never calls back into Java's threads: it polls {@link #pollEvent}
 * from a dedicated goroutine and issues plain method calls for actions. That
 * keeps all the JNI traffic one-directional (Go -> Java) which is far easier to
 * get right than registering native callbacks.
 *
 * Self-contained: depends only on android.jar (no third-party BLE library), so
 * it compiles to a single dex that ships embedded in the Go package.
 */
public final class GoBle {
    // Event type tags (must match the Go decoder in gattc_android.go).
    static final int EV_SCAN = 1;
    static final int EV_CONNECTED = 2;
    static final int EV_DISCONNECTED = 3;
    static final int EV_SERVICES = 4;
    static final int EV_NOTIFY = 5;
    static final int EV_WRITE = 6;
    static final int EV_READ = 7;
    static final int EV_DESC_WRITE = 8;
    static final int EV_SCAN_FAILED = 9;

    private static final UUID CCCD =
            UUID.fromString("00002902-0000-1000-8000-00805f9b34fb");

    private final Context ctx;
    private final BluetoothAdapter adapter;
    private BluetoothLeScanner scanner;

    private final BlockingQueue<byte[]> events = new LinkedBlockingQueue<>();
    private final Map<String, BluetoothGatt> gatts = new ConcurrentHashMap<>();

    public GoBle(Context ctx) {
        this.ctx = ctx;
        BluetoothManager mgr =
                (BluetoothManager) ctx.getSystemService(Context.BLUETOOTH_SERVICE);
        this.adapter = mgr != null ? mgr.getAdapter() : null;
    }

    public boolean isEnabled() {
        return adapter != null && adapter.isEnabled();
    }

    // ---------------------------------------------------------------- events

    // pollEvent blocks up to timeoutMillis for the next event, returning its
    // encoded bytes, or null on timeout. Called from a single Go goroutine.
    public byte[] pollEvent(long timeoutMillis) {
        try {
            return events.poll(timeoutMillis, TimeUnit.MILLISECONDS);
        } catch (InterruptedException e) {
            return null;
        }
    }

    private void push(byte[] e) {
        if (e != null) {
            events.offer(e);
        }
    }

    // --- encoders. Ints are big-endian (DataOutputStream); strings and byte
    //     blobs are length-prefixed with a big-endian int. ---

    private static void putStr(DataOutputStream o, String s) throws IOException {
        byte[] b = s == null ? new byte[0] : s.getBytes(StandardCharsets.UTF_8);
        o.writeInt(b.length);
        o.write(b);
    }

    private static void putBytes(DataOutputStream o, byte[] b) throws IOException {
        if (b == null) {
            b = new byte[0];
        }
        o.writeInt(b.length);
        o.write(b);
    }

    private byte[] encScan(String addr, int rssi, String name, List<ParcelUuid> uuids) {
        try {
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            DataOutputStream o = new DataOutputStream(bos);
            o.writeInt(EV_SCAN);
            putStr(o, addr);
            o.writeInt(rssi);
            putStr(o, name);
            int n = uuids == null ? 0 : uuids.size();
            o.writeInt(n);
            for (int i = 0; i < n; i++) {
                putStr(o, uuids.get(i).getUuid().toString());
            }
            return bos.toByteArray();
        } catch (IOException e) {
            return null;
        }
    }

    private byte[] encAddrStatus(int type, String addr, int status) {
        try {
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            DataOutputStream o = new DataOutputStream(bos);
            o.writeInt(type);
            putStr(o, addr);
            o.writeInt(status);
            return bos.toByteArray();
        } catch (IOException e) {
            return null;
        }
    }

    private byte[] encCharValue(int type, String addr, String chr, byte[] value) {
        try {
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            DataOutputStream o = new DataOutputStream(bos);
            o.writeInt(type);
            putStr(o, addr);
            putStr(o, chr);
            putBytes(o, value);
            return bos.toByteArray();
        } catch (IOException e) {
            return null;
        }
    }

    private byte[] encCharStatus(int type, String addr, String chr, int status) {
        try {
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            DataOutputStream o = new DataOutputStream(bos);
            o.writeInt(type);
            putStr(o, addr);
            putStr(o, chr);
            o.writeInt(status);
            return bos.toByteArray();
        } catch (IOException e) {
            return null;
        }
    }

    // EV_READ carries addr, chr, status, then the value (matches the Go decoder).
    private byte[] encRead(String addr, String chr, int status, byte[] value) {
        try {
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            DataOutputStream o = new DataOutputStream(bos);
            o.writeInt(EV_READ);
            putStr(o, addr);
            putStr(o, chr);
            o.writeInt(status);
            putBytes(o, value);
            return bos.toByteArray();
        } catch (IOException e) {
            return null;
        }
    }

    // --------------------------------------------------------------- scanning

    private final ScanCallback scanCb = new ScanCallback() {
        @Override
        public void onScanResult(int callbackType, ScanResult result) {
            emitScan(result);
        }

        @Override
        public void onBatchScanResults(List<ScanResult> results) {
            for (ScanResult r : results) {
                emitScan(r);
            }
        }

        @Override
        public void onScanFailed(int errorCode) {
            push(encAddrStatus(EV_SCAN_FAILED, "", errorCode));
        }
    };

    private void emitScan(ScanResult r) {
        if (r == null || r.getDevice() == null) {
            return;
        }
        String addr = r.getDevice().getAddress();
        String name = null;
        List<ParcelUuid> uuids = null;
        ScanRecord rec = r.getScanRecord();
        if (rec != null) {
            name = rec.getDeviceName();
            uuids = rec.getServiceUuids();
        }
        push(encScan(addr, r.getRssi(), name, uuids));
    }

    public void startScan() {
        if (adapter == null) {
            Log.e("goble", "startScan: no BluetoothAdapter");
            return;
        }
        if (!adapter.isEnabled()) {
            Log.e("goble", "startScan: Bluetooth is off");
            return;
        }
        scanner = adapter.getBluetoothLeScanner();
        if (scanner == null) {
            Log.e("goble", "startScan: no LE scanner");
            return;
        }
        ScanSettings settings = new ScanSettings.Builder()
                .setScanMode(ScanSettings.SCAN_MODE_LOW_LATENCY)
                .build();
        // Null filters => report every advertisement; the Go side filters by
        // service UUID, matching the other backends' behaviour.
        try {
            scanner.startScan(null, settings, scanCb);
            Log.i("goble", "startScan: started");
        } catch (Exception e) {
            Log.e("goble", "startScan failed", e);
        }
    }

    public void stopScan() {
        if (scanner != null) {
            try {
                scanner.stopScan(scanCb);
            } catch (Exception ignored) {
            }
        }
    }

    // -------------------------------------------------------------- gatt (client)

    private final BluetoothGattCallback gattCb = new BluetoothGattCallback() {
        @Override
        public void onConnectionStateChange(BluetoothGatt gatt, int status, int newState) {
            String addr = gatt.getDevice().getAddress();
            if (newState == BluetoothGatt.STATE_CONNECTED) {
                push(encAddrStatus(EV_CONNECTED, addr, status));
            } else if (newState == BluetoothGatt.STATE_DISCONNECTED) {
                gatts.remove(addr);
                try {
                    gatt.close();
                } catch (Exception ignored) {
                }
                push(encAddrStatus(EV_DISCONNECTED, addr, status));
            }
        }

        @Override
        public void onServicesDiscovered(BluetoothGatt gatt, int status) {
            push(encAddrStatus(EV_SERVICES, gatt.getDevice().getAddress(), status));
        }

        // API 33+ delivers the value directly.
        @Override
        public void onCharacteristicChanged(BluetoothGatt gatt,
                                            BluetoothGattCharacteristic ch, byte[] value) {
            push(encCharValue(EV_NOTIFY, gatt.getDevice().getAddress(),
                    ch.getUuid().toString(), value));
        }

        // Pre-33 path: read the value off the characteristic.
        @Override
        @SuppressWarnings("deprecation")
        public void onCharacteristicChanged(BluetoothGatt gatt,
                                            BluetoothGattCharacteristic ch) {
            if (Build.VERSION.SDK_INT < 33) {
                push(encCharValue(EV_NOTIFY, gatt.getDevice().getAddress(),
                        ch.getUuid().toString(), ch.getValue()));
            }
        }

        @Override
        public void onCharacteristicWrite(BluetoothGatt gatt,
                                          BluetoothGattCharacteristic ch, int status) {
            push(encCharStatus(EV_WRITE, gatt.getDevice().getAddress(),
                    ch.getUuid().toString(), status));
        }

        @Override
        public void onCharacteristicRead(BluetoothGatt gatt,
                                         BluetoothGattCharacteristic ch, byte[] value, int status) {
            push(encRead(gatt.getDevice().getAddress(), ch.getUuid().toString(), status, value));
        }

        @Override
        @SuppressWarnings("deprecation")
        public void onCharacteristicRead(BluetoothGatt gatt,
                                         BluetoothGattCharacteristic ch, int status) {
            if (Build.VERSION.SDK_INT < 33) {
                push(encRead(gatt.getDevice().getAddress(), ch.getUuid().toString(),
                        status, ch.getValue()));
            }
        }

        @Override
        public void onDescriptorWrite(BluetoothGatt gatt,
                                      BluetoothGattDescriptor desc, int status) {
            push(encAddrStatus(EV_DESC_WRITE, gatt.getDevice().getAddress(), status));
        }
    };

    public boolean connect(String address) {
        if (adapter == null) {
            return false;
        }
        BluetoothDevice dev;
        try {
            dev = adapter.getRemoteDevice(address);
        } catch (IllegalArgumentException e) {
            return false;
        }
        BluetoothGatt g = dev.connectGatt(ctx, false, gattCb, BluetoothDevice.TRANSPORT_LE);
        if (g == null) {
            return false;
        }
        gatts.put(address, g);
        return true;
    }

    public void disconnect(String address) {
        BluetoothGatt g = gatts.remove(address);
        if (g != null) {
            try {
                g.disconnect();
                g.close();
            } catch (Exception ignored) {
            }
        }
    }

    public boolean discoverServices(String address) {
        BluetoothGatt g = gatts.get(address);
        return g != null && g.discoverServices();
    }

    // getServiceUuids returns a comma-separated list of the discovered service
    // UUIDs (empty string if none / unknown device).
    public String getServiceUuids(String address) {
        BluetoothGatt g = gatts.get(address);
        if (g == null) {
            return "";
        }
        StringBuilder sb = new StringBuilder();
        for (BluetoothGattService s : g.getServices()) {
            if (sb.length() > 0) {
                sb.append(',');
            }
            sb.append(s.getUuid().toString());
        }
        return sb.toString();
    }

    // getCharUuids returns the characteristic UUIDs of the first service whose
    // UUID matches, comma-separated.
    public String getCharUuids(String address, String serviceUuid) {
        BluetoothGattService svc = findService(address, serviceUuid);
        if (svc == null) {
            return "";
        }
        StringBuilder sb = new StringBuilder();
        for (BluetoothGattCharacteristic c : svc.getCharacteristics()) {
            if (sb.length() > 0) {
                sb.append(',');
            }
            sb.append(c.getUuid().toString());
        }
        return sb.toString();
    }

    // writeCharacteristic returns 0 on success (dispatch accepted), negative on
    // a lookup failure, or the platform status code on API 33+.
    public int writeCharacteristic(String address, String serviceUuid, String charUuid,
                                   byte[] value, boolean withResponse) {
        BluetoothGatt g = gatts.get(address);
        if (g == null) {
            return -1;
        }
        BluetoothGattCharacteristic c = findChar(address, serviceUuid, charUuid);
        if (c == null) {
            return -2;
        }
        int writeType = withResponse
                ? BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT
                : BluetoothGattCharacteristic.WRITE_TYPE_NO_RESPONSE;
        if (Build.VERSION.SDK_INT >= 33) {
            return g.writeCharacteristic(c, value, writeType); // 0 == SUCCESS
        }
        //noinspection deprecation
        c.setWriteType(writeType);
        //noinspection deprecation
        c.setValue(value);
        //noinspection deprecation
        return g.writeCharacteristic(c) ? 0 : -3;
    }

    public boolean readCharacteristic(String address, String serviceUuid, String charUuid) {
        BluetoothGatt g = gatts.get(address);
        if (g == null) {
            return false;
        }
        BluetoothGattCharacteristic c = findChar(address, serviceUuid, charUuid);
        return c != null && g.readCharacteristic(c);
    }

    // setNotify enables/disables notifications locally and writes the CCCD.
    // Returns 0 on success (or when the characteristic has no CCCD), negative on
    // failure.
    public int setNotify(String address, String serviceUuid, String charUuid, boolean enable) {
        BluetoothGatt g = gatts.get(address);
        if (g == null) {
            return -1;
        }
        BluetoothGattCharacteristic c = findChar(address, serviceUuid, charUuid);
        if (c == null) {
            return -2;
        }
        if (!g.setCharacteristicNotification(c, enable)) {
            return -3;
        }
        BluetoothGattDescriptor d = c.getDescriptor(CCCD);
        if (d == null) {
            return 0; // nothing to write; local notify is enough
        }
        byte[] val = enable
                ? BluetoothGattDescriptor.ENABLE_NOTIFICATION_VALUE
                : BluetoothGattDescriptor.DISABLE_NOTIFICATION_VALUE;
        if (Build.VERSION.SDK_INT >= 33) {
            return g.writeDescriptor(d, val); // 0 == SUCCESS
        }
        //noinspection deprecation
        d.setValue(val);
        //noinspection deprecation
        return g.writeDescriptor(d) ? 0 : -4;
    }

    // ---------------------------------------------------------------- helpers

    private BluetoothGattService findService(String address, String serviceUuid) {
        BluetoothGatt g = gatts.get(address);
        if (g == null) {
            return null;
        }
        UUID want = UUID.fromString(serviceUuid);
        for (BluetoothGattService s : g.getServices()) {
            if (s.getUuid().equals(want)) {
                return s;
            }
        }
        return null;
    }

    private BluetoothGattCharacteristic findChar(String address, String serviceUuid,
                                                 String charUuid) {
        BluetoothGattService s = findService(address, serviceUuid);
        if (s == null) {
            return null;
        }
        UUID want = UUID.fromString(charUuid);
        for (BluetoothGattCharacteristic c : s.getCharacteristics()) {
            if (c.getUuid().equals(want)) {
                return c;
            }
        }
        return null;
    }
}
