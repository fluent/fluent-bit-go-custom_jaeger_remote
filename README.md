# Custom Go Plugin for Jaeger

**Note:** This plugin is under heavily development and needed to use with this Fluent Bit's PR: https://github.com/fluent/fluent-bit/pull/10299

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
    server_url "http://localhost:14268"
    sampling_url "http://localhost:5778/sampling"
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
