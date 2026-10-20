package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"marketplace/internal/api"
)

func newTestServer(t *testing.T, dst io.Writer) *Server {
	t.Helper()
	c := Config{AccessKey: strings.Repeat("a", 32), RefreshKey: strings.Repeat("r", 32)}
	s, err := New(nil, c, slog.New(slog.NewJSONHandler(dst, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func accessFor(t *testing.T, s *Server, id int64, role string) string {
	t.Helper()
	v, err := s.sign(api.UserResponse{ID: id, Role: api.Role(role)}, "access", newID(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func call(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func checkCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if !tokenIDPattern.MatchString(w.Header().Get("X-Request-Id")) {
		t.Fatal("missing request id")
	}
	if code != "" {
		var e api.ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || string(e.ErrorCode) != code {
			t.Fatalf("expected %s: %s, %v", code, w.Body.String(), err)
		}
		if code == "VALIDATION_ERROR" && (e.Details == nil || len(e.Details.Fields) == 0) {
			t.Fatal("missing validation details")
		}
	}
}

func TestContractValidation(t *testing.T) {
	s := newTestServer(t, io.Discard)
	token := accessFor(t, s, 1, "ADMIN")
	good := `{"name":"x","price":0.29,"stock":0,"category":"books","status":"ACTIVE"}`
	goodPromo := `{"code":"SAVE10","discount_type":"PERCENTAGE","discount_value":10,"min_order_amount":0,"max_uses":2147483647,"valid_from":"2025-01-01T00:00:00Z","valid_until":"2030-01-01T00:00:00Z"}`
	largeItems := strings.Repeat(`{"product_id":1,"quantity":1},`, 50) + `{"product_id":2,"quantity":1}`
	cases := []struct {
		name, method, path, body string
		valid                    bool
	}{
		{"zero stock", "POST", "/products", good, true},
		{"description null", "POST", "/products", strings.Replace(good, `"name":"x"`, `"name":"x","description":null`, 1), true},
		{"max price", "POST", "/products", strings.Replace(good, "0.29", "9999999999.99", 1), true},
		{"large price", "POST", "/products", strings.Replace(good, "0.29", "1000000.01", 1), true},
		{"too high price", "POST", "/products", strings.Replace(good, "0.29", "10000000000.00", 1), false},
		{"sub-cent", "POST", "/products", strings.Replace(good, "0.29", "0.291", 1), false},
		{"zero price", "POST", "/products", strings.Replace(good, "0.29", "0", 1), false},
		{"negative stock", "POST", "/products", strings.Replace(good, `"stock":0`, `"stock":-1`, 1), false},
		{"missing stock", "POST", "/products", strings.Replace(good, `,"stock":0`, "", 1), false},
		{"empty name", "POST", "/products", strings.Replace(good, `"x"`, `""`, 1), false},
		{"long unicode name", "POST", "/products", strings.Replace(good, `"x"`, `"`+strings.Repeat("я", 256)+`"`, 1), false},
		{"allowed unicode name", "POST", "/products", strings.Replace(good, `"x"`, `"`+strings.Repeat("я", 255)+`"`, 1), true},
		{"long description", "POST", "/products", strings.Replace(good, `"name":"x"`, `"name":"x","description":"`+strings.Repeat("a", 4001)+`"`, 1), false},
		{"long category", "POST", "/products", strings.Replace(good, "books", strings.Repeat("a", 101), 1), false},
		{"bad status", "POST", "/products", strings.Replace(good, "ACTIVE", "DELETED", 1), false},
		{"id in body", "POST", "/products", strings.Replace(good, `"name"`, `"id":1,"name"`, 1), false},
		{"null body", "POST", "/products", "null", false},
		{"empty body", "POST", "/products", "", false},
		{"two documents", "POST", "/products", good + good, false},
		{"bad json", "POST", "/products", `{"name":`, false},
		{"negative page", "GET", "/products?page=-1", "", false},
		{"size zero", "GET", "/products?size=0", "", false},
		{"size huge", "GET", "/products?size=101", "", false},
		{"bad filter", "GET", "/products?status=BAD", "", false},
		{"defaults", "GET", "/products", "", true},
		{"bad id", "GET", "/products/abc", "", false},
		{"empty items", "POST", "/orders", `{"items":[]}`, false},
		{"too many items", "POST", "/orders", `{"items":[` + largeItems + `]}`, false},
		{"qty zero", "POST", "/orders", `{"items":[{"product_id":1,"quantity":0}]}`, false},
		{"qty huge", "POST", "/orders", `{"items":[{"product_id":1,"quantity":1000}]}`, false},
		{"qty max", "POST", "/orders", `{"items":[{"product_id":1,"quantity":999}]}`, true},
		{"bad promo", "POST", "/orders", `{"items":[{"product_id":1,"quantity":1}],"promo_code":"bad"}`, false},
		{"duplicate id", "POST", "/orders", `{"items":[{"product_id":1,"quantity":1},{"product_id":1,"quantity":2}]}`, false},
		{"cannot patch status", "PUT", "/orders/1", `{"items":[{"product_id":1,"quantity":1}],"status":"PAID"}`, false},
		{"cannot register admin", "POST", "/auth/register", `{"email":"a@example.test","password":"password123","role":"ADMIN"}`, false},
		{"max promo uses", "POST", "/promo-codes", goodPromo, true},
		{"promo uses overflow", "POST", "/promo-codes", strings.Replace(goodPromo, "2147483647", "2147483648", 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := s.logging(s.validate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(204)
			})))
			w := call(h, tc.method, tc.path, token, tc.body)
			if tc.valid {
				checkCode(t, w, 204, "")
			} else {
				checkCode(t, w, 400, "VALIDATION_ERROR")
			}
			if reached != tc.valid {
				t.Fatalf("handler called: %v", reached)
			}
		})
	}
}

func TestRoleMatrix(t *testing.T) {
	s := newTestServer(t, io.Discard)
	h := s.logging(s.validate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	cases := []struct {
		method, path, body string
		allowed            map[string]bool
	}{
		{"GET", "/products", "", map[string]bool{"USER": true, "SELLER": true, "ADMIN": true}},
		{"GET", "/products/1", "", map[string]bool{"USER": true, "SELLER": true, "ADMIN": true}},
		{"POST", "/products", `{"name":"x","price":1,"stock":1,"category":"c","status":"ACTIVE"}`, map[string]bool{"SELLER": true, "ADMIN": true}},
		{"PUT", "/products/1", `{"name":"x","price":1,"stock":1,"category":"c","status":"ACTIVE"}`, map[string]bool{"SELLER": true, "ADMIN": true}},
		{"DELETE", "/products/1", "", map[string]bool{"SELLER": true, "ADMIN": true}},
		{"POST", "/orders", `{"items":[{"product_id":1,"quantity":1}]}`, map[string]bool{"USER": true, "ADMIN": true}},
		{"GET", "/orders/1", "", map[string]bool{"USER": true, "ADMIN": true}},
		{"PUT", "/orders/1", `{"items":[{"product_id":1,"quantity":1}]}`, map[string]bool{"USER": true, "ADMIN": true}},
		{"POST", "/orders/1/cancel", "", map[string]bool{"USER": true, "ADMIN": true}},
		{"POST", "/promo-codes", `{"code":"SAVE10","discount_type":"PERCENTAGE","discount_value":10,"min_order_amount":0,"max_uses":1,"valid_from":"2025-01-01T00:00:00Z","valid_until":"2030-01-01T00:00:00Z"}`, map[string]bool{"SELLER": true, "ADMIN": true}},
	}
	for _, role := range []string{"USER", "SELLER", "ADMIN"} {
		token := accessFor(t, s, 1, role)
		for _, tc := range cases {
			w := call(h, tc.method, tc.path, token, tc.body)
			if tc.allowed[role] {
				checkCode(t, w, 204, "")
			} else {
				checkCode(t, w, 403, "ACCESS_DENIED")
			}
		}
	}
}

func TestLogsAndAuthErrors(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServer(t, &buf)
	h := s.logging(s.validate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	w := call(h, "POST", "/auth/login", "", `{"email":"a@example.test","password":"dont-log-this"}`)
	checkCode(t, w, 204, "")
	if strings.Contains(buf.String(), "dont-log-this") {
		t.Fatal("password leaked")
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"request_id", "method", "endpoint", "status_code", "duration_ms", "user_id", "timestamp", "request_body"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if entry["request_id"] != w.Header().Get("X-Request-Id") || entry["user_id"] != nil {
		t.Fatal("invalid request metadata")
	}
	w2 := call(h, "GET", "/products", "", "")
	checkCode(t, w2, 401, "TOKEN_INVALID")
	if w.Header().Get("X-Request-Id") == w2.Header().Get("X-Request-Id") {
		t.Fatal("request id reused")
	}
	expired, err := s.sign(api.UserResponse{ID: 1, Role: "USER"}, "access", newID(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	checkCode(t, call(h, "GET", "/products", expired, ""), 401, "TOKEN_EXPIRED")
	checkCode(t, call(h, "GET", "/products", "bad.token", ""), 401, "TOKEN_INVALID")
	checkCode(t, call(h, "GET", "/unknown", "", ""), 404, "ROUTE_NOT_FOUND")
	checkCode(t, call(h, "PATCH", "/products/1", "", ""), 405, "METHOD_NOT_ALLOWED")
}
