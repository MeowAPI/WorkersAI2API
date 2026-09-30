package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func adminRequest(s *Server, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func loginAdmin(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/admin/api/session", nil)
	r.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal(cookies)
	}
	return cookies[0], object(t, w.Body.Bytes())["csrf_token"].(string)
}
func TestAdminAuthenticationCRUDAndModels(t *testing.T) {
	s, err := New(Config{AuthToken: "client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	w := adminRequest(s, "GET", "/admin/api/state", "", nil, "")
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = adminRequest(s, "POST", "/admin/api/session", "", nil, "")
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	cookie, csrf := loginAdmin(t, s)
	body := `{"name":"first","account_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","api_token":"cloudflare-secret"}`
	w = adminRequest(s, "POST", "/admin/api/accounts", body, cookie, "")
	if w.Code != 403 {
		t.Fatal("missing CSRF accepted", w.Code)
	}
	w = adminRequest(s, "POST", "/admin/api/accounts", body, cookie, csrf)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	id := object(t, w.Body.Bytes())["id"].(string)
	if strings.Contains(w.Body.String(), "cloudflare-secret") {
		t.Fatal("secret in admin response")
	}
	w = adminRequest(s, "POST", "/admin/api/accounts", body, cookie, csrf)
	if w.Code != 409 {
		t.Fatal("duplicate accepted", w.Code)
	}
	w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, `{"name":"renamed","api_token":"","enabled":false}`, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if s.accounts.accounts[0].APIToken != "cloudflare-secret" || s.accounts.accounts[0].Enabled {
		t.Fatal("update lost token or toggle")
	}
	w = adminRequest(s, "GET", "/admin/api/state", "", cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	state := object(t, w.Body.Bytes())
	if state["routing"] != "round_robin" || state["catalog"].(map[string]any)["refresh_interval_seconds"] != float64(3600) {
		t.Fatal(w.Body)
	}
	w = adminRequest(s, "GET", "/admin/api/models", "", cookie, "")
	if w.Code != 200 || len(object(t, w.Body.Bytes())["data"].([]any)) < 60 {
		t.Fatal(w.Code, w.Body)
	}
	for _, bad := range []string{`{"unknown":"value"}`, body + ` {}`, `{"account_id":"bad"}`, `{"api_token":"has whitespace"}`} {
		w = adminRequest(s, "PATCH", "/admin/api/accounts/"+id, bad, cookie, csrf)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	w = adminRequest(s, "DELETE", "/admin/api/accounts/"+id, "", cookie, csrf)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = adminRequest(s, "DELETE", "/admin/api/session", "", cookie, csrf)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = adminRequest(s, "GET", "/admin/api/state", "", cookie, "")
	if w.Code != 401 {
		t.Fatal("logout failed", w.Code)
	}
}
func TestAdminAssetsAndForwardedImageURL(t *testing.T) {
	s, _ := New(Config{AuthToken: "client-secret"})
	for _, path := range []string{"/admin/", "/admin/app.js", "/admin/app.css"} {
		w := adminRequest(s, "GET", path, "", nil, "")
		if w.Code != 200 || w.Body.Len() == 0 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal(path, w.Code)
		}
	}
	w := adminRequest(s, "GET", "/admin/../accounts.json", "", nil, "")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "http://internal:8080/v1/images/generations", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "ai.example.com")
	if base := requestBaseURL(r); base != "https://ai.example.com" {
		t.Fatal(base)
	}
	r.Header.Set("X-Forwarded-Host", "user@evil.example")
	if base := requestBaseURL(r); base != "https://internal:8080" {
		t.Fatal(base)
	}
}
func TestAdminCookieIsNotAPIAuthentication(t *testing.T) {
	s, _ := New(Config{AuthToken: "client-secret"})
	cookie, _ := loginAdmin(t, s)
	w := adminRequest(s, "GET", "/v1/models", "", cookie, "")
	if w.Code != 401 {
		t.Fatal("management cookie authenticated public API")
	}
}
func TestAdminStateContainsNoCredentials(t *testing.T) {
	s, _ := New(Config{AuthToken: "client-secret"})
	addTestAccount(t, s, strings.Repeat("a", 32), "super-private-cloudflare-token")
	cookie, _ := loginAdmin(t, s)
	w := adminRequest(s, "GET", "/admin/api/state", "", cookie, "")
	var data any
	if json.Unmarshal(w.Body.Bytes(), &data) != nil || strings.Contains(w.Body.String(), "super-private") || strings.Contains(w.Body.String(), "client-secret") {
		t.Fatal("state leaked credentials")
	}
}
