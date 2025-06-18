# Custom Go Plugin for Jaeger

**Note:** This plugin is under heavily development.

This plugin implements Jaeger Remote Sampling protocol on Golang cutom plugin mechanism.

## Prerequisites

* Golang 1.23.0 or later

## Building

```
$ go mod download
$ make
```

Then, the c-shared object will be created as:

```
ls *.so
custom_jeager_remote.so
```

## Configuration

For specifying the place of plugin's shared object, we need to specify the path in plugin.yaml:

```yaml
plugins:
    - /path/to/custom_jeager_remote.so
```

For custom jeager plugin, we need to add custom section in fluent-bit conf:

### Basic structure

```ini
[CUSTOM]
    # The name registered in the Go code ("jaeger_remote")
    Name         jaeger_remote

    # --- Mode Selection ---
    # Can be: "client", "server", or "all" (default)
    Mode         all

    # --- Add parameters for the selected mode below ---
```

### Mode: client

In this mode, the plugin acts as an OpenTelemetry client, fetching sampling strategies from an external server.

```ini
[CUSTOM]
    Name                jaeger_remote
    Mode                client

    # URL of the OTLP-compatible collector to send traces to.
    client.server_url   http://localhost:4318

    # URL of the Jaeger-compatible sampling server.
    client.sampling_url http://localhost:5778/sampling
```

### Mode: Server

In this mode, the plugin polls a Jaeger Collector for strategies and serves them via its own HTTP and/or gRPC endpoints. You can enable one or both by providing their listen addresses.

```ini
[CUSTOM]
    Name                jaeger_remote
    Mode                server

    # --- Connection to Jaeger Collector ---
    server.endpoint      jaeger-collector:14250
    server.service_names frontend,backend,database

    # --- Exposed Endpoints (enable one or both) ---
    server.http.listen_addr 0.0.0.0:8899
    server.grpc.listen_addr 0.0.0.0:9099

    # --- Advanced Connection Settings (Optional) ---
    server.retry.initial_interval 10s
    server.keepalive.time         30s
```

### Mode: server (Local File)

This mode loads a static strategy file and serves it via HTTP and/or gRPC. It does not connect to a remote Jaeger Collector.

```
[CUSTOM]
    Name                   jaeger_remote
    Mode                   server

    # --- Local Strategy File ---
    server.strategy_file   /path/to/my_strategies.json

    # --- Exposed Endpoints ---
    server.http.listen_addr  0.0.0.0:8899
```

### Mode: All

This mode enables both client and server functionalities simultaneously.

```ini
[CUSTOM]
    Name                jaeger_remote
    Mode                all

    # --- Client Parameters ---
    client.server_url   http://localhost:4318
    client.sampling_url http://localhost:5778/sampling

    # --- Server Parameters ---
    server.endpoint      jaeger-collector:14250
    server.service_names frontend,backend,database
    server.http.listen_addr 0.0.0.0:8899
    server.grpc.listen_addr 0.0.0.0:9099
```

## Parameter Reference

| Key                                      | Mode          | Description                                                                                             | Default                  |
| ---------------------------------------- | ------------- | ------------------------------------------------------------------------------------------------------- | ------------------------ |
| `mode`                                   | **Global** | Sets the operating mode. Can be `client`, `server`, or `all`.                                             | `all`                    |
| **Client Settings** |               |                                                                                                         |                          |
| `client.server_url`                      | `client`/`all`  | The endpoint URL of the OTLP collector to which traces will be sent.                                      | **Required** |
| `client.sampling_url`                    | `client`/`all`  | The URL of the Jaeger-compatible sampling server to poll for strategies.                                | **Required** |
| **Server Settings** |               |                                                                                                         |                          |
| `server.endpoint`                        | `server`/`all`  | The gRPC endpoint of the Jaeger Collector to poll for sampling strategies. **Mutually exclusive** with server.strategy_file. | **Required** |
| `server.strategy_file`                   | `server`/`all`  | Path to a local JSON file containing sampling strategies. **Mutually exclusive** with `server.endpoint`.    | ` ` (Disabled)           |
| `server.service_names`                   | `server`/`all`  | A comma-separated list of service names to fetch strategies for.                                        | **Required** |
| `server.http.listen_addr`                | `server`/`all`  | The address and port for the internal HTTP server to listen on. If empty, the HTTP server is disabled.    | ` ` (Disabled)           |
| `server.grpc.listen_addr`                | `server`/`all`  | The address and port for the internal gRPC server to listen on. If empty, the gRPC server is disabled.   | ` ` (Disabled)           |
| `server.headers`                         | `server`/`all`  | Comma-separated key=value pairs to add as gRPC metadata to requests to the Jaeger Collector.          | ` `                      |
| **Server TLS Settings** |               |                                                                                                         |                          |
| `server.tls.insecure`                    | `server`/`all`  | If `true`, TLS certificate verification is skipped when connecting to the Jaeger Collector.               | `false`                  |
| `server.tls.server_name_override`        | `server`/`all`  | Overrides the server name used for TLS validation.                                                      | ` `                      |
| `server.tls.ca_file`                     | `server`/`all`  | Path to the CA certificate file for verifying the Jaeger Collector's certificate.                       | ` `                      |
| `server.tls.cert_file`                   | `server`/`all`  | Path to the client's TLS certificate file.                                                              | ` `                      |
| `server.tls.key_file`                    | `server`/`all`  | Path to the client's TLS private key file.                                                              | ` `                      |
| **Server Keepalive Settings** |               |                                                                                                         |                          |
| `server.keepalive.time`                  | `server`/`all`  | Interval to send keepalive pings to the Jaeger Collector. If not set, keepalive is disabled.            | ` ` (Disabled)           |
| `server.keepalive.timeout`               | `server`/`all`  | Time to wait for a keepalive ack before considering the connection dead.                                | `20s`                    |
| `server.keepalive.permit_without_stream` | `server`/`all`  | If `true`, allows pings to be sent even if there are no active streams.                               | `true`                   |
| **Server Retry Settings** |               |                                                                                                         |                          |
| `server.retry.initial_interval`          | `server`/`all`  | The initial time to wait before retrying a failed connection to the Jaeger Collector.                   | `5s`                     |
| `server.retry.max_interval`              | `server`/`all`  | The maximum time to wait between retries.                                                               | `5m`                     |
| `server.retry.multiplier`                | `server`/`all`  | The factor by which the retry interval is multiplied after each failed attempt. Must be > 1.0.            | `1.5`                    |
| `server.retry.max_retry`                 | `server`/`all`  | The factor by which the maximum retries after each failed attempt. Must be > 1.0.            | `10`                    |

## Run

Put c-chared object in the specified path and run fluent-bit:

```
$ /path/to/fluent-bit -c fluent-bit.conf
```
