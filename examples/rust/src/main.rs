// Emite una factura de prueba contra la API REST del spooler fiscal.
//
//   SPOOLER_TOKEN=mi-token cargo run
//   SPOOLER_URL=http://192.168.1.10:8080 STATION=ECOM-01 cargo run
//
// Dependencias: ureq (HTTP) y serde_json. Ver Cargo.toml.

use serde_json::{json, Value};
use std::env;

fn env_or(key: &str, default: &str) -> String {
    env::var(key).unwrap_or_else(|_| default.to_string())
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let base = env_or("SPOOLER_URL", "http://127.0.0.1:8080");
    let station = env_or("STATION", "ECOM-01");
    let order = env_or("ORDER_ID", "pedido-1001");
    let token = env::var("SPOOLER_TOKEN")
        .map_err(|_| "Defina SPOOLER_TOKEN con el token del spooler")?;

    let payload = json!({
        "station": station,
        "client_ref": order,
        "items": [
            {"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
            {"description": "PAN",  "quantity": 2, "unit_price": 5.00,  "tax": "reduced"}
        ]
    });

    let result = ureq::post(&format!("{base}/v1/invoices/structured"))
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .set("Idempotency-Key", &order)
        .send_json(payload);

    let response = match result {
        Ok(r) => r,
        Err(ureq::Error::Status(code, r)) => {
            eprintln!("ERROR {code}: {}", r.into_string()?);
            std::process::exit(1);
        }
        Err(e) => return Err(e.into()),
    };

    let body: Value = response.into_json()?;
    let number = body["invoice_number"].as_i64().unwrap_or(0);
    let total = body["totals"]["total"].as_f64().unwrap_or(0.0);
    let sequence = body["sequence"].as_i64().unwrap_or(0);
    println!("Factura {number} emitida: total {total:.2} (secuencia {sequence})");
    if let Some(texto) = body["document"]["response_text"].as_str() {
        println!("Impresora: {texto}");
    }
    Ok(())
}
