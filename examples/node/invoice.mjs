#!/usr/bin/env node
// Emite una factura de prueba contra la API REST del spooler fiscal.
//
//   SPOOLER_TOKEN=mi-token node invoice.mjs
//   SPOOLER_URL=http://192.168.1.10:8080 STATION=ECOM-01 node invoice.mjs
//
// Requiere Node.js 18 o superior (fetch nativo), sin dependencias.

const base = (process.env.SPOOLER_URL ?? "http://127.0.0.1:8080").replace(/\/$/, "");
const station = process.env.STATION ?? "ECOM-01";
const order = process.env.ORDER_ID ?? "pedido-1001";
const token = process.env.SPOOLER_TOKEN;

if (!token) {
  console.error("Defina SPOOLER_TOKEN con el token del spooler.");
  process.exit(1);
}

const payload = {
  station,
  client_ref: order,
  items: [
    { description: "CAFE", quantity: 1, unit_price: 10.0, tax: "general" },
    { description: "PAN", quantity: 2, unit_price: 5.0, tax: "reduced" },
  ],
};

const res = await fetch(`${base}/v1/invoices/structured`, {
  method: "POST",
  headers: {
    Authorization: `Bearer ${token}`,
    "Content-Type": "application/json",
    "Idempotency-Key": order,
  },
  body: JSON.stringify(payload),
  signal: AbortSignal.timeout(60_000),
});

const text = await res.text();
if (res.status !== 201) {
  console.error(`ERROR ${res.status}: ${text}`);
  process.exit(1);
}

const factura = JSON.parse(text);
console.log(
  `Factura ${factura.invoice_number} emitida: total ${factura.totals.total.toFixed(2)} ` +
    `(secuencia ${factura.sequence})`
);
if (factura.document?.response_text) {
  console.log(`Impresora: ${factura.document.response_text}`);
}
