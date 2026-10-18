// This small program is just a small web server created in static mode
// in order to provide the smallest docker image possible

package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	// Def of flags
	portPtr                  = flag.Int("port", 8043, "The listening port")
	contextPath              = flag.String("context", "", "The 'context' path on which files are served, e.g. 'doc' will serve the files at 'http://localhost:<port>/doc/'")
	basePath                 = flag.String("path", "/srv/http", "The path for the static files")
	fallbackPath             = flag.String("fallback", "", "Default fallback file. Either absolute for a specific asset (/index.html), or relative to recursively resolve (index.html)")
	headerFlag               = flag.String("append-header", "", "HTTP response header, specified as `HeaderName:Value` that should be added to all responses.")
	enableGzip               = flag.Bool("enable-gzip", false, "Compress responses with gzip for clients that accept it. Payloads that are already compressed (images, archives, fonts) are served as-is.")
	basicAuth                = flag.Bool("enable-basic-auth", false, "Enable basic auth. By default, password are randomly generated. Use --set-basic-auth to set it.")
	healthCheck              = flag.Bool("enable-health", false, "Enable health check endpoint. You can call /health to get a 200 response. Useful for Kubernetes, OpenFaas, etc.")
	setBasicAuth             = flag.String("set-basic-auth", "", "Define the basic auth. Form must be user:password")
	defaultUsernameBasicAuth = flag.String("default-user-basic-auth", "gopher", "Define the user")
	sizeRandom               = flag.Int("password-length", 16, "Size of the randomized password")
	logLevel                 = flag.String("log-level", "info", "default: info - What level of logging to run, debug logs all requests (error, warn, info, debug)")
	logRequest               = flag.Bool("enable-logging", false, "Enable log request. NOTE: Deprecated, set log-level to debug to log all requests")
	httpsPromote             = flag.Bool("https-promote", false, "All HTTP requests should be redirected to HTTPS")
	headerConfigPath         = flag.String("header-config-path", "/config/headerConfig.json", "Path to the config file for custom response headers")

	username string
	password string
)

func setupLogger(logLevel string) {
	switch logLevel {
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

func parseHeaderFlag(headerFlag string) (string, string) {
	if len(headerFlag) == 0 {
		return "", ""
	}
	pieces := strings.SplitN(headerFlag, ":", 2)
	if len(pieces) == 1 {
		return pieces[0], ""
	}
	return pieces[0], pieces[1]
}

var gzPool = sync.Pool{
	New: func() interface{} {
		w := gzip.NewWriter(io.Discard)
		return w
	},
}

// alreadyCompressed holds extensions whose payloads carry their own
// compression. Running them through gzip costs CPU and typically produces a
// slightly larger response.
var alreadyCompressed = map[string]bool{
	".7z": true, ".br": true, ".bz2": true, ".gz": true, ".rar": true,
	".xz": true, ".zip": true, ".zst": true,
	".avif": true, ".gif": true, ".jpeg": true, ".jpg": true,
	".png": true, ".webp": true,
	".flac": true, ".mp3": true, ".mp4": true, ".ogg": true, ".webm": true,
	".pdf": true, ".woff": true, ".woff2": true,
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

// gzipMiddleware compresses responses for clients that accept gzip, skipping
// payloads that are already compressed.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body depends on Accept-Encoding whether or not this particular
		// response ends up compressed, so shared caches must always key on it.
		w.Header().Set("Vary", "Accept-Encoding")

		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			alreadyCompressed[strings.ToLower(path.Ext(r.URL.Path))] {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")
		gz := gzPool.Get().(*gzip.Writer)
		defer gzPool.Put(gz)

		gz.Reset(w)
		defer gz.Close()

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

func handleReq(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *httpsPromote && r.Header.Get("X-Forwarded-Proto") == "http" {
			http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusMovedPermanently)
			log.Debug().Int("http_code", 301).Str("Method", r.Method).Str("Path", r.URL.Path).Msg("Request Redirected")
			return
		}
		log.Debug().Str("Method", r.Method).Str("Path", r.URL.Path).Msg("Request Handled")
		h.ServeHTTP(w, r)
	})
}

func handleIP(w http.ResponseWriter, r *http.Request) {
	clientIP := requestClientIP(r)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, clientIP)
}

func requestClientIP(r *http.Request) string {
	clientIP := forwardedIP(r.Header.Get("X-Forwarded-For"))
	if clientIP == "" {
		clientIP = forwardedIP(r.Header.Get("X-Real-IP"))
	}
	if clientIP == "" {
		clientIP = forwardedFieldIP(r.Header.Get("Forwarded"))
	}
	if clientIP == "" {
		clientIP = parseIPAddress(r.RemoteAddr)
	}
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}
	return clientIP
}

func handleDNS(w http.ResponseWriter, r *http.Request) {
	handleDNSWithLookup(w, r, net.LookupAddr)
}

func handleDNSWithLookup(w http.ResponseWriter, r *http.Request, lookupAddr func(string) ([]string, error)) {
	clientIP := parseIPAddress(requestClientIP(r))
	if clientIP == "" {
		http.Error(w, "Unable to determine client IP", http.StatusBadRequest)
		return
	}

	names, err := lookupAddr(clientIP)
	if err != nil {
		log.Error().Err(err).Str("ip", clientIP).Msg("PTR lookup failed")
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			http.Error(w, "PTR record not found", http.StatusNotFound)
			return
		}
		http.Error(w, "PTR lookup failed", http.StatusBadGateway)
		return
	}
	if len(names) == 0 {
		http.Error(w, "PTR record not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, names[0])
}

func handleJSON(w http.ResponseWriter, r *http.Request) {
	handleJSONWithHandlers(w, r, handleDNS, handleIP)
}

func handleJSONWithHandlers(w http.ResponseWriter, r *http.Request, dnsHandler, ipHandler http.HandlerFunc) {
	dnsResponse := httptest.NewRecorder()
	dnsHandler(dnsResponse, r)

	ipResponse := httptest.NewRecorder()
	ipHandler(ipResponse, r)

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode([]string{dnsResponse.Body.String(), ipResponse.Body.String()}); err != nil {
		log.Error().Err(err).Msg("Failed to encode JSON response")
	}
}

func forwardedIP(value string) string {
	first := strings.SplitN(value, ",", 2)[0]
	return parseIPAddress(first)
}

func forwardedFieldIP(value string) string {
	first := strings.SplitN(value, ",", 2)[0]
	for _, parameter := range strings.Split(first, ";") {
		name, address, found := strings.Cut(strings.TrimSpace(parameter), "=")
		if found && strings.EqualFold(name, "for") {
			return parseIPAddress(address)
		}
	}
	return ""
}

func parseIPAddress(value string) string {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	if ip := net.ParseIP(value); ip != nil {
		return ip.String()
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
		if ip := net.ParseIP(value); ip != nil {
			return ip.String()
		}
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
}

func main() {

	flag.Parse()

	// Setting up logging output before setting up level to print out deprecation warnings
	// UNIX Time is faster and smaller than most timestamps
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	// Set a pretty console output
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	if *logRequest {
		log.Warn().Msg("enable-logging is deprecated in favor of log-level")
	}

	//if log-level not default and enable-logging set to true, use log-level
	if *logLevel != "info" && *logRequest {
		log.Warn().Msg("log-level not 'info' and enable-logging set, using log-level")
		*logRequest = false
	}

	//if log-level is info and we have enable-logging, set log-level to debug
	if *logLevel == "info" && *logRequest {
		log.Warn().Msg("since enable-logging is set, setting log-level to debug")
		*logLevel = "debug"
	}

	//setting up the logger
	setupLogger(*logLevel)
	log.Debug().Str("Logging Level", zerolog.GlobalLevel().String()).Msg("Logger setup...")

	// sanity check
	if len(*setBasicAuth) != 0 && !*basicAuth {
		log.Debug().Msg("Basic Auth Set")
		*basicAuth = true
	}

	port := ":" + strconv.FormatInt(int64(*portPtr), 10)

	var fileSystem http.FileSystem = http.Dir(*basePath)
	log.Debug().Str("path", *basePath).Msg("File serve path set")

	if *fallbackPath != "" {
		log.Debug().Str("FallbackPath", *fallbackPath).Msg("Fallback path set")
		fileSystem = fallback{
			defaultPath: *fallbackPath,
			fs:          fileSystem,
		}
	}

	handler := handleReq(http.FileServer(fileSystem))

	pathPrefix := "/"
	if len(*contextPath) > 0 {
		pathPrefix = "/" + *contextPath + "/"
		handler = http.StripPrefix(pathPrefix, handler)
	}

	if *basicAuth {
		log.Debug().Msg("Enabling Basic Auth")
		if len(*setBasicAuth) != 0 {
			parseAuth(*setBasicAuth)
		} else {
			generateRandomAuth()
		}
		handler = authMiddleware(handler)
	}

	headerConfigValid := initHeaderConfig(*headerConfigPath)
	if headerConfigValid {
		handler = customHeadersMiddleware(handler)
	}

	// Extra headers.
	if len(*headerFlag) > 0 {
		header, headerValue := parseHeaderFlag(*headerFlag)
		if len(header) > 0 && len(headerValue) > 0 {
			next := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.Debug().Str("URL", r.URL.Path).Str("header", header).Str("headerValue", headerValue).Msg("Extra Headers Handled")
				w.Header().Set(header, headerValue)
				next.ServeHTTP(w, r)
			})

			if !*enableGzip {
				log.Warn().Msg("gzip is no longer implied by append-header; pass --enable-gzip to compress responses")
			}
		} else {
			log.Warn().Msg("appendHeader misconfigured; ignoring.")
		}
	}

	if *enableGzip {
		log.Debug().Msg("Enabling gzip compression")
		handler = gzipMiddleware(handler)
	}

	if *healthCheck {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			log.Debug().Msg("Returning Service Health")
			fmt.Fprintf(w, "Ok")
		})
	}
	http.HandleFunc("/ip", handleIP)
	http.HandleFunc("/dns", handleDNS)
	http.HandleFunc("/json", handleJSON)

	http.Handle(pathPrefix, handler)

	server := &http.Server{
		Addr: port,
		// A client that opens a connection and dribbles out its request
		// forever holds a goroutine hostage, so cap how long the header and
		// the body may take.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is deliberately left unset: it covers the whole
		// response, and a large file over a slow link would be truncated
		// mid-transfer.
	}

	log.Info().Msgf("Listening at http://0.0.0.0%v %v...", port, pathPrefix)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("Server startup failed")
	}

}
