#!/usr/bin/env python3
"""Emite una factura de prueba contra la API REST del spooler fiscal.

    SPOOLER_TOKEN=mi-token python invoice.py
    SPOOLER_URL=http://192.168.1.10:8080 STATION=ECOM-01 python invoice.py

Sólo usa la biblioteca estándar (urllib), sin pip install.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request


def env(key: str, default: str) -> str:
    return os.environ.get(key) or default


def main() -> int:
    base = env("SPOOLER_URL", "http://127.0.0.1:8080").rstrip("/")
    station = env("STATION", "ECOM-01")
    order = env("ORDER_ID", "pedido-1001")
    token = os.environ.get("SPOOLER_TOKEN", "")
    if not token:
        print("Defina SPOOLER_TOKEN con el token del spooler.", file=sys.stderr)
        return 1

    payload = {
        "station": station,
        "client_ref": order,
        "items": [
            {"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
            {"description": "PAN", "quantity": 2, "unit_price": 5.00, "tax": "reduced"},
        ],
    }
    req = urllib.request.Request(
        f"{base}/v1/invoices/structured",
        data=json.dumps(payload).encode("utf-8"),
        method="POST",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
            "Idempotency-Key": order,
        },
    )

    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            status = resp.status
            body = resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        print(f"ERROR {exc.code}: {exc.read().decode('utf-8', 'replace')}", file=sys.stderr)
        return 1
    except urllib.error.URLError as exc:
        print(f"Error de red: {exc.reason}", file=sys.stderr)
        return 1

    if status != 201:
        print(f"ERROR {status}: {body}", file=sys.stderr)
        return 1

    factura = json.loads(body)
    print(
        f"Factura {factura['invoice_number']} emitida: "
        f"total {factura['totals']['total']:.2f} (secuencia {factura['sequence']})"
    )
    texto = (factura.get("document") or {}).get("response_text", "")
    if texto:
        print(f"Impresora: {texto}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
