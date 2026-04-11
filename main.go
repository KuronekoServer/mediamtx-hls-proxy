package main

import (
	"container/list"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type config struct {
	ListenHTTPS     string         `json:"listen_https"`
	MetricsPath     string         `json:"metrics_path"`
	CacheMaxBytes   int64          `json:"cache_max_bytes"`
	CacheTTLSeconds int            `json:"cache_ttl_seconds"`
	Domains         []domainConfig `json:"domains"`
}

type domainConfig struct {
	Host      string `json:"host"`
	Upstream  string `json:"upstream"`
	ProxyPath string `json:"proxy_path"`
	CertFile  string `json:"cert_file"`
	KeyFile   string `json:"key_file"`
}

type site struct {
	host      string
	stream    string
	upstream  *url.URL
	proxyPath string
	proxy     *httputil.ReverseProxy
}

type app struct {
	sites       map[string]*site
	metricsPath string
	metrics     *proxyMetrics
	cache       *segmentCache
	viewers     *viewerTracker
	httpClient  *http.Client
}

type proxyMetrics struct {
	inflight       *prometheus.GaugeVec
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	responses      *prometheus.CounterVec
	hlsViewers     *prometheus.GaugeVec
	cacheHits      *prometheus.CounterVec
	cacheMiss      *prometheus.CounterVec
	cacheFill      *prometheus.CounterVec
	cacheSize      prometheus.Gauge
	cacheItems     prometheus.Gauge
	cacheEvictions prometheus.Counter
}

type viewerTracker struct {
	mu           sync.Mutex
	activeWindow time.Duration
	viewers      map[string]map[string]time.Time
}

type segmentCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	size    int64
	limit   int64
	ttl     time.Duration
	metrics *proxyMetrics
}

type cacheEntry struct {
	key        string
	host       string
	statusCode int
	header     http.Header
	body       []byte
	size       int64
	expiresAt  time.Time
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	wroteHead  bool
}

const (
	defaultCacheMaxBytes   int64 = 512 * 1024 * 1024
	defaultCacheTTLSeconds       = 30
	activeViewerWindow           = 30 * time.Second
)

func newStatusCapturingResponseWriter(writer http.ResponseWriter) *statusCapturingResponseWriter {
	return &statusCapturingResponseWriter{
		ResponseWriter: writer,
		statusCode:     http.StatusOK,
	}
}

func (writer *statusCapturingResponseWriter) WriteHeader(statusCode int) {
	if writer.wroteHead {
		writer.ResponseWriter.WriteHeader(statusCode)
		return
	}
	writer.statusCode = statusCode
	writer.wroteHead = true
	writer.ResponseWriter.WriteHeader(statusCode)
}

func (writer *statusCapturingResponseWriter) Write(body []byte) (int, error) {
	if !writer.wroteHead {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(body)
}

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	resolvedConfigPath, err := resolveConfigPath(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := loadConfig(resolvedConfigPath)
	if err != nil {
		log.Fatal(err)
	}

	application, err := newApp(cfg)
	if err != nil {
		log.Fatal(err)
	}

	if err := run(cfg, application); err != nil {
		log.Fatal(err)
	}
}

func resolveConfigPath(configPath string) (string, error) {
	if filepath.IsAbs(configPath) {
		return configPath, nil
	}

	executablePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}

	return filepath.Join(filepath.Dir(executablePath), configPath), nil
}

func loadConfig(configPath string) (*config, error) {
	contents, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg config
	if err := json.Unmarshal(contents, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	applyDefaults(&cfg)
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func applyDefaults(cfg *config) {
	if cfg.ListenHTTPS == "" {
		cfg.ListenHTTPS = ":443"
	}
	if cfg.MetricsPath == "" {
		cfg.MetricsPath = "/metrics/"
	}
	if cfg.CacheMaxBytes == 0 {
		cfg.CacheMaxBytes = defaultCacheMaxBytes
	}
	if cfg.CacheTTLSeconds == 0 {
		cfg.CacheTTLSeconds = defaultCacheTTLSeconds
	}
	cfg.MetricsPath = normalizeRoute(cfg.MetricsPath, false)

	for index := range cfg.Domains {
		domain := &cfg.Domains[index]
		if domain.ProxyPath == "" {
			domain.ProxyPath = "/"
		}
		domain.ProxyPath = normalizeRoute(domain.ProxyPath, true)
	}
}

func validateConfig(cfg *config) error {
	if len(cfg.Domains) == 0 {
		return errors.New("config.domains must not be empty")
	}
	if cfg.MetricsPath == "/" {
		return errors.New("metrics_path must not be /")
	}
	if cfg.CacheMaxBytes <= 0 {
		return errors.New("cache_max_bytes must be greater than 0")
	}
	if cfg.CacheTTLSeconds <= 0 {
		return errors.New("cache_ttl_seconds must be greater than 0")
	}

	seen := make(map[string]struct{}, len(cfg.Domains))
	for _, domain := range cfg.Domains {
		if domain.Host == "" {
			return errors.New("domain.host is required")
		}
		host := strings.ToLower(domain.Host)
		if _, exists := seen[host]; exists {
			return fmt.Errorf("duplicate host %q", domain.Host)
		}
		seen[host] = struct{}{}

		parsed, err := url.Parse(domain.Upstream)
		if err != nil {
			return fmt.Errorf("invalid upstream for %s: %w", domain.Host, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("upstream for %s must include scheme and host", domain.Host)
		}
		if domain.CertFile == "" || domain.KeyFile == "" {
			return fmt.Errorf("cert_file and key_file are required for %s", domain.Host)
		}
	}

	return nil
}

func newApp(cfg *config) (*app, error) {
	application := &app{
		sites:       make(map[string]*site, len(cfg.Domains)),
		metricsPath: cfg.MetricsPath,
		metrics:     newProxyMetrics(),
		viewers:     newViewerTracker(activeViewerWindow),
		httpClient:  newUpstreamHTTPClient(),
	}
	application.cache = newSegmentCache(cfg.CacheMaxBytes, time.Duration(cfg.CacheTTLSeconds)*time.Second, application.metrics)

	for _, domain := range cfg.Domains {
		parsed, err := url.Parse(domain.Upstream)
		if err != nil {
			return nil, fmt.Errorf("parse upstream for %s: %w", domain.Host, err)
		}

		site := &site{
			host:      strings.ToLower(domain.Host),
			stream:    deriveStreamNameFromPath(parsed.Path, strings.ToLower(domain.Host)),
			upstream:  parsed,
			proxyPath: domain.ProxyPath,
		}
		site.proxy = buildReverseProxy(site)

		application.sites[site.host] = site
	}

	return application, nil
}

func run(cfg *config, application *app) error {
	httpsServer, err := newFileTLSServer(cfg, application)
	if err != nil {
		return err
	}

	log.Printf("https proxy server listening on %s", httpsServer.Addr)
	return httpsServer.ListenAndServeTLS("", "")
}

func newFileTLSServer(cfg *config, handler http.Handler) (*http.Server, error) {
	certificates := make(map[string]*tls.Certificate, len(cfg.Domains))
	for _, domain := range cfg.Domains {
		certificate, err := tls.LoadX509KeyPair(domain.CertFile, domain.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load certificate for %s: %w", domain.Host, err)
		}
		certificates[strings.ToLower(domain.Host)] = &certificate
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := strings.ToLower(hello.ServerName)
			certificate, ok := certificates[host]
			if !ok {
				return nil, fmt.Errorf("no certificate configured for server name %q", hello.ServerName)
			}
			return certificate, nil
		},
	}

	return &http.Server{
		Addr:      cfg.ListenHTTPS,
		Handler:   handler,
		TLSConfig: tlsConfig,
	}, nil
}

func buildReverseProxy(site *site) *httputil.ReverseProxy {
	target := *site.upstream
	proxy := httputil.NewSingleHostReverseProxy(&target)
	originalDirector := proxy.Director

	proxy.Director = func(request *http.Request) {
		originalHost := request.Host
		originalDirector(request)
		request.URL.Path = joinURLPath(site.upstream.Path, strings.TrimPrefix(request.URL.Path, site.proxyPath))
		request.Host = site.upstream.Host
		request.Header.Set("X-Forwarded-Host", originalHost)
		request.Header.Set("X-Forwarded-Proto", "https")
	}

	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		log.Printf("proxy error for %s: %v", request.Host, err)
		http.Error(writer, "bad gateway", http.StatusBadGateway)
	}

	return proxy
}

func (application *app) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if isReservedPath(request.URL.Path, application.metricsPath) {
		application.viewers.UpdateMetric(application.metrics.hlsViewers, time.Now())
		promhttp.Handler().ServeHTTP(writer, request)
		return
	}

	if request.URL.Path == "/healthz" {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
		return
	}

	host := normalizeHost(request.Host)
	site, ok := application.sites[host]
	if !ok {
		http.NotFound(writer, request)
		return
	}

	if strings.HasPrefix(request.URL.Path, site.proxyPath) {
		application.serveProxy(writer, request, site)
		return
	}

	http.NotFound(writer, request)
}

func (application *app) serveProxy(writer http.ResponseWriter, request *http.Request, site *site) {
	startedAt := time.Now()
	statusWriter := newStatusCapturingResponseWriter(writer)
	method := request.Method
	proxyPath := site.proxyPath

	if application.isViewerActivityRequest(request) {
		application.trackViewer(site, request, startedAt)
	}

	application.metrics.inflight.WithLabelValues(site.host, method, proxyPath).Inc()
	defer application.metrics.inflight.WithLabelValues(site.host, method, proxyPath).Dec()

	if application.isCacheableSegmentRequest(request) {
		if err := application.serveCachedSegment(statusWriter, request, site); err != nil {
			log.Printf("cached proxy error for %s: %v", request.Host, err)
			if !statusWriter.wroteHead {
				http.Error(statusWriter, "bad gateway", http.StatusBadGateway)
			}
		}
	} else {
		site.proxy.ServeHTTP(statusWriter, request)
	}

	statusCode := strconv.Itoa(statusWriter.statusCode)
	statusText := http.StatusText(statusWriter.statusCode)
	if statusText == "" {
		statusText = "UNKNOWN"
	}

	application.metrics.requests.WithLabelValues(site.host, method, proxyPath).Inc()
	application.metrics.responses.WithLabelValues(site.host, method, statusText, statusCode).Inc()
	application.metrics.duration.WithLabelValues(site.host, method, proxyPath).Observe(time.Since(startedAt).Seconds())
}

func (application *app) isCacheableSegmentRequest(request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	if request.Header.Get("Range") != "" {
		return false
	}

	extension := strings.ToLower(path.Ext(request.URL.Path))
	return extension == ".ts" || extension == ".mpegts"
}

func (application *app) isViewerActivityRequest(request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}

	switch strings.ToLower(path.Ext(request.URL.Path)) {
	case ".m3u8", ".ts", ".mpegts", ".mp4", ".m4s":
		return true
	default:
		return false
	}
}

func (application *app) trackViewer(site *site, request *http.Request, now time.Time) {
	stream := resolveStreamName(site, request)
	viewer := clientIdentity(request)
	if stream == "" || viewer == "" {
		return
	}

	application.viewers.Touch(stream, viewer, now)
}

func (application *app) serveCachedSegment(writer http.ResponseWriter, request *http.Request, site *site) error {
	cacheKey := buildCacheKey(site.host, request.URL)
	if entry, ok := application.cache.Get(cacheKey); ok {
		application.metrics.cacheHits.WithLabelValues(site.host).Inc()
		writeCachedEntry(writer, entry)
		return nil
	}

	application.metrics.cacheMiss.WithLabelValues(site.host).Inc()

	upstreamRequest, err := newUpstreamRequest(request, site)
	if err != nil {
		return err
	}

	response, err := application.httpClient.Do(upstreamRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	copyHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)

	body, err := streamResponseWithOptionalCache(writer, response.Body, application.cache.limit)
	if err != nil {
		return err
	}

	if response.StatusCode == http.StatusOK && len(body) > 0 {
		entry := &cacheEntry{
			key:        cacheKey,
			host:       site.host,
			statusCode: response.StatusCode,
			header:     cloneHeader(response.Header),
			body:       body,
			size:       int64(len(body)),
			expiresAt:  time.Now().Add(application.cache.ttl),
		}
		if application.cache.Set(entry) {
			application.metrics.cacheFill.WithLabelValues(site.host).Inc()
		}
	}

	return nil
}

func normalizeHost(hostport string) string {
	if hostport == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func normalizeRoute(route string, trailingSlash bool) string {
	if route == "" {
		if trailingSlash {
			return "/"
		}
		return "/"
	}
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	if trailingSlash && !strings.HasSuffix(route, "/") {
		route += "/"
	}
	if !trailingSlash && len(route) > 1 {
		route = strings.TrimSuffix(route, "/")
	}
	return route
}

func isReservedPath(requestPath string, reservedPath string) bool {
	if requestPath == reservedPath {
		return true
	}
	if reservedPath == "/" {
		return true
	}
	return strings.HasPrefix(requestPath, reservedPath+"/")
}

func buildCacheKey(host string, requestURL *url.URL) string {
	return host + "|" + requestURL.Path + "?" + requestURL.RawQuery
}

func resolveStreamName(site *site, request *http.Request) string {
	effectivePath := joinURLPath(site.upstream.Path, strings.TrimPrefix(request.URL.Path, site.proxyPath))
	return deriveStreamNameFromPath(effectivePath, site.stream)
}

func deriveStreamNameFromPath(requestPath string, fallback string) string {
	cleanedPath := path.Clean("/" + strings.TrimSpace(requestPath))
	if cleanedPath == "/" {
		return fallback
	}

	if extension := path.Ext(cleanedPath); extension != "" {
		parent := path.Dir(cleanedPath)
		if parent == "/" || parent == "." {
			return fallback
		}

		stream := path.Base(parent)
		if stream != "." && stream != "/" {
			return stream
		}
		return fallback
	}

	stream := path.Base(cleanedPath)
	if stream == "." || stream == "/" {
		return fallback
	}
	return stream
}

func clientIdentity(request *http.Request) string {
	forwardedFor := request.Header.Get("X-Forwarded-For")
	if forwardedFor != "" {
		parts := strings.Split(forwardedFor, ",")
		if len(parts) > 0 {
			candidate := strings.TrimSpace(parts[0])
			if candidate != "" {
				return candidate
			}
		}
	}

	if realIP := strings.TrimSpace(request.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}

	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		return host
	}

	return strings.TrimSpace(request.RemoteAddr)
}

func joinURLPath(base string, suffix string) string {
	if suffix == "" {
		if base == "" {
			return "/"
		}
		if strings.HasSuffix(base, "/") {
			return base
		}
		return base + "/"
	}

	cleanBase := strings.TrimSuffix(base, "/")
	cleanSuffix := strings.TrimPrefix(suffix, "/")
	if cleanBase == "" {
		return "/" + cleanSuffix
	}
	return cleanBase + "/" + cleanSuffix
}

func newUpstreamHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}

	return &http.Client{Transport: transport}
}

func newUpstreamRequest(request *http.Request, site *site) (*http.Request, error) {
	upstreamURL := *site.upstream
	upstreamURL.Path = joinURLPath(site.upstream.Path, strings.TrimPrefix(request.URL.Path, site.proxyPath))
	upstreamURL.RawQuery = request.URL.RawQuery

	upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, upstreamURL.String(), nil)
	if err != nil {
		return nil, err
	}

	upstreamRequest.Header = cloneHeader(request.Header)
	upstreamRequest.Host = site.upstream.Host
	upstreamRequest.Header.Set("X-Forwarded-Host", request.Host)
	upstreamRequest.Header.Set("X-Forwarded-Proto", "https")

	return upstreamRequest, nil
}

func streamResponseWithOptionalCache(writer http.ResponseWriter, body io.Reader, limit int64) ([]byte, error) {
	buffer := make([]byte, 32*1024)
	var cachedBody []byte
	cacheEnabled := limit > 0
	cacheSize := int64(0)

	for {
		readBytes, readErr := body.Read(buffer)
		if readBytes > 0 {
			chunk := buffer[:readBytes]
			if _, err := writer.Write(chunk); err != nil {
				return nil, err
			}

			if cacheEnabled {
				if cacheSize+int64(readBytes) <= limit {
					cachedBody = append(cachedBody, chunk...)
					cacheSize += int64(readBytes)
				} else {
					cacheEnabled = false
					cachedBody = nil
				}
			}
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}

	if !cacheEnabled {
		return nil, nil
	}

	return cachedBody, nil
}

func writeCachedEntry(writer http.ResponseWriter, entry *cacheEntry) {
	copyHeaders(writer.Header(), entry.header)
	writer.WriteHeader(entry.statusCode)
	_, _ = writer.Write(entry.body)
}

func copyHeaders(destination http.Header, source http.Header) {
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}

func cloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	copyHeaders(cloned, header)
	return cloned
}

func newSegmentCache(limit int64, ttl time.Duration, metrics *proxyMetrics) *segmentCache {
	cache := &segmentCache{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		limit:   limit,
		ttl:     ttl,
		metrics: metrics,
	}
	cache.updateMetrics()
	return cache
}

func newViewerTracker(activeWindow time.Duration) *viewerTracker {
	return &viewerTracker{
		activeWindow: activeWindow,
		viewers:      make(map[string]map[string]time.Time),
	}
}

func (tracker *viewerTracker) Touch(stream string, viewer string, now time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	tracker.pruneLocked(now)

	streamViewers, ok := tracker.viewers[stream]
	if !ok {
		streamViewers = make(map[string]time.Time)
		tracker.viewers[stream] = streamViewers
	}

	streamViewers[viewer] = now
}

func (tracker *viewerTracker) UpdateMetric(metric *prometheus.GaugeVec, now time.Time) {
	snapshot := tracker.snapshot(now)
	metric.Reset()
	for stream, count := range snapshot {
		metric.WithLabelValues(stream).Set(float64(count))
	}
}

func (tracker *viewerTracker) snapshot(now time.Time) map[string]int {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	tracker.pruneLocked(now)

	snapshot := make(map[string]int, len(tracker.viewers))
	for stream, viewers := range tracker.viewers {
		snapshot[stream] = len(viewers)
	}

	return snapshot
}

func (tracker *viewerTracker) pruneLocked(now time.Time) {
	threshold := now.Add(-tracker.activeWindow)
	for stream, viewers := range tracker.viewers {
		for viewer, lastSeenAt := range viewers {
			if lastSeenAt.Before(threshold) {
				delete(viewers, viewer)
			}
		}

		if len(viewers) == 0 {
			delete(tracker.viewers, stream)
		}
	}
}

func (cache *segmentCache) Get(key string) (*cacheEntry, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	element, ok := cache.entries[key]
	if !ok {
		return nil, false
	}

	entry := element.Value.(*cacheEntry)
	if time.Now().After(entry.expiresAt) {
		cache.removeElement(element)
		cache.updateMetrics()
		return nil, false
	}

	cache.order.MoveToFront(element)
	return entry, true
}

func (cache *segmentCache) Set(entry *cacheEntry) bool {
	if entry.size <= 0 || entry.size > cache.limit {
		return false
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if existing, ok := cache.entries[entry.key]; ok {
		cache.removeElement(existing)
	}

	element := cache.order.PushFront(entry)
	cache.entries[entry.key] = element
	cache.size += entry.size

	for cache.size > cache.limit {
		oldest := cache.order.Back()
		if oldest == nil {
			break
		}
		cache.removeElement(oldest)
		cache.metrics.cacheEvictions.Inc()
	}

	cache.updateMetrics()
	return true
}

func (cache *segmentCache) removeElement(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(cache.entries, entry.key)
	cache.order.Remove(element)
	cache.size -= entry.size
	if cache.size < 0 {
		cache.size = 0
	}
}

func (cache *segmentCache) updateMetrics() {
	cache.metrics.cacheSize.Set(float64(cache.size))
	cache.metrics.cacheItems.Set(float64(len(cache.entries)))
}

func newProxyMetrics() *proxyMetrics {
	return &proxyMetrics{
		inflight: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mediamtx_hls_proxy_inflight_requests",
			Help: "Current number of in-flight proxy requests.",
		}, []string{"host", "method", "proxy_path"}),
		requests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "mediamtx_hls_proxy_requests_total",
			Help: "Total number of proxy requests.",
		}, []string{"host", "method", "proxy_path"}),
		duration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mediamtx_hls_proxy_request_duration_seconds",
			Help:    "Duration of proxy requests in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"host", "method", "proxy_path"}),
		responses: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "mediamtx_hls_proxy_response_status_total",
			Help: "Total number of proxy responses by status.",
		}, []string{"host", "method", "status", "status_code"}),
		hlsViewers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "hls_viewers",
			Help: "Estimated active HLS viewers in the last 30 seconds by stream.",
		}, []string{"stream"}),
		cacheHits: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "mediamtx_hls_proxy_cache_hits_total",
			Help: "Total number of cache hits for MPEG-TS segments.",
		}, []string{"host"}),
		cacheMiss: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "mediamtx_hls_proxy_cache_misses_total",
			Help: "Total number of cache misses for MPEG-TS segments.",
		}, []string{"host"}),
		cacheFill: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "mediamtx_hls_proxy_cache_store_total",
			Help: "Total number of MPEG-TS segments stored in cache.",
		}, []string{"host"}),
		cacheSize: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "mediamtx_hls_proxy_cache_bytes",
			Help: "Current memory usage of the MPEG-TS cache in bytes.",
		}),
		cacheItems: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "mediamtx_hls_proxy_cache_items",
			Help: "Current number of cached MPEG-TS segments.",
		}),
		cacheEvictions: promauto.NewCounter(prometheus.CounterOpts{
			Name: "mediamtx_hls_proxy_cache_evictions_total",
			Help: "Total number of cache evictions caused by the memory limit.",
		}),
	}
}
