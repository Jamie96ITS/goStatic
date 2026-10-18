package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestParseHeaderFlag(t *testing.T) {
	tests := []struct {
		name      string
		flag      string
		wantKey   string
		wantValue string
	}{
		{name: "empty flag", flag: "", wantKey: "", wantValue: ""},
		{name: "key and value", flag: "X-Frame-Options:DENY", wantKey: "X-Frame-Options", wantValue: "DENY"},
		{name: "key without value", flag: "X-Frame-Options", wantKey: "X-Frame-Options", wantValue: ""},
		{name: "value containing a colon", flag: "Link:<https://example.com>", wantKey: "Link", wantValue: "<https://example.com>"},
		{name: "empty value", flag: "X-Empty:", wantKey: "X-Empty", wantValue: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value := parseHeaderFlag(tt.flag)
			if key != tt.wantKey {
				t.Errorf("key = %q, want %q", key, tt.wantKey)
			}
			if value != tt.wantValue {
				t.Errorf("value = %q, want %q", value, tt.wantValue)
			}
		})
	}
}

// echoHandler writes a compressible body so compression is observable.
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := strings.Repeat("compress me please\n", 500)
		w.Header().Set("Content-Length", "0") // must not survive compression
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	})
}

func TestGzipMiddleware(t *testing.T) {
	tests := []struct {
		name           string
		target         string
		acceptEncoding string
		wantCompressed bool
	}{
		{name: "compressible file, client accepts gzip", target: "/site.css", acceptEncoding: "gzip", wantCompressed: true},
		{name: "gzip among several encodings", target: "/site.css", acceptEncoding: "deflate, gzip, br", wantCompressed: true},
		{name: "client does not accept gzip", target: "/site.css", acceptEncoding: "", wantCompressed: false},
		{name: "client accepts only deflate", target: "/site.css", acceptEncoding: "deflate", wantCompressed: false},
		{name: "extensionless path", target: "/about", acceptEncoding: "gzip", wantCompressed: true},
		{name: "png is already compressed", target: "/logo.png", acceptEncoding: "gzip", wantCompressed: false},
		{name: "archive is already compressed", target: "/bundle.zip", acceptEncoding: "gzip", wantCompressed: false},
		{name: "font is already compressed", target: "/font.woff2", acceptEncoding: "gzip", wantCompressed: false},
		{name: "extension matching is case insensitive", target: "/PHOTO.JPG", acceptEncoding: "gzip", wantCompressed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}
			rec := httptest.NewRecorder()
			gzipMiddleware(echoHandler()).ServeHTTP(rec, req)

			// Shared caches must key on Accept-Encoding either way.
			if got, want := rec.Header().Get("Vary"), "Accept-Encoding"; got != want {
				t.Errorf("Vary = %q, want %q", got, want)
			}

			encoding := rec.Header().Get("Content-Encoding")
			if tt.wantCompressed && encoding != "gzip" {
				t.Fatalf("Content-Encoding = %q, want %q", encoding, "gzip")
			}
			if !tt.wantCompressed && encoding != "" {
				t.Fatalf("Content-Encoding = %q, want it to be absent", encoding)
			}

			want := strings.Repeat("compress me please\n", 500)
			if !tt.wantCompressed {
				if got := rec.Body.String(); got != want {
					t.Errorf("body length = %d, want %d", len(got), len(want))
				}
				return
			}

			if rec.Body.Len() >= len(want) {
				t.Errorf("compressed body is %d bytes, not smaller than the %d byte original", rec.Body.Len(), len(want))
			}

			zr, err := gzip.NewReader(rec.Body)
			if err != nil {
				t.Fatalf("body is not valid gzip: %v", err)
			}
			defer zr.Close()
			got, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("decompressing body: %v", err)
			}
			if string(got) != want {
				t.Errorf("decompressed body length = %d, want %d", len(got), len(want))
			}
		})
	}
}

// The compressed body has a different length than the handler announced, so
// the stale Content-Length must be dropped.
func TestGzipMiddlewareDropsContentLength(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/site.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()

	gzipMiddleware(echoHandler()).ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it to be absent", got)
	}
}

func TestGzipMiddlewarePassesStatusThrough(t *testing.T) {
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))

	req := httptest.NewRequest(http.MethodGet, "/missing.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusGone)
	}
}

// setHTTPSPromote toggles the flag for the duration of a test.
func setHTTPSPromote(t *testing.T, enabled bool) {
	t.Helper()
	prev := httpsPromote
	httpsPromote = &enabled
	t.Cleanup(func() { httpsPromote = prev })
}

func TestHandleReqHTTPSPromotion(t *testing.T) {
	tests := []struct {
		name             string
		promote          bool
		forwardedProto   string
		wantStatus       int
		wantLocation     string
		wantHandlerCalls bool
	}{
		{
			name:           "redirects proxied http requests",
			promote:        true,
			forwardedProto: "http",
			wantStatus:     http.StatusMovedPermanently,
			wantLocation:   "https://example.com/a/b.html",
		},
		{
			name:             "leaves https requests alone",
			promote:          true,
			forwardedProto:   "https",
			wantStatus:       http.StatusOK,
			wantHandlerCalls: true,
		},
		{
			name:             "leaves requests without the header alone",
			promote:          true,
			forwardedProto:   "",
			wantStatus:       http.StatusOK,
			wantHandlerCalls: true,
		},
		{
			name:             "does nothing when promotion is disabled",
			promote:          false,
			forwardedProto:   "http",
			wantStatus:       http.StatusOK,
			wantHandlerCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHTTPSPromote(t, tt.promote)

			var served bool
			handler := handleReq(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served = true
			}))

			// A path-only target keeps RequestURI in origin form, which is
			// what a real server receives.
			req := httptest.NewRequest(http.MethodGet, "/a/b.html", nil)
			req.Host = "example.com"
			if tt.forwardedProto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.forwardedProto)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("Location = %q, want %q", got, tt.wantLocation)
			}
			if served != tt.wantHandlerCalls {
				t.Errorf("next handler served = %v, want %v", served, tt.wantHandlerCalls)
			}
		})
	}
}

func TestHandleIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{name: "IPv4", remoteAddr: "192.0.2.1:12345", want: "192.0.2.1"},
		{name: "IPv6", remoteAddr: "[2001:db8::1]:12345", want: "2001:db8::1"},
		{name: "X-Forwarded-For", remoteAddr: "192.0.2.1:12345", headers: map[string]string{"X-Forwarded-For": "198.51.100.2, 192.0.2.10"}, want: "198.51.100.2"},
		{name: "X-Real-IP", remoteAddr: "192.0.2.1:12345", headers: map[string]string{"X-Real-IP": "198.51.100.3"}, want: "198.51.100.3"},
		{name: "Forwarded", remoteAddr: "192.0.2.1:12345", headers: map[string]string{"Forwarded": `for="[2001:db8::2]:1234";proto=https`}, want: "2001:db8::2"},
		{name: "invalid forwarded header falls back to observed address", remoteAddr: "192.0.2.1:12345", headers: map[string]string{"X-Forwarded-For": "not-an-ip"}, want: "192.0.2.1"},
		{name: "X-Forwarded-For takes precedence", remoteAddr: "192.0.2.1:12345", headers: map[string]string{"X-Forwarded-For": "198.51.100.2", "X-Real-IP": "198.51.100.3"}, want: "198.51.100.2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = tt.remoteAddr
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()

			handleIP(rec, req)

			if got := rec.Body.String(); got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q, want %q", got, "text/plain; charset=utf-8")
			}
		})
	}
}

func TestHandleDNS(t *testing.T) {
	tests := []struct {
		name        string
		remoteAddr  string
		headers     map[string]string
		lookupIPs   string
		lookupNames []string
		lookupErr   error
		wantStatus  int
		wantBody    string
	}{
		{
			name:        "returns PTR record for client IP",
			remoteAddr:  "192.0.2.1:12345",
			lookupIPs:   "192.0.2.1",
			lookupNames: []string{"ptr.example."},
			wantStatus:  http.StatusOK,
			wantBody:    "ptr.example.",
		},
		{
			name:        "uses forwarded client IP",
			remoteAddr:  "192.0.2.1:12345",
			headers:     map[string]string{"X-Forwarded-For": "198.51.100.2, 192.0.2.10"},
			lookupIPs:   "198.51.100.2",
			lookupNames: []string{"forwarded.example."},
			wantStatus:  http.StatusOK,
			wantBody:    "forwarded.example.",
		},
		{
			name:       "returns not found when PTR record is absent",
			remoteAddr: "192.0.2.1:12345",
			lookupIPs:  "192.0.2.1",
			lookupErr:  &net.DNSError{IsNotFound: true},
			wantStatus: http.StatusNotFound,
			wantBody:   "PTR record not found\n",
		},
		{
			name:       "returns bad gateway when DNS lookup fails",
			remoteAddr: "192.0.2.1:12345",
			lookupIPs:  "192.0.2.1",
			lookupErr:  errors.New("resolver unavailable"),
			wantStatus: http.StatusBadGateway,
			wantBody:   "PTR lookup failed\n",
		},
		{
			name:       "rejects invalid client address",
			remoteAddr: "not-an-ip",
			wantStatus: http.StatusBadRequest,
			wantBody:   "Unable to determine client IP\n",
		},
		{
			name:        "returns not found for an empty lookup result",
			remoteAddr:  "192.0.2.1:12345",
			lookupIPs:   "192.0.2.1",
			lookupNames: []string{},
			wantStatus:  http.StatusNotFound,
			wantBody:    "PTR record not found\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/dns", nil)
			req.RemoteAddr = tt.remoteAddr
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()
			lookupCalled := false

			handleDNSWithLookup(rec, req, func(ip string) ([]string, error) {
				lookupCalled = true
				if ip != tt.lookupIPs {
					t.Errorf("lookup IP = %q, want %q", ip, tt.lookupIPs)
				}
				return tt.lookupNames, tt.lookupErr
			})

			if got := rec.Code; got != tt.wantStatus {
				t.Errorf("status = %d, want %d", got, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q, want %q", got, "text/plain; charset=utf-8")
			}
			if lookupCalled != (tt.lookupIPs != "") {
				t.Errorf("lookup called = %v, want %v", lookupCalled, tt.lookupIPs != "")
			}
		})
	}
}

func TestHandleJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.2, 192.0.2.10")
	rec := httptest.NewRecorder()

	dnsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleDNSWithLookup(w, r, func(ip string) ([]string, error) {
			if ip != "198.51.100.2" {
				t.Errorf("lookup IP = %q, want %q", ip, "198.51.100.2")
			}
			return []string{"ptr.example."}, nil
		})
	})
	handleJSONWithHandlers(rec, req, dnsHandler, handleIP)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	var got []string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	want := []string{"ptr.example.", "198.51.100.2"}
	if len(got) != len(want) {
		t.Fatalf("response array length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("response[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSetupLogger(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })

	tests := []struct {
		level string
		want  zerolog.Level
	}{
		{level: "error", want: zerolog.ErrorLevel},
		{level: "warn", want: zerolog.WarnLevel},
		{level: "info", want: zerolog.InfoLevel},
		{level: "debug", want: zerolog.DebugLevel},
		{level: "nonsense", want: zerolog.InfoLevel},
		{level: "", want: zerolog.InfoLevel},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			setupLogger(tt.level)
			if got := zerolog.GlobalLevel(); got != tt.want {
				t.Errorf("global level = %v, want %v", got, tt.want)
			}
		})
	}
}
