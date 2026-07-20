package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/iamcc30/codexm/internal/monitor"
)

//go:embed static/*
var assets embed.FS

type Options struct {
	Listen      string
	LAN         bool
	NoOpen      bool
	RotateToken bool
	RuntimeHome string
	OnReady     func(accessURL string)
}

type Server struct {
	store *monitor.Store
	opts  Options
	token string
}

func New(store *monitor.Store, opts Options) (*Server, error) {
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("invalid dashboard listen address %q: %w", opts.Listen, err)
	}
	if !opts.LAN && !isLoopbackHost(host) {
		return nil, errors.New("non-loopback dashboard listeners require --lan")
	}
	if opts.RuntimeHome == "" {
		return nil, errors.New("dashboard runtime home is required")
	}
	token, err := dashboardToken(opts.RuntimeHome, opts.RotateToken)
	if err != nil {
		return nil, err
	}
	return &Server{store: store, opts: opts, token: token}, nil
}

func (s *Server) Run(ctx context.Context) (string, error) {
	listener, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return "", fmt.Errorf("listen for dashboard: %w", err)
	}
	defer listener.Close()
	scheme := "http"
	serveListener := listener
	var certFile, keyFile string
	if s.opts.LAN {
		scheme = "https"
		certFile, keyFile, err = ensureCertificate(s.opts.RuntimeHome, listener.Addr().String())
		if err != nil {
			return "", err
		}
	}
	displayHost := listener.Addr().String()
	host, port, _ := net.SplitHostPort(displayHost)
	if host == "::" || host == "0.0.0.0" {
		host = firstLANAddress()
		displayHost = net.JoinHostPort(host, port)
	}
	baseURL := scheme + "://" + displayHost
	accessURL := baseURL + "/?token=" + url.QueryEscape(s.token)
	server := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if s.opts.LAN {
			errCh <- server.ServeTLS(serveListener, certFile, keyFile)
		} else {
			errCh <- server.Serve(serveListener)
		}
	}()
	if s.opts.OnReady != nil {
		s.opts.OnReady(accessURL)
	}
	if !s.opts.NoOpen {
		_ = OpenBrowser(accessURL)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err = <-errCh
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return accessURL, err
}

// ListenURL prepares a listener long enough for CLI callers to print a stable
// URL only after Run has actually bound. It is intentionally not exposed;
// callers should use OnReady when browser behavior matters.
func (s *Server) AccessToken() string { return s.token }

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", s.authenticate)
	mux.HandleFunc("/api/v1/snapshot", s.requireAuth(s.snapshot))
	mux.HandleFunc("/api/v1/sessions", s.requireAuth(s.sessions))
	mux.HandleFunc("/api/v1/events", s.requireAuth(s.events))
	mux.HandleFunc("/", s.root)
	return securityHeaders(mux)
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/static/") {
		http.NotFound(w, r)
		return
	}
	if token := r.URL.Query().Get("token"); token != "" {
		if subtleEqual(token, s.token) {
			s.setCookie(w)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.Error(w, "invalid dashboard token", http.StatusUnauthorized)
		return
	}
	if !s.authenticated(r) {
		http.Error(w, "dashboard authentication required; open the tokenized URL printed by codexm", http.StatusUnauthorized)
		return
	}
	path := "static/index.html"
	contentType := "text/html; charset=utf-8"
	if strings.HasPrefix(r.URL.Path, "/static/") {
		path = strings.TrimPrefix(r.URL.Path, "/")
		switch filepath.Ext(path) {
		case ".css":
			contentType = "text/css; charset=utf-8"
		case ".js":
			contentType = "text/javascript; charset=utf-8"
		case ".svg":
			contentType = "image/svg+xml"
		}
	}
	data, err := assets.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) {
	if !subtleEqual(r.URL.Query().Get("token"), s.token) {
		http.Error(w, "invalid dashboard token", http.StatusUnauthorized)
		return
	}
	s.setCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) snapshot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(webSnapshot(s.store.Snapshot()))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	updates, unsubscribe := s.store.Subscribe()
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			data, err := json.Marshal(webSnapshot(snapshot))
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func webSnapshot(snapshot monitor.Snapshot) monitor.Snapshot {
	// Sessions have a dedicated filtered, sorted, paginated endpoint. Keeping
	// them out of snapshot/SSE prevents a large history from being retransmitted
	// on every two-second status update.
	snapshot.Sessions = nil
	return snapshot
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie("codexm_dashboard")
	return err == nil && subtleEqual(cookie.Value, s.token)
}

func (s *Server) setCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: "codexm_dashboard", Value: s.token, Path: "/",
		HttpOnly: true, Secure: s.opts.LAN, SameSite: http.SameSiteStrictMode,
		MaxAge: 12 * 60 * 60,
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func dashboardToken(home string, rotate bool) (string, error) {
	dir := filepath.Join(home, "runtime", "dashboard")
	if err := secureDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "access.token")
	if !rotate {
		if data, err := os.ReadFile(path); err == nil && len(data) >= 32 {
			return string(data), nil
		}
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	if err := secureWrite(path, []byte(token)); err != nil {
		return "", err
	}
	return token, nil
}

func ensureCertificate(home, address string) (string, string, error) {
	dir := filepath.Join(home, "runtime", "dashboard")
	if err := secureDir(dir); err != nil {
		return "", "", err
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	requiredIPs := certificateIPs(address)
	if cert, err := readCertificate(certPath); err == nil &&
		time.Until(cert.NotAfter) > 30*24*time.Hour &&
		certificateHasIPs(cert, requiredIPs) {
		if _, keyErr := tls.LoadX509KeyPair(certPath, keyPath); keyErr == nil {
			return certPath, keyPath, nil
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "codexm dashboard"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
		IPAddresses: requiredIPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := secureWrite(certPath, certPEM); err != nil {
		return "", "", err
	}
	if err := secureWrite(keyPath, keyPEM); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func certificateIPs(address string) []net.IP {
	values := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	host, _, _ := net.SplitHostPort(address)
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		values = append(values, ip)
	}
	ifaces, _ := net.InterfaceAddrs()
	for _, addr := range ifaces {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip != nil && !ip.IsLoopback() {
			values = append(values, ip)
		}
	}
	unique := make([]net.IP, 0, len(values))
	for _, value := range values {
		if value == nil || certificateIPContains(unique, value) {
			continue
		}
		unique = append(unique, value)
	}
	return unique
}

func certificateHasIPs(cert *x509.Certificate, expected []net.IP) bool {
	for _, value := range expected {
		if !certificateIPContains(cert.IPAddresses, value) {
			return false
		}
	}
	return true
}

func certificateIPContains(values []net.IP, expected net.IP) bool {
	for _, value := range values {
		if value.Equal(expected) {
			return true
		}
	}
	return false
}

func readCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

func secureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(path, 0o700)
	}
	return nil
}

func secureWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0o600)
	}
	return os.Rename(tmp, path)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstLANAddress() string {
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip != nil && !ip.IsLoopback() && ip.To4() != nil {
			return ip.String()
		}
	}
	return "127.0.0.1"
}

func subtleEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := range left {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

func OpenBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
