// Copyright (c) 2018 Palantir Technologies. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"io/ioutil"
	"net/url"
	"sort"
	"time"

	"github.com/palantir/conjure-go-runtime/v2/conjure-go-client/httpclient/internal/refreshingclient"
	"github.com/palantir/pkg/metrics"
	"github.com/palantir/pkg/tlsconfig"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
)

// ServicesConfig is the top-level configuration struct for all HTTP clients. It supports
// setting default values and overriding those values per-service. Use ClientConfig(serviceName)
// to retrieve a specific service's configuration, and the httpclient.WithConfig() param to
// construct a Client using that configuration. The fields of this struct should generally not
// be read directly by application code.
type ServicesConfig struct {
	// Default values will be used for any field which is not set for a specific client.
	Default ClientConfig `json:",inline" yaml:",inline"`
	// Services is a map of serviceName (e.g. "my-api") to service-specific configuration.
	Services map[string]ClientConfig `json:"services,omitempty" yaml:"services,omitempty"`
}

// ClientConfig represents the configuration for a single REST client.
type ClientConfig struct {
	ServiceName string `json:"-" yaml:"-"`
	// URIs is a list of fully specified base URIs for the service. These can optionally include a path
	// which will be prepended to the request path specified when invoking the client.
	URIs []string `json:"uris,omitempty" yaml:"uris,omitempty"`
	// APIToken is a string which, if provided, will be used as a Bearer token in the Authorization header.
	// This takes precedence over APITokenFile.
	APIToken *string `json:"api-token,omitempty" yaml:"api-token,omitempty"`
	// APITokenFile is an on-disk location containing a Bearer token. If APITokenFile is provided and APIToken
	// is not, the content of the file will be used as the APIToken.
	APITokenFile *string `json:"api-token-file,omitempty" yaml:"api-token-file,omitempty"`
	// DisableHTTP2, if true, will prevent the client from modifying the *tls.Config object to support H2 connections.
	DisableHTTP2 *bool `json:"disable-http2,omitempty" yaml:"disable-http2,omitempty"`
	// ProxyFromEnvironment enables reading HTTP proxy information from environment variables.
	// See 'http.ProxyFromEnvironment' documentation for specific behavior.
	ProxyFromEnvironment *bool `json:"proxy-from-environment,omitempty" yaml:"proxy-from-environment,omitempty"`
	// ProxyURL uses the provided URL for proxying the request. Schemes http, https, and socks5 are supported.
	ProxyURL *string `json:"proxy-url,omitempty" yaml:"proxy-url,omitempty"`

	// MaxNumRetries controls the number of times the client will retry retryable failures.
	// If unset, this defaults to twice the number of URIs provided.
	MaxNumRetries *int `json:"max-num-retries,omitempty" yaml:"max-num-retries,omitempty"`
	// InitialBackoff controls the duration of the first backoff interval. This delay will double for each subsequent backoff, capped at the MaxBackoff value.
	InitialBackoff *time.Duration `json:"initial-backoff,omitempty" yaml:"initial-backoff,omitempty"`
	// MaxBackoff controls the maximum duration the client will sleep before retrying a request.
	MaxBackoff *time.Duration `json:"max-backoff,omitempty" yaml:"max-backoff,omitempty"`

	// ConnectTimeout is the maximum time for the net.Dialer to connect to the remote host.
	ConnectTimeout *time.Duration `json:"connect-timeout,omitempty" yaml:"connect-timeout,omitempty"`
	// ReadTimeout is the maximum timeout for non-mutating requests.
	// NOTE: The current implementation uses the max(ReadTimeout, WriteTimeout) to set the http.Client timeout value.
	ReadTimeout *time.Duration `json:"read-timeout,omitempty" yaml:"read-timeout,omitempty"`
	// WriteTimeout is the maximum timeout for mutating requests.
	// NOTE: The current implementation uses the max(ReadTimeout, WriteTimeout) to set the http.Client timeout value.
	WriteTimeout *time.Duration `json:"write-timeout,omitempty" yaml:"write-timeout,omitempty"`
	// IdleConnTimeout sets the timeout for idle connections.
	IdleConnTimeout *time.Duration `json:"idle-conn-timeout,omitempty" yaml:"idle-conn-timeout,omitempty"`
	// TLSHandshakeTimeout sets the timeout for TLS handshakes
	TLSHandshakeTimeout *time.Duration `json:"tls-handshake-timeout,omitempty" yaml:"tls-handshake-timeout,omitempty"`
	// IdleConnTimeout sets the timeout to receive the server's first response headers after
	// fully writing the request headers if the request has an "Expect: 100-continue" header.
	ExpectContinueTimeout *time.Duration `json:"expect-continue-timeout,omitempty" yaml:"expect-continue-timeout,omitempty"`

	// HTTP2ReadIdleTimeout sets the maximum time to wait before sending periodic health checks (pings) for an HTTP/2 connection.
	// If unset, the client defaults to 30s for HTTP/2 clients.
	HTTP2ReadIdleTimeout *time.Duration `json:"http2-read-idle-timeout,omitempty" yaml:"http2-read-idle-timeout,omitempty"`
	// HTTP2PingTimeout is the maximum time to wait for a ping response in an HTTP/2 connection,
	// when health checking is enabled which is done by setting the HTTP2ReadIdleTimeout > 0.
	// If unset, the client defaults to 15s if the HTTP2ReadIdleTimeout is > 0.
	HTTP2PingTimeout *time.Duration `json:"http2-ping-timeout,omitempty" yaml:"http2-ping-timeout,omitempty"`

	// MaxIdleConns sets the number of reusable TCP connections the client will maintain.
	// If unset, the client defaults to 32.
	MaxIdleConns *int `json:"max-idle-conns,omitempty" yaml:"max-idle-conns,omitempty"`
	// MaxIdleConnsPerHost sets the number of reusable TCP connections the client will maintain per destination.
	// If unset, the client defaults to 32.
	MaxIdleConnsPerHost *int `json:"max-idle-conns-per-host,omitempty" yaml:"max-idle-conns-per-host,omitempty"`

	// Metrics allows disabling metric emission or adding additional static tags to the client metrics.
	Metrics MetricsConfig `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	// Security configures the TLS configuration for the client. It accepts file paths which should be
	// absolute paths or relative to the process's current working directory.
	Security SecurityConfig `json:"security,omitempty" yaml:"security,omitempty"`
}

type MetricsConfig struct {
	// Enabled can be used to disable metrics with an explicit 'false'. Metrics are enabled if this is unset.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Tags allows setting arbitrary additional tags on the metrics emitted by the client.
	Tags map[string]string `json:"tags,omitempty" yaml:"tags,omitempty"`
}

type SecurityConfig struct {
	CAFiles  []string `json:"ca-files,omitempty" yaml:"ca-files,omitempty"`
	CertFile string   `json:"cert-file,omitempty" yaml:"cert-file,omitempty"`
	KeyFile  string   `json:"key-file,omitempty" yaml:"key-file,omitempty"`
}

// MustClientConfig returns an error if the service name is not configured.
func (c ServicesConfig) MustClientConfig(serviceName string) (ClientConfig, error) {
	if _, ok := c.Services[serviceName]; !ok {
		return ClientConfig{}, werror.Error("ClientConfiguration not found for serviceName", werror.SafeParam("serviceName", serviceName))
	}
	return c.ClientConfig(serviceName), nil
}

// ClientConfig returns the default configuration merged with service-specific configuration.
// If the serviceName is not in the service map, an empty configuration (plus defaults) is used.
func (c ServicesConfig) ClientConfig(serviceName string) ClientConfig {
	conf, ok := c.Services[serviceName]
	if !ok {
		conf = ClientConfig{}
	}
	conf.ServiceName = serviceName

	return MergeClientConfig(conf, c.Default)
}

// MergeClientConfig merges two instances of ClientConfig, preferring values from conf over defaults.
// The ServiceName field is not affected, and is expected to be set in the config before building a Client.
func MergeClientConfig(conf, defaults ClientConfig) ClientConfig {
	if len(conf.URIs) == 0 {
		conf.URIs = defaults.URIs
	}
	if conf.APIToken == nil {
		conf.APIToken = defaults.APIToken
	}
	if conf.APITokenFile == nil {
		conf.APITokenFile = defaults.APITokenFile
	}
	if conf.MaxNumRetries == nil {
		conf.MaxNumRetries = defaults.MaxNumRetries
	}
	if conf.ConnectTimeout == nil {
		conf.ConnectTimeout = defaults.ConnectTimeout
	}
	if conf.ReadTimeout == nil {
		conf.ReadTimeout = defaults.ReadTimeout
	}
	if conf.WriteTimeout == nil {
		conf.WriteTimeout = defaults.WriteTimeout
	}
	if conf.IdleConnTimeout == nil {
		conf.IdleConnTimeout = defaults.IdleConnTimeout
	}
	if conf.TLSHandshakeTimeout == nil {
		conf.TLSHandshakeTimeout = defaults.TLSHandshakeTimeout
	}
	if conf.ExpectContinueTimeout == nil {
		conf.ExpectContinueTimeout = defaults.ExpectContinueTimeout
	}
	if conf.HTTP2ReadIdleTimeout == nil {
		conf.HTTP2ReadIdleTimeout = defaults.HTTP2ReadIdleTimeout
	}
	if conf.HTTP2PingTimeout == nil {
		conf.HTTP2PingTimeout = defaults.HTTP2PingTimeout
	}
	if conf.MaxIdleConns == nil {
		conf.MaxIdleConns = defaults.MaxIdleConns
	}
	if conf.MaxIdleConnsPerHost == nil {
		conf.MaxIdleConnsPerHost = defaults.MaxIdleConnsPerHost
	}
	if conf.Metrics.Enabled == nil {
		conf.Metrics.Enabled = defaults.Metrics.Enabled
	}
	if conf.InitialBackoff == nil {
		conf.InitialBackoff = defaults.InitialBackoff
	}
	if conf.MaxBackoff == nil {
		conf.MaxBackoff = defaults.MaxBackoff
	}
	if conf.DisableHTTP2 == nil {
		conf.DisableHTTP2 = defaults.DisableHTTP2
	}
	if conf.ProxyFromEnvironment == nil {
		conf.ProxyFromEnvironment = defaults.ProxyFromEnvironment
	}
	if conf.ProxyURL == nil {
		conf.ProxyURL = defaults.ProxyURL
	}

	if len(defaults.Metrics.Tags) != 0 {
		if conf.Metrics.Tags == nil {
			conf.Metrics.Tags = make(map[string]string, len(defaults.Metrics.Tags))
		}
		for k, v := range defaults.Metrics.Tags {
			if _, ok := conf.Metrics.Tags[k]; !ok {
				conf.Metrics.Tags[k] = v
			}
		}
	}
	if conf.Security.CAFiles == nil {
		conf.Security.CAFiles = defaults.Security.CAFiles
	}
	if conf.Security.CertFile == "" {
		conf.Security.CertFile = defaults.Security.CertFile
	}
	if conf.Security.KeyFile == "" {
		conf.Security.KeyFile = defaults.Security.KeyFile
	}
	return conf
}

func configToParams(c ClientConfig) ([]ClientParam, error) {
	var params []ClientParam

	if len(c.URIs) > 0 {
		params = append(params, WithBaseURLs(c.URIs))
	}

	if c.ServiceName != "" {
		params = append(params, WithServiceName(c.ServiceName))
	}

	// Bearer Token

	if c.APIToken != nil && *c.APIToken != "" {
		params = append(params, WithAuthToken(*c.APIToken))
	} else if c.APITokenFile != nil && *c.APITokenFile != "" {
		token, err := ioutil.ReadFile(*c.APITokenFile)
		if err != nil {
			return nil, werror.Wrap(err, "failed to read api-token-file", werror.SafeParam("file", *c.APITokenFile))
		}
		params = append(params, WithAuthToken(string(bytes.TrimSpace(token))))
	}

	// Disable HTTP2 (http2 is enabled by default)
	if c.DisableHTTP2 != nil && *c.DisableHTTP2 {
		params = append(params, WithDisableHTTP2())
	}

	// Retries

	if c.MaxNumRetries != nil {
		params = append(params, WithMaxRetries(*c.MaxNumRetries))
	}

	// Backoff

	if c.MaxBackoff != nil {
		params = append(params, WithMaxBackoff(*c.MaxBackoff))
	}

	if c.InitialBackoff != nil {
		params = append(params, WithInitialBackoff(*c.InitialBackoff))
	}

	// Metrics (default enabled)

	if c.Metrics.Enabled == nil || (c.Metrics.Enabled != nil && *c.Metrics.Enabled) {
		configuredTags, err := metrics.NewTags(c.Metrics.Tags)
		if err != nil {
			return nil, werror.Wrap(err, "invalid metrics configuration")
		}
		params = append(params, WithMetrics(StaticTagsProvider(configuredTags)))
	}

	// Proxy

	if c.ProxyFromEnvironment != nil && *c.ProxyFromEnvironment {
		params = append(params, WithProxyFromEnvironment())
	}
	if c.ProxyURL != nil {
		params = append(params, WithProxyURL(*c.ProxyURL))
	}

	// Timeouts

	if c.ConnectTimeout != nil && *c.ConnectTimeout != 0 {
		params = append(params, WithDialTimeout(*c.ConnectTimeout))
	}
	if c.IdleConnTimeout != nil && *c.IdleConnTimeout != 0 {
		params = append(params, WithIdleConnTimeout(*c.IdleConnTimeout))
	}
	if c.TLSHandshakeTimeout != nil && *c.TLSHandshakeTimeout != 0 {
		params = append(params, WithTLSHandshakeTimeout(*c.TLSHandshakeTimeout))
	}
	if c.ExpectContinueTimeout != nil && *c.ExpectContinueTimeout != 0 {
		params = append(params, WithExpectContinueTimeout(*c.ExpectContinueTimeout))
	}
	if c.HTTP2ReadIdleTimeout != nil && *c.HTTP2ReadIdleTimeout >= 0 {
		params = append(params, WithHTTP2ReadIdleTimeout(*c.HTTP2ReadIdleTimeout))
	}
	if c.HTTP2PingTimeout != nil && *c.HTTP2PingTimeout >= 0 {
		params = append(params, WithHTTP2PingTimeout(*c.HTTP2PingTimeout))
	}

	// Connections

	if c.MaxIdleConns != nil && *c.MaxIdleConns != 0 {
		params = append(params, WithMaxIdleConns(*c.MaxIdleConns))
	}
	if c.MaxIdleConnsPerHost != nil && *c.MaxIdleConnsPerHost != 0 {
		params = append(params, WithMaxIdleConnsPerHost(*c.MaxIdleConnsPerHost))
	}

	// N.B. we only have one timeout field (not based on method) so just take the max of read and write for now.
	var timeout time.Duration
	if orZero(c.WriteTimeout) > orZero(c.ReadTimeout) {
		timeout = *c.WriteTimeout
	} else if c.ReadTimeout != nil {
		timeout = *c.ReadTimeout
	}
	if timeout != 0 {
		params = append(params, WithHTTPTimeout(timeout))
	}

	// Security (TLS) Config

	if tlsConfig, err := newTLSConfig(c.Security); err != nil {
		return nil, err
	} else if tlsConfig != nil {
		params = append(params, WithTLSConfig(tlsConfig))
	}

	return params, nil
}

func RefreshableClientConfigFromServiceConfig(servicesConfig RefreshableServicesConfig, serviceName string) RefreshableClientConfig {
	return NewRefreshingClientConfig(servicesConfig.MapServicesConfig(func(servicesConfig ServicesConfig) interface{} {
		return servicesConfig.ClientConfig(serviceName)
	}))
}

func newValidatedClientParamsFromConfig(ctx context.Context, config ClientConfig, isHTTPClient bool) (refreshingclient.ValidatedClientParams, error) {
	dialer := refreshingclient.DialerParams{
		DialTimeout: derefDurationPtr(config.ConnectTimeout, defaultDialTimeout),
		KeepAlive:   defaultKeepAlive,
	}

	transport := refreshingclient.TransportParams{
		MaxIdleConns:          derefIntPtr(config.MaxIdleConns, defaultMaxIdleConns),
		MaxIdleConnsPerHost:   derefIntPtr(config.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost),
		DisableHTTP2:          derefBoolPtr(config.DisableHTTP2, false),
		IdleConnTimeout:       derefDurationPtr(config.IdleConnTimeout, defaultIdleConnTimeout),
		ExpectContinueTimeout: derefDurationPtr(config.ExpectContinueTimeout, defaultExpectContinueTimeout),
		HTTP2PingTimeout:      derefDurationPtr(config.HTTP2PingTimeout, defaultHTTP2PingTimeout),
		HTTP2ReadIdleTimeout:  derefDurationPtr(config.HTTP2ReadIdleTimeout, defaultHTTP2ReadIdleTimeout),
		ProxyFromEnvironment:  derefBoolPtr(config.ProxyFromEnvironment, true),
		TLSHandshakeTimeout:   derefDurationPtr(config.TLSHandshakeTimeout, defaultTLSHandshakeTimeout),
	}

	if config.ProxyURL != nil {
		proxyURL, err := url.ParseRequestURI(*config.ProxyURL)
		if err != nil {
			return refreshingclient.ValidatedClientParams{}, werror.WrapWithContextParams(ctx, err, "invalid proxy url")
		}
		switch proxyURL.Scheme {
		case "http", "https":
			transport.HTTPProxyURL = proxyURL
		case "socks5", "socks5h":
			dialer.SocksProxyURL = proxyURL
		default:
			return refreshingclient.ValidatedClientParams{}, werror.WrapWithContextParams(ctx, err, "invalid proxy url: only http(s) and socks5 are supported")
		}
	}

	var apiToken *string
	if config.APIToken != nil {
		apiToken = config.APIToken
	} else if config.APITokenFile != nil {
		file := *config.APITokenFile
		token, err := ioutil.ReadFile(file)
		if err != nil {
			return refreshingclient.ValidatedClientParams{}, werror.WrapWithContextParams(ctx, err, "failed to read api-token-file", werror.SafeParam("file", file))
		}
		tokenStr := string(token)
		apiToken = &tokenStr
	}

	disableMetrics := config.Metrics.Enabled != nil && !*config.Metrics.Enabled

	metricsTags, err := metrics.NewTags(config.Metrics.Tags)
	if err != nil {
		return refreshingclient.ValidatedClientParams{}, err
	}

	retryParams := refreshingclient.RetryParams{
		InitialBackoff: derefDurationPtr(config.InitialBackoff, defaultInitialBackoff),
		MaxBackoff:     derefDurationPtr(config.MaxBackoff, defaultMaxBackoff),
	}
	var maxAttempts *int
	if config.MaxNumRetries != nil {
		attempts := *config.MaxNumRetries + 1
		maxAttempts = &attempts
	}

	timeout := defaultHTTPTimeout
	if config.ReadTimeout != nil || config.WriteTimeout != nil {
		rt := derefDurationPtr(config.ReadTimeout, 0)
		wt := derefDurationPtr(config.WriteTimeout, 0)
		// return max of read and write
		if rt > wt {
			timeout = rt
		} else {
			timeout = wt
		}
	}

	uris := make([]string, 0, len(config.URIs))
	for _, uriStr := range config.URIs {
		if uriStr == "" {
			continue
		}
		if _, err := url.ParseRequestURI(uriStr); err != nil {
			return refreshingclient.ValidatedClientParams{}, werror.WrapWithContextParams(ctx, err, "invalid url")
		}
		uris = append(uris, uriStr)
	}
	// Plain HTTP clients do not store URIs
	if !isHTTPClient && len(uris) == 0 {
		return refreshingclient.ValidatedClientParams{}, werror.ErrorWithContextParams(ctx, "httpclient URLs must not be empty")
	}
	sort.Strings(uris)

	return refreshingclient.ValidatedClientParams{
		APIToken:       apiToken,
		Dialer:         dialer,
		DisableMetrics: disableMetrics,
		MaxAttempts:    maxAttempts,
		MetricsTags:    metricsTags,
		Retry:          retryParams,
		Timeout:        timeout,
		Transport:      transport,
		URIs:           uris,
	}, nil
}

func subscribeTLSConfigUpdateWarning(ctx context.Context, security RefreshableSecurityConfig) (*tls.Config, error) {
	//TODO: Implement refreshable TLS configuration.
	// It is hard to represent all of the configuration (e.g. a dynamic function for GetCertificate) in primitive values friendly to reflect.DeepEqual.
	currentSecurity := security.CurrentSecurityConfig()

	security.CAFiles().SubscribeToStringSlice(func(caFiles []string) {
		svc1log.FromContext(ctx).Warn("conjure-go-runtime: CAFiles configuration changed but can not be live-reloaded.",
			svc1log.SafeParam("existingCAFiles", currentSecurity.CAFiles),
			svc1log.SafeParam("ignoredCAFiles", caFiles))
	})
	security.CertFile().SubscribeToString(func(certFile string) {
		svc1log.FromContext(ctx).Warn("conjure-go-runtime: CertFile configuration changed but can not be live-reloaded.",
			svc1log.SafeParam("existingCertFile", currentSecurity.CertFile),
			svc1log.SafeParam("ignoredCertFile", certFile))
	})
	security.KeyFile().SubscribeToString(func(keyFile string) {
		svc1log.FromContext(ctx).Warn("conjure-go-runtime: KeyFile configuration changed but can not be live-reloaded.",
			svc1log.SafeParam("existingKeyFile", currentSecurity.KeyFile),
			svc1log.SafeParam("ignoredKeyFile", keyFile))
	})

	return newTLSConfig(currentSecurity)
}

func newTLSConfig(security SecurityConfig) (*tls.Config, error) {
	var tlsParams []tlsconfig.ClientParam
	if len(security.CAFiles) != 0 {
		tlsParams = append(tlsParams, tlsconfig.ClientRootCAFiles(security.CAFiles...))
	}
	if security.CertFile != "" && security.KeyFile != "" {
		tlsParams = append(tlsParams, tlsconfig.ClientKeyPairFiles(security.CertFile, security.KeyFile))
	}
	if len(tlsParams) != 0 {
		tlsConfig, err := tlsconfig.NewClientConfig(tlsParams...)
		if err != nil {
			return nil, werror.Wrap(err, "failed to build tlsConfig")
		}
		return tlsConfig, nil
	}
	return nil, nil
}

func derefDurationPtr(durPtr *time.Duration, defaultVal time.Duration) time.Duration {
	if durPtr == nil {
		return defaultVal
	}
	return *durPtr
}

func derefIntPtr(intPtr *int, defaultVal int) int {
	if intPtr == nil {
		return defaultVal
	}
	return *intPtr
}

func derefBoolPtr(boolPtr *bool, defaultVal bool) bool {
	if boolPtr == nil {
		return defaultVal
	}
	return *boolPtr
}

func orZero(d *time.Duration) time.Duration {
	return derefDurationPtr(d, 0)
}
