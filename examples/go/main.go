// Emite una factura de prueba contra la API REST del spooler fiscal.
//
//	SPOOLER_TOKEN=mi-token go run .
//	SPOOLER_URL=http://192.168.1.10:8080 STATION=ECOM-01 go run .
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type item struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	Tax         string  `json:"tax,omitempty"`
}

type invoiceRequest struct {
	Station   string `json:"station"`
	ClientRef string `json:"client_ref,omitempty"`
	Items     []item `json:"items"`
}

type invoiceResponse struct {
	InvoiceNumber int64 `json:"invoice_number"`
	Sequence      int64 `json:"sequence"`
	Totals        struct {
		Subtotal float64 `json:"subtotal"`
		Tax      float64 `json:"tax"`
		Total    float64 `json:"total"`
	} `json:"totals"`
	Document *struct {
		ResponseText string `json:"response_text"`
	} `json:"document"`
}

func main() {
	base := env("SPOOLER_URL", "http://127.0.0.1:8080")
	station := env("STATION", "ECOM-01")
	order := env("ORDER_ID", "pedido-1001")
	token := os.Getenv("SPOOLER_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "Defina SPOOLER_TOKEN con el token del spooler.")
		os.Exit(1)
	}

	payload := invoiceRequest{
		Station:   station,
		ClientRef: order,
		Items: []item{
			{Description: "CAFE", Quantity: 1, UnitPrice: 10.00, Tax: "general"},
			{Description: "PAN", Quantity: 2, UnitPrice: 5.00, Tax: "reduced"},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serializar:", err)
		os.Exit(1)
	}

	req, err := http.NewRequest(http.MethodPost, base+"/v1/invoices/structured", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "petición:", err)
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", order)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "red:", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "leer respuesta:", err)
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "ERROR %d: %s\n", resp.StatusCode, data)
		os.Exit(1)
	}

	var out invoiceResponse
	if err := json.Unmarshal(data, &out); err != nil {
		fmt.Fprintln(os.Stderr, "JSON:", err)
		os.Exit(1)
	}
	fmt.Printf("Factura %d emitida: total %.2f (secuencia %d)\n",
		out.InvoiceNumber, out.Totals.Total, out.Sequence)
	if out.Document != nil && out.Document.ResponseText != "" {
		fmt.Printf("Impresora: %s\n", out.Document.ResponseText)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
