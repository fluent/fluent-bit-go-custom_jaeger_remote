# Custom Go Plugin for Jaeger

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

For custom jeager plugin, we need to add custom section in fluent-bit conf:

```
[CUSTOM]
    Name jaeger_remote
    serverURL "http://localhost:14268"
    samplingURL "http://localhost:5778/sampling"
    rate 5s
```

For plugins.yaml,

```yaml
plugins:
    - /path/to/custom_jeager_remote.so
```

## Run

Put c-chared object in the specified path and run fluent-bit:

```
$ /path/to/fluent-bit -c fluent-bit.conf
```
