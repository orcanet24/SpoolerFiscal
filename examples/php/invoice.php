<?php
/**
 * Emite una factura de prueba contra la API REST del spooler fiscal.
 *
 *   SPOOLER_TOKEN=mi-token php invoice.php
 *   SPOOLER_URL=http://192.168.1.10:8080 STATION=ECOM-01 php invoice.php
 *
 * Requiere la extensión curl (incluida en PHP para Windows y en php-curl en Linux).
 */

declare(strict_types=1);

$base    = getenv('SPOOLER_URL') ?: 'http://127.0.0.1:8080';
$token   = getenv('SPOOLER_TOKEN') ?: '';
$station = getenv('STATION') ?: 'ECOM-01';
$order   = getenv('ORDER_ID') ?: 'pedido-1001';

if ($token === '') {
    fwrite(STDERR, "Defina SPOOLER_TOKEN con el token del spooler.\n");
    exit(1);
}

$payload = [
    'station'    => $station,
    'client_ref' => $order,
    'items'      => [
        ['description' => 'CAFE', 'quantity' => 1, 'unit_price' => 10.00, 'tax' => 'general'],
        ['description' => 'PAN',  'quantity' => 2, 'unit_price' => 5.00,  'tax' => 'reduced'],
    ],
];

$ch = curl_init($base . '/v1/invoices/structured');
curl_setopt_array($ch, [
    CURLOPT_POST           => true,
    CURLOPT_RETURNTRANSFER => true,
    CURLOPT_TIMEOUT        => 60,
    CURLOPT_HTTPHEADER     => [
        'Authorization: Bearer ' . $token,
        'Content-Type: application/json',
        'Idempotency-Key: ' . $order,
    ],
    CURLOPT_POSTFIELDS     => json_encode($payload, JSON_UNESCAPED_UNICODE),
]);

$body   = curl_exec($ch);
$status = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
$error  = curl_error($ch);
curl_close($ch);

if ($body === false) {
    fwrite(STDERR, "Error de red: {$error}\n");
    exit(1);
}
if ($status !== 201) {
    fwrite(STDERR, "ERROR {$status}: {$body}\n");
    exit(1);
}

$factura = json_decode($body, true, 512, JSON_THROW_ON_ERROR);
printf(
    "Factura %d emitida: total %.2f (secuencia %d)\n",
    $factura['invoice_number'],
    $factura['totals']['total'],
    $factura['sequence']
);
if (!empty($factura['document']['response_text'])) {
    echo 'Impresora: ' . $factura['document']['response_text'] . PHP_EOL;
}
