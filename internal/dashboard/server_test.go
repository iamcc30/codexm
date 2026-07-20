package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iamcc30/codexm/internal/monitor"
)

func TestAPIsRequireAuthenticationAndLANCookieIsSecure(t *testing.T) {
	store := monitor.NewStore()
	server, err := New(store, Options{RuntimeHome: t.TempDir(), LAN: true, Listen: "0.0.0.0:0"})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.handler()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API returned %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/?token="+server.token, nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("token authentication returned %d", response.Code)
	}
	cookie := response.Header().Get("Set-Cookie")
	for _, expected := range []string{"HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(cookie, expected) {
			t.Fatalf("cookie is missing %s: %s", expected, cookie)
		}
	}
}

func TestSessionEndpointFiltersSortsAndPaginatesLargeHistory(t *testing.T) {
	store := monitor.NewStore()
	snapshot := monitor.Snapshot{GeneratedAt: time.Now()}
	for i := 0; i < 5000; i++ {
		snapshot.Sessions = append(snapshot.Sessions, monitor.Session{
			ID: fmt.Sprintf("thread-%04d", i), Title: fmt.Sprintf("Session %04d", i),
			Profile: []string{"one", "two"}[i%2], Project: "/project",
			Status: []string{"idle", "active"}[i%2], Source: "cli",
			UpdatedAt: time.Unix(int64(i), 0), Archived: i%3 == 0,
		})
	}
	store.Publish(snapshot)
	server, err := New(store, Options{RuntimeHome: t.TempDir(), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions?profile=two&status=active&archived=false&sort=updated&direction=desc&page=2&page_size=40", nil)
	request.AddCookie(&http.Cookie{Name: "codexm_dashboard", Value: server.token})
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("sessions endpoint returned %d: %s", response.Code, response.Body.String())
	}
	var page sessionPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 40 || page.Page != 2 || page.Total == 0 {
		t.Fatalf("unexpected page: data=%d page=%d total=%d", len(page.Data), page.Page, page.Total)
	}
	for _, item := range page.Data {
		if item.Profile != "two" || item.Status != "active" || item.Archived {
			t.Fatalf("filter leaked item: %+v", item)
		}
	}
	if !page.Data[0].UpdatedAt.After(page.Data[len(page.Data)-1].UpdatedAt) {
		t.Fatal("descending updated sort was not applied")
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	request.AddCookie(&http.Cookie{Name: "codexm_dashboard", Value: server.token})
	response = httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)
	var compact monitor.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &compact); err != nil {
		t.Fatal(err)
	}
	if compact.Sessions != nil {
		t.Fatalf("snapshot retransmitted %d sessions instead of using pagination", len(compact.Sessions))
	}
}

func TestNonLoopbackRequiresLANAndTokenRotation(t *testing.T) {
	home := t.TempDir()
	if _, err := New(monitor.NewStore(), Options{RuntimeHome: home, Listen: "0.0.0.0:0"}); err == nil {
		t.Fatal("non-loopback listener was accepted without --lan")
	}
	first, err := New(monitor.NewStore(), Options{RuntimeHome: home, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(monitor.NewStore(), Options{RuntimeHome: home, Listen: "127.0.0.1:0", RotateToken: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.token == second.token {
		t.Fatal("token rotation reused the old token")
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	request.AddCookie(&http.Cookie{Name: "codexm_dashboard", Value: first.token})
	response := httptest.NewRecorder()
	second.handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("rotated server accepted old token with status %d", response.Code)
	}
}

func TestGeneratedCertificateContainsLocalSANAndPrivateKey(t *testing.T) {
	home := t.TempDir()
	certPath, keyPath, err := ensureCertificate(home, "127.0.0.1:7443")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := readCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("loopback SAN missing: %v", err)
	}
	if !containsIP(cert.IPAddresses, net.ParseIP("127.0.0.1")) {
		t.Fatalf("certificate SANs = %v", cert.IPAddresses)
	}
	for _, path := range []string{certPath, keyPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o", path, info.Mode().Perm())
		}
	}
	before, err := os.Stat(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, _, err := ensureCertificate(home, "127.0.0.1:7443"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Fatal("certificate was not renewed after its private key disappeared")
	}
}

func TestSSEStreamsInitialSnapshotOnEveryConnection(t *testing.T) {
	store := monitor.NewStore()
	store.Publish(monitor.Snapshot{
		GeneratedAt: time.Now(), Summary: monitor.Summary{Profiles: 2},
		Sessions: []monitor.Session{{ID: "must-not-leak-through-sse"}},
	})
	server, err := New(store, Options{RuntimeHome: t.TempDir(), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/events", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: "codexm_dashboard", Value: server.token})
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(response.Body)
		var data string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		_ = response.Body.Close()
		cancel()
		if data == "" {
			t.Fatalf("connection %d did not receive an initial snapshot: %v", attempt+1, scanner.Err())
		}
		var snapshot monitor.Snapshot
		if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Summary.Profiles != 2 || snapshot.Sessions != nil {
			t.Fatalf("unexpected SSE snapshot: %+v", snapshot)
		}
	}
}

func TestEmbeddedDashboardIncludesResponsiveAndReconnectAssets(t *testing.T) {
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]struct {
		data []byte
		text string
	}{
		"responsive CSS":  {css, "@media(max-width:760px)"},
		"SSE reconnect":   {js, `new EventSource("/api/v1/events")`},
		"session id":      {js, `ID · ${esc(item.id)}`},
		"session filters": {html, `id="filter-profile"`},
		"pagination":      {html, `id="page-next"`},
	} {
		if !strings.Contains(string(value.data), value.text) {
			t.Fatalf("embedded dashboard is missing %s marker %q", name, value.text)
		}
	}
	if strings.Contains(string(js), "style=") || strings.Contains(string(html), "style=") {
		t.Fatal("embedded dashboard uses inline styles that its CSP would block")
	}
}

func containsIP(values []net.IP, expected net.IP) bool {
	for _, value := range values {
		if value.Equal(expected) {
			return true
		}
	}
	return false
}
