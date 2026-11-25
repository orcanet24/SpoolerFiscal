#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Probador TFHKA por COM virtual (ventana Tkinter, sin dependencias).

Sirve para probar el spooler fiscal sin el demo VB.NET:
  - Abre el OTRO extremo del par de puertos (Virtual Serial Port Driver).
    Si el agente abre COM2, este probador abre COM1.
  - Habla el protocolo HKA real: tramas STX/ETX/LRC y el handshake ENQ/ACK
    de los reportes (bytes 0x05/0x06 sueltos, sin envolver).
  - Muestra la respuesta cruda en hex y la interpreta.

Requiere: Windows + Python 3 con tkinter (el Python oficial lo trae).
No usa pyserial: habla con la API serie de Windows por ctypes.

Uso:
    python tools\\tfhka_gui.py
o doble clic en tools\\Probar_TFHKA.bat
"""

import ctypes
import ctypes.wintypes as wt
import struct
import threading
import time
import tkinter as tk
from tkinter import filedialog, messagebox, ttk
import winreg

# --------------------------------------------------------------------------
# Puerto serie de Windows por ctypes (sin pyserial)
# --------------------------------------------------------------------------

GENERIC_READ = 0x80000000
GENERIC_WRITE = 0x40000000
OPEN_EXISTING = 3
PURGE_TXCLEAR = 0x0004
PURGE_RXCLEAR = 0x0008
MAXDWORD = 0xFFFFFFFF
INVALID_HANDLE_VALUE = ctypes.c_void_p(-1).value

_k32 = ctypes.WinDLL("kernel32", use_last_error=True)

_k32.CreateFileW.argtypes = [wt.LPCWSTR, wt.DWORD, wt.DWORD, ctypes.c_void_p,
                             wt.DWORD, wt.DWORD, wt.HANDLE]
_k32.CreateFileW.restype = wt.HANDLE
_k32.CloseHandle.argtypes = [wt.HANDLE]
_k32.CloseHandle.restype = wt.BOOL
_k32.SetupComm.argtypes = [wt.HANDLE, wt.DWORD, wt.DWORD]
_k32.SetupComm.restype = wt.BOOL
_k32.SetCommState.argtypes = [wt.HANDLE, ctypes.c_void_p]
_k32.SetCommState.restype = wt.BOOL
_k32.GetCommState.argtypes = [wt.HANDLE, ctypes.c_void_p]
_k32.GetCommState.restype = wt.BOOL
_k32.SetCommTimeouts.argtypes = [wt.HANDLE, ctypes.c_void_p]
_k32.SetCommTimeouts.restype = wt.BOOL
_k32.PurgeComm.argtypes = [wt.HANDLE, wt.DWORD]
_k32.PurgeComm.restype = wt.BOOL
_k32.ReadFile.argtypes = [wt.HANDLE, ctypes.c_void_p, wt.DWORD,
                          ctypes.POINTER(wt.DWORD), ctypes.c_void_p]
_k32.ReadFile.restype = wt.BOOL
_k32.WriteFile.argtypes = [wt.HANDLE, ctypes.c_void_p, wt.DWORD,
                           ctypes.POINTER(wt.DWORD), ctypes.c_void_p]
_k32.WriteFile.restype = wt.BOOL


def _build_dcb(baudrate, bytesize, parity, stopbits, flow):
    """Arma el DCB (28 bytes) para SetCommState."""
    dcb = bytearray(28)
    struct.pack_into("<I", dcb, 0, 28)          # DCBlength
    struct.pack_into("<I", dcb, 4, baudrate)    # BaudRate
    flags = 0x00000001                          # fBinary
    if parity != "N":
        flags |= 0x00000002                     # fParity
    if flow == "rtscts":
        flags |= 0x00000004                     # fOutxCtsFlow
        flags |= 2 << 12                        # fRtsControl = handshake
    elif flow == "xonxoff":
        flags |= (1 << 8) | (1 << 9)            # fOutX / fInX
    struct.pack_into("<I", dcb, 8, flags)
    struct.pack_into("<H", dcb, 12, 0)          # wReserved
    struct.pack_into("<H", dcb, 14, 0)          # XonLim
    struct.pack_into("<H", dcb, 16, 0)          # XoffLim
    dcb[18] = bytesize
    dcb[19] = {"N": 0, "O": 1, "E": 2, "M": 3, "S": 4}[parity]
    dcb[20] = {1: 0, 1.5: 1, 2: 2}[stopbits]
    dcb[21] = 0x11                              # XonChar
    dcb[22] = 0x13                              # XoffChar
    dcb[23] = 0x00
    dcb[24] = 0x00
    dcb[25] = 0x00
    struct.pack_into("<H", dcb, 26, 0)          # wReserved1
    return bytes(dcb)


def _build_timeouts():
    """Lecturas no bloqueantes: ReadFile devuelve lo que haya y vuelve."""
    t = bytearray(20)
    struct.pack_into("<I", t, 0, MAXDWORD)      # ReadIntervalTimeout
    struct.pack_into("<I", t, 4, 0)             # ReadTotalTimeoutMultiplier
    struct.pack_into("<I", t, 8, 0)             # ReadTotalTimeoutConstant
    struct.pack_into("<I", t, 12, 0)            # WriteTotalTimeoutMultiplier
    struct.pack_into("<I", t, 16, 0)            # WriteTotalTimeoutConstant
    return bytes(t)


class WinSerial:
    def __init__(self):
        self.h = None
        self.port = None

    @property
    def is_open(self):
        return self.h is not None

    def open(self, port, baudrate=9600, bytesize=8, parity="N", stopbits=1, flow="none"):
        self.close()
        path = "\\\\.\\" + port
        h = _k32.CreateFileW(path, GENERIC_READ | GENERIC_WRITE, 0, None,
                             OPEN_EXISTING, 0, None)
        if not h or h == INVALID_HANDLE_VALUE:
            err = ctypes.get_last_error()
            raise OSError(err, f"No se pudo abrir {port}: {ctypes.FormatError(err)} "
                               f"(si el demo VB la tiene abierta, ciérralo primero)")
        try:
            if not _k32.SetupComm(h, 4096, 4096):
                raise OSError(ctypes.get_last_error(), "SetupComm")
            dcb = _build_dcb(baudrate, bytesize, parity, stopbits, flow)
            dcb_buf = ctypes.create_string_buffer(dcb, len(dcb))
            if not _k32.SetCommState(h, ctypes.byref(dcb_buf)):
                raise OSError(ctypes.get_last_error(), "SetCommState")
            tmo = _build_timeouts()
            tmo_buf = ctypes.create_string_buffer(tmo, len(tmo))
            if not _k32.SetCommTimeouts(h, ctypes.byref(tmo_buf)):
                raise OSError(ctypes.get_last_error(), "SetCommTimeouts")
            _k32.PurgeComm(h, PURGE_RXCLEAR | PURGE_TXCLEAR)
        except Exception:
            _k32.CloseHandle(h)
            raise
        self.h = h
        self.port = port

    def close(self):
        if self.h:
            _k32.CloseHandle(self.h)
            self.h = None
            self.port = None

    def write(self, data):
        if not self.h:
            raise OSError("Puerto cerrado")
        data = bytes(data)
        if not data:
            return 0
        buf = ctypes.create_string_buffer(data, len(data))
        written = wt.DWORD(0)
        if not _k32.WriteFile(self.h, ctypes.byref(buf), len(data),
                              ctypes.byref(written), None):
            raise OSError(ctypes.get_last_error(), "WriteFile")
        return written.value

    def read_available(self, maxlen=4096):
        if not self.h:
            return b""
        buf = ctypes.create_string_buffer(maxlen)
        n = wt.DWORD(0)
        if not _k32.ReadFile(self.h, ctypes.byref(buf), maxlen,
                             ctypes.byref(n), None):
            return b""
        return buf.raw[: n.value]

    def read_until_idle(self, idle_ms=350, total_ms=8000, max_bytes=65536):
        """Lee hasta que el emisor guarda silencio idle_ms o vence total_ms."""
        out = bytearray()
        deadline = time.monotonic() + total_ms / 1000.0
        last = time.monotonic()
        while len(out) < max_bytes:
            if time.monotonic() >= deadline:
                break
            chunk = self.read_available()
            if chunk:
                out.extend(chunk)
                last = time.monotonic()
            else:
                if out and (time.monotonic() - last) * 1000.0 >= idle_ms:
                    break
                time.sleep(0.01)
        return bytes(out)


# --------------------------------------------------------------------------
# Protocolo TFHKA
# --------------------------------------------------------------------------

STX = 0x02
ETX = 0x03
ENQ = 0x05
ACK = 0x06
NAK = 0x15


def lrc(data):
    x = 0
    for b in data:
        x ^= b
    return x


def tfhka_frame(cmd):
    """Envuelve un comando de texto en STX + comando + ETX + LRC."""
    cmd = bytes(cmd).rstrip(b"\r\n")
    body = cmd + bytes([ETX])
    return bytes([STX]) + body + bytes([lrc(body)])


def parse_frame(data):
    """Devuelve (payload, lrc_ok) de la primera trama STX...ETX."""
    i = data.find(bytes([STX]))
    if i < 0:
        return data, False
    j = data.find(bytes([ETX]), i + 1)
    if j < 0:
        return data[i + 1:], False
    payload = data[i + 1:j]
    if j + 1 < len(data):
        return payload, data[j + 1] == lrc(data[i + 1:j + 1])
    return payload, False


ERROR_NAMES = {
    0x00: "Sin error",
    0x01: "Fin de papel",
    0x02: "Error mecánico de papel",
    0x03: "Fin de papel + error mecánico",
    0x50: "Comando inválido",
    0x54: "Tasa inválida",
    0x58: "Sin directivas",
    0x5C: "Comando inválido",
    0x60: "Error fiscal",
    0x64: "Error de memoria fiscal",
    0x6C: "Memoria fiscal llena",
    0x70: "Buffer completo",
    0x80: "Error de comunicación",
    0x89: "Sin respuesta",
    0x90: "Error de LRC",
    0x91: "Error interno",
    0x99: "Error de archivo",
}

STATUS_NAMES = {
    0x01: "Modo prueba - en espera",
    0x02: "Modo prueba - emisión fiscal",
    0x03: "Modo prueba - emisión no fiscal",
    0x04: "Modo fiscal - en espera",
    0x05: "Modo fiscal - emisión fiscal",
    0x06: "Modo fiscal - emisión no fiscal",
    0x07: "Memoria casi llena - en espera",
    0x08: "Memoria casi llena - emisión fiscal",
    0x09: "Memoria casi llena - emisión no fiscal",
    0x0A: "Memoria llena - en espera",
    0x0B: "Memoria llena - emisión fiscal",
    0x0C: "Memoria llena - emisión no fiscal",
}


def describe_status(payload, ok):
    lines = ["LRC: " + ("OK" if ok else "inválido o ausente"),
             "Payload: " + payload.hex(" ").upper()]
    if payload:
        b = payload[0]
        name = ERROR_NAMES.get(b) or STATUS_NAMES.get(b)
        lines.append(f"Byte 0: 0x{b:02X}" + (f" — {name}" if name else " — desconocido"))
    if len(payload) >= 2:
        b = payload[1]
        name = ERROR_NAMES.get(b) or STATUS_NAMES.get(b)
        lines.append(f"Byte 1: 0x{b:02X}" + (f" — {name}" if name else " — desconocido"))
    txt = payload.decode("latin-1", "replace").strip()
    if txt and any(32 <= ord(c) < 127 for c in txt):
        lines.append("Texto: " + repr(txt))
    return "\n".join(lines)


def _lines_after_prefix(text, prefix):
    t = text.strip()
    if t.startswith(prefix):
        t = t[len(prefix):].lstrip("\r\n")
    return [ln.strip().replace("*", "") for ln in t.replace("\r", "").split("\n")]


def parse_s1(text):
    lines = _lines_after_prefix(text, "S1")
    out = {}
    if len(lines) >= 14:
        out["Última factura"] = lines[2]
        out["Última NC"] = lines[6]
        out["Última ND"] = lines[7]
        out["Último DNF"] = lines[8]
        out["Último cierre Z"] = lines[11]
        out["RIF"] = lines[12]
        out["Serial"] = lines[13]
    if len(lines) >= 16:
        out["Hora"] = lines[14]
        out["Fecha"] = lines[15]
    return out


def parse_s5(text):
    lines = _lines_after_prefix(text, "S5")
    out = {}
    if len(lines) >= 5:
        out["RIF"] = lines[0]
        out["Serial"] = lines[1]
        out["Nro auditoría"] = lines[2]
        out["Capacidad total (KB)"] = lines[3]
        out["Capacidad libre (KB)"] = lines[4]
    if len(lines) >= 6:
        out["Documentos"] = lines[5]
    return out


def list_serial_ports():
    ports = []
    try:
        key = winreg.OpenKey(winreg.HKEY_LOCAL_MACHINE,
                             r"HARDWARE\DEVICEMAP\SERIALCOMM")
        i = 0
        while True:
            try:
                _, val, _ = winreg.EnumValue(key, i)
            except OSError:
                break
            ports.append(val)
            i += 1
        winreg.CloseKey(key)
    except OSError:
        pass

    def sort_key(p):
        try:
            return (0, int(p[3:]))
        except ValueError:
            return (1, p)

    return sorted(set(ports), key=sort_key)


# --------------------------------------------------------------------------
# Ventana
# --------------------------------------------------------------------------

class App:
    def __init__(self, root):
        self.root = root
        self.ser = WinSerial()
        self.busy = threading.Lock()
        self._build()
        self.refresh_ports()

    # ---------- utilidades de UI ----------

    def _ui(self, fn, *args):
        self.root.after(0, lambda: fn(*args))

    def log_tx(self, data):
        self._append_log("TX", data)

    def log_rx(self, data):
        self._append_log("RX", data)

    def _append_log(self, tag, data):
        hexs = " ".join(f"{b:02X}" for b in data)
        self._ui(self._log_line, f"{tag} {hexs}")

    def _log_line(self, line):
        self.txt_log.insert("end", line + "\n")
        self.txt_log.see("end")

    def info_set(self, text):
        self._ui(self._info_set, text)

    def _info_set(self, text):
        self.txt_info.delete("1.0", "end")
        self.txt_info.insert("1.0", text)

    def _set_estado(self, text, color):
        self.lbl_estado.configure(text=text, foreground=color)

    def _bg(self, fn, *args):
        if not self.busy.acquire(blocking=False):
            self.info_set("Hay una operación en curso; espera a que termine.")
            return

        def run():
            try:
                fn(*args)
            except Exception as exc:  # noqa: BLE001
                self.info_set(f"ERROR: {exc}")
            finally:
                self.busy.release()

        threading.Thread(target=run, daemon=True).start()

    # ---------- armado ----------

    def _build(self):
        self.root.title("Probador TFHKA — Spooler Fiscal")
        self.root.geometry("1180x760")
        self.root.minsize(1000, 640)

        footer = ttk.Label(
            self.root,
            text="Abre COM1 (el otro extremo del par). El agente abre COM2. "
                 "Cierra el demo VB antes de abrir COM1. 9600 8N1, sin flujo.",
            foreground="#555555",
        )
        footer.pack(side="bottom", fill="x", padx=10, pady=6)

        body = ttk.Frame(self.root)
        body.pack(side="top", fill="both", expand=True, padx=10, pady=6)

        top = ttk.LabelFrame(body, text="Funciones de Comunicación")
        top.pack(fill="x", pady=(0, 6))
        ttk.Label(top, text="Puerto:").grid(row=0, column=0, padx=6, pady=6, sticky="w")
        self.cmb_port = ttk.Combobox(top, width=10, state="readonly")
        self.cmb_port.grid(row=0, column=1, padx=4, pady=6)
        ttk.Button(top, text="Actualizar", command=self.refresh_ports).grid(row=0, column=2, padx=4)
        ttk.Button(top, text="Abrir", command=self.on_open).grid(row=0, column=3, padx=4)
        ttk.Button(top, text="Cerrar", command=self.on_close).grid(row=0, column=4, padx=4)
        self.lbl_estado = ttk.Label(top, text="Puerto cerrado", foreground="red")
        self.lbl_estado.grid(row=0, column=5, padx=14)

        columns = ttk.Frame(body)
        columns.pack(fill="both", expand=True)

        left = ttk.Frame(columns)
        left.pack(side="left", fill="y", padx=(0, 8))

        box1 = ttk.LabelFrame(left, text="Comunicación y estado")
        box1.pack(fill="x", pady=4)
        for text, cmd in (
            ("Verificar Conexión", self.act_check),
            ("Estado y Error", self.act_status),
            ("Verificar Modelo y País", self.act_sv),
            ("Abrir Gaveta", self.act_drawer),
        ):
            ttk.Button(box1, text=text, command=cmd).pack(fill="x", padx=4, pady=2)

        box2 = ttk.LabelFrame(left, text="Reportes y Status")
        box2.pack(fill="x", pady=4)
        for text, cmd in (("Leer S1", b"S1"), ("Leer S5", b"S5"), ("Leer S2", b"S2")):
            ttk.Button(box2, text=text,
                       command=lambda c=cmd: self.act_text_cmd(c)).pack(fill="x", padx=4, pady=2)
        ttk.Button(box2, text="Leer Último Reporte Z",
                   command=lambda: self.act_report(b"U0Z")).pack(fill="x", padx=4, pady=2)
        ttk.Button(box2, text="Reporte X",
                   command=lambda: self.act_report(b"U0X")).pack(fill="x", padx=4, pady=2)

        box3 = ttk.LabelFrame(left, text="Envío de Comandos")
        box3.pack(fill="x", pady=4)
        self.ent_cmd = ttk.Entry(box3)
        self.ent_cmd.pack(fill="x", padx=4, pady=2)
        self.ent_cmd.bind("<Return>", lambda _e: self.act_send())
        ttk.Button(box3, text="Enviar comando", command=self.act_send).pack(fill="x", padx=4, pady=2)
        ttk.Button(box3, text="Enviar archivo...", command=self.act_send_file).pack(fill="x", padx=4, pady=2)
        ttk.Button(box3, text="Limpiar registro", command=self.clear_log).pack(fill="x", padx=4, pady=8)

        right = ttk.Frame(columns)
        right.pack(side="left", fill="both", expand=True)

        info_box = ttk.LabelFrame(right, text="Información")
        info_box.pack(fill="both", expand=True, pady=4)
        self.txt_info = tk.Text(info_box, height=16, wrap="word", font=("Consolas", 10))
        self.txt_info.pack(fill="both", expand=True, padx=2, pady=2)

        log_box = ttk.LabelFrame(right, text="Registro TX/RX (hex)")
        log_box.pack(fill="both", expand=True, pady=4)
        self.txt_log = tk.Text(log_box, height=12, wrap="none", font=("Consolas", 9))
        self.txt_log.pack(fill="both", expand=True, padx=2, pady=2)

    # ---------- puerto ----------

    def refresh_ports(self):
        ports = list_serial_ports()
        self.cmb_port["values"] = ports
        if ports:
            current = self.cmb_port.get()
            if current not in ports:
                self.cmb_port.set("COM1" if "COM1" in ports else ports[0])
        else:
            self.cmb_port.set("")

    def on_open(self):
        port = self.cmb_port.get().strip()
        if not port:
            messagebox.showwarning("Puerto", "Selecciona un puerto COM.")
            return

        def work():
            self.ser.open(port)
            self._ui(self._set_estado, f"{port} abierto", "green")
            self.info_set(f"Puerto {port} abierto a 9600 8N1, sin control de flujo.\n\n"
                          "Pulsa «Verificar Conexión» para mandar el ENQ (0x05).")

        self._bg(work)

    def on_close(self):
        self.ser.close()
        self._set_estado("Puerto cerrado", "red")
        self.info_set("Puerto cerrado.")

    def clear_log(self):
        self.txt_log.delete("1.0", "end")

    # ---------- operaciones ----------

    def _transact(self, frame_bytes):
        if not self.ser.is_open:
            raise OSError("Abre el puerto primero.")
        self.log_tx(frame_bytes)
        self.ser.write(frame_bytes)
        resp = self.ser.read_until_idle()
        self.log_rx(resp)
        return resp

    def act_check(self):
        def work():
            if not self.ser.is_open:
                raise OSError("Abre el puerto primero.")
            self.log_tx(bytes([ENQ]))
            self.ser.write(bytes([ENQ]))
            resp = self.ser.read_until_idle()
            self.log_rx(resp)
            if not resp:
                self.info_set(
                    "SIN RESPUESTA al ENQ (0x05).\n\n"
                    "La impresora no contestó:\n"
                    "  - revisa que el agente esté abierto en COM2 y que el demo VB no tenga COM1,\n"
                    "  - revisa que el par de puertos sea el correcto,\n"
                    "  - mira el log del agente (debe aparecer un comando 05).")
                self._ui(self._set_estado, "Sin respuesta", "red")
                return
            payload, ok = parse_frame(resp)
            self.info_set("CONEXIÓN OK: la impresora respondió al ENQ.\n\n"
                          + describe_status(payload, ok))
            self._ui(self._set_estado, "Conectada", "green")

        self._bg(work)

    def act_status(self):
        def work():
            if not self.ser.is_open:
                raise OSError("Abre el puerto primero.")
            self.log_tx(bytes([ENQ]))
            self.ser.write(bytes([ENQ]))
            resp = self.ser.read_until_idle()
            self.log_rx(resp)
            if not resp:
                self.info_set("SIN RESPUESTA al ENQ. La impresora no contestó.")
                return
            payload, ok = parse_frame(resp)
            self.info_set("Estado y Error (respuesta al ENQ 0x05)\n\n"
                          + describe_status(payload, ok))

        self._bg(work)

    def act_sv(self):
        def work():
            resp = self._transact(tfhka_frame(b"SV"))
            payload, ok = parse_frame(resp)
            text = payload.decode("latin-1", "replace")
            lines = _lines_after_prefix(text, "SV")
            lines = [ln for ln in lines if ln]
            model = lines[0] if lines else "?"
            country = lines[1] if len(lines) > 1 else "?"
            self.info_set(
                "Modelo y País (comando SV)\n\n"
                f"Modelo: {model}\nPaís: {country}\n"
                f"LRC: {'OK' if ok else 'inválido'}\n"
                f"Payload: {payload.hex(' ').upper()}")

        self._bg(work)

    def act_drawer(self):
        def work():
            resp = self._transact(tfhka_frame(b"0"))
            payload, ok = parse_frame(resp)
            self.info_set("Abrir gaveta (comando 0)\n\n" + describe_status(payload, ok))

        self._bg(work)

    def act_text_cmd(self, cmd):
        def work():
            resp = self._transact(tfhka_frame(cmd))
            payload, ok = parse_frame(resp)
            text = payload.decode("latin-1", "replace").replace("\r", "")
            if cmd == b"S1":
                parsed = parse_s1(text)
            elif cmd == b"S5":
                parsed = parse_s5(text)
            else:
                parsed = {}
            body = "\n".join(f"{k}: {v}" for k, v in parsed.items()) if parsed else text
            self.info_set(
                f"[{cmd.decode()}]  LRC {'OK' if ok else 'inválido'}\n\n"
                f"{body}\n\n--- respuesta cruda (sin STX/ETX/LRC) ---\n{text}")

        self._bg(work)

    def act_report(self, cmd):
        def work():
            if not self.ser.is_open:
                raise OSError("Abre el puerto primero.")
            first = tfhka_frame(cmd)
            self.log_tx(first)
            self.ser.write(first)
            pending = self.ser.read_until_idle(idle_ms=400, total_ms=6000)
            self.log_rx(pending)
            if not pending:
                self.info_set(f"[{cmd.decode()}] sin respuesta.")
                return
            acc = bytearray()
            for _ in range(64):
                if not pending:
                    break
                if pending == bytes([ENQ]):
                    self.log_tx(bytes([ACK]))
                    self.ser.write(bytes([ACK]))
                    pending = self.ser.read_until_idle(idle_ms=500, total_ms=8000)
                    self.log_rx(pending)
                    continue
                acc.extend(pending)
                pending = self.ser.read_until_idle(idle_ms=300, total_ms=3000)
                self.log_rx(pending)
            payload, ok = parse_frame(bytes(acc))
            text = payload.decode("latin-1", "replace").replace("\r", "")
            self.info_set(
                f"[{cmd.decode()}]  LRC {'OK' if ok else 'inválido'}\n\n{text}")

        self._bg(work)

    def act_send(self):
        text = self.ent_cmd.get()
        if not text:
            return

        def work():
            resp = self._transact(tfhka_frame(text.encode("latin-1", "replace")))
            payload, ok = parse_frame(resp)
            self.info_set(
                f"Comando enviado: {text!r}\n"
                f"LRC respuesta: {'OK' if ok else 'inválido'}\n"
                f"Payload: {payload.hex(' ').upper()}\n\n"
                + payload.decode("latin-1", "replace"))

        self._bg(work)

    def act_send_file(self):
        path = filedialog.askopenfilename(
            title="Archivo de comandos",
            filetypes=[("Texto", "*.txt"), ("Todos", "*.*")])
        if not path:
            return

        def work():
            with open(path, "r", encoding="latin-1") as fh:
                lines = [ln.rstrip("\r\n") for ln in fh]
            sent = 0
            for line in lines:
                if not line.strip():
                    continue
                resp = self._transact(tfhka_frame(line.encode("latin-1", "replace")))
                payload, _ok = parse_frame(resp)
                self._ui(self._log_line,
                         f"   {line!r} -> {payload.decode('latin-1', 'replace').strip()!r}")
                sent += 1
            self.info_set(f"Archivo enviado: {sent} línea(s).\n{path}")

        self._bg(work)


def main():
    root = tk.Tk()
    App(root)
    root.mainloop()


if __name__ == "__main__":
    main()
