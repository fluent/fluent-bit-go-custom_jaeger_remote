package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	"github.com/calyptia/plugin"
)

func loadTLSConfig(cfg TLSSettings) (*tls.Config, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: cfg.Insecure, ServerName: cfg.ServerName}
	if cfg.CAFile != "" {
		caBytes, err := ioutil.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		caPool := x509.NewCertPool()
		caPool.AppendCertsFromPEM(caBytes)
		tlsConfig.RootCAs = caPool
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return tlsConfig, nil
}

func parseCorsConfig(cfg *Config, conf plugin.ConfigLoader) (*Config, error) {
	if cfg == nil {
		return nil, errors.New("cfg must not nil")
	}
	if corsStr := conf.String("server.cors.allowed_origins"); corsStr != "" {
		for _, origin := range strings.Split(corsStr, ",") {
			trimmedOrigin := strings.TrimSpace(origin)
			if trimmedOrigin != "" { // Only append non-empty origins
				cfg.ServerCors.AllowedOrigins = append(cfg.ServerCors.AllowedOrigins, trimmedOrigin)
			}
		}
	}

	return cfg, nil
}

func loadConfig(fbit *plugin.Fluentbit) (*Config, error) {
	log := fbit.Logger
	cfg := &Config{
		Mode:                 fbit.Conf.String("mode"),
		ClientServerURL:      fbit.Conf.String("client.server_url"),
		ClientSamplingURL:    fbit.Conf.String("client.sampling_url"),
		ServerEndpoint:       fbit.Conf.String("server.endpoint"),
		ServerStrategyFile:   fbit.Conf.String("server.strategy_file"),
		ServerHttpListenAddr: fbit.Conf.String("server.http.listen_addr"),
		ServerGrpcListenAddr: fbit.Conf.String("server.grpc.listen_addr"),
		ServerHeaders:        parseHeaders(fbit.Conf.String("server.headers")),
	}

	cfg, err := parseCorsConfig(cfg, fbit.Conf)
	if err != nil {
		return nil, err
	}

	if cfg.Mode == "" {
		cfg.Mode = "all"
	}

	if cfg.Mode == "client" || cfg.Mode == "all" {
		if cfg.ClientServerURL == "" {
			return nil, errors.New("'client.server_url' is required for client/all mode")
		}
		if cfg.ClientSamplingURL == "" {
			return nil, errors.New("'client.sampling_url' is required for client/all mode")
		}
	}

	if cfg.Mode == "server" || cfg.Mode == "all" {
		if cfg.ServerEndpoint == "" && cfg.ServerStrategyFile == "" {
			return nil, errors.New("for server mode, either 'server.endpoint' or 'server.strategy_file' must be configured")
		}
		if cfg.ServerEndpoint != "" && cfg.ServerStrategyFile != "" {
			return nil, errors.New("'server.endpoint' and 'server.strategy_file' are mutually exclusive")
		}

		// Remote-only server settings
		if cfg.ServerEndpoint != "" {
			if valStr := fbit.Conf.String("server.reload_interval"); valStr != "" {
				val, err := time.ParseDuration(valStr)
				if err != nil {
					log.Warn("could not parse 'server.reload_interval', using default of 5m. error: %v", err)
					cfg.ServerReloadInterval = 5 * time.Minute
				} else {
					cfg.ServerReloadInterval = val
				}
			} else {
				cfg.ServerReloadInterval = 5 * time.Minute // Default TTL
			}

			cfg.ServerTLS = TLSSettings{
				ServerName: fbit.Conf.String("server.tls.server_name_override"),
				CAFile:     fbit.Conf.String("server.tls.ca_file"),
				CertFile:   fbit.Conf.String("server.tls.cert_file"),
				KeyFile:    fbit.Conf.String("server.tls.key_file"),
			}
			if insecureStr := fbit.Conf.String("server.tls.insecure"); insecureStr != "" {
				cfg.ServerTLS.Insecure, _ = strconv.ParseBool(insecureStr)
			}

			if kaTimeStr := fbit.Conf.String("server.keepalive.time"); kaTimeStr != "" {
				cfg.ServerKeepalive = &KeepaliveConfig{}
				kaTime, err := time.ParseDuration(kaTimeStr)
				if err != nil {
					log.Warn("could not parse 'server.keepalive.time', skipping keepalive config. error: %v", err)
					cfg.ServerKeepalive = nil
				} else {
					cfg.ServerKeepalive.Time = kaTime
					kaTimeoutStr := fbit.Conf.String("server.keepalive.timeout")
					if kaTimeoutStr == "" {
						cfg.ServerKeepalive.Timeout = 20 * time.Second
					} else {
						kaTimeout, err := time.ParseDuration(kaTimeoutStr)
						if err != nil {
							log.Warn("could not parse 'server.keepalive.timeout', using default of 20s. error: %v", err)
							cfg.ServerKeepalive.Timeout = 20 * time.Second
						} else {
							cfg.ServerKeepalive.Timeout = kaTimeout
						}
					}
					kaPermitStr := fbit.Conf.String("server.keepalive.permit_without_stream")
					if kaPermitStr == "" {
						cfg.ServerKeepalive.PermitWithoutStream = true
					} else {
						kaPermit, err := strconv.ParseBool(kaPermitStr)
						if err != nil {
							log.Warn("could not parse 'server.keepalive.permit_without_stream', using default of true. error: %v", err)
							cfg.ServerKeepalive.PermitWithoutStream = true
						} else {
							cfg.ServerKeepalive.PermitWithoutStream = kaPermit
						}
					}
				}
			}

			cfg.ServerRetry = &RetryConfig{}
			if valStr := fbit.Conf.String("server.retry.initial_interval"); valStr != "" {
				val, err := time.ParseDuration(valStr)
				if err != nil {
					log.Warn("could not parse 'server.retry.initial_interval', using default of 5s. error: %v", err)
					cfg.ServerRetry.InitialInterval = 5 * time.Second
				} else {
					cfg.ServerRetry.InitialInterval = val
				}
			} else {
				cfg.ServerRetry.InitialInterval = 5 * time.Second
			}
			if valStr := fbit.Conf.String("server.retry.max_interval"); valStr != "" {
				val, err := time.ParseDuration(valStr)
				if err != nil {
					log.Warn("could not parse 'server.retry.max_interval', using default of 5m. error: %v", err)
					cfg.ServerRetry.MaxInterval = 5 * time.Minute
				} else {
					cfg.ServerRetry.MaxInterval = val
				}
			} else {
				cfg.ServerRetry.MaxInterval = 5 * time.Minute
			}

			if valStr := fbit.Conf.String("server.retry.max_retry"); valStr != "" {
				val, err := strconv.ParseInt(valStr, 10, 0)
				if err != nil {
					log.Warn("could not parse 'server.retry.max_retry', using default of 5m. error: %v", err)
					cfg.ServerRetry.MaxRetry = 10
				} else {
					cfg.ServerRetry.MaxRetry = val
				}
			} else {
				cfg.ServerRetry.MaxRetry = 10
			}
			if cfg.ServerRetry.MaxRetry <= 0 {
				log.Warn("'server.retry.multiplier' must be > 0, using default of 10")
				cfg.ServerRetry.MaxRetry = 10
			}
			if valStr := fbit.Conf.String("server.retry.multiplier"); valStr != "" {
				val, err := strconv.ParseFloat(valStr, 64)
				if err != nil {
					log.Warn("could not parse 'server.retry.multiplier', using default of 1.5. error: %v", err)
					cfg.ServerRetry.Multiplier = 1.5
				} else {
					cfg.ServerRetry.Multiplier = val
				}
			} else {
				cfg.ServerRetry.Multiplier = 1.5
			}
			if cfg.ServerRetry.Multiplier <= 1.0 {
				log.Warn("'server.retry.multiplier' must be > 1.0, using default of 1.5")
				cfg.ServerRetry.Multiplier = 1.5
			}

			serviceNamesStr := fbit.Conf.String("server.service_names")
			if serviceNamesStr == "" {
				return nil, errors.New("'server.service_names' (comma-separated) is required for server/all mode with a remote endpoint")
			}
			for _, s := range strings.Split(serviceNamesStr, ",") {
				cfg.ServerServiceNames = append(cfg.ServerServiceNames, strings.TrimSpace(s))
			}
		}
	}
	return cfg, nil
}
