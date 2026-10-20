package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"

	"marketplace/internal/api"
)

type fixture struct {
	t    *testing.T
	s    *Server
	h    http.Handler
	hash string
}

func integration(t *testing.T, limit time.Duration) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; run make integration")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	var version string
	if err = db.QueryRowContext(ctx, `SELECT version FROM flyway_schema_history WHERE success ORDER BY installed_rank DESC LIMIT 1`).Scan(&version); err != nil {
		t.Fatal("run Flyway migrations first:", err)
	}
	if version != "2" {
		t.Fatal("expected migration 2, got", version)
	}
	c := Config{AccessKey: strings.Repeat("a", 32), RefreshKey: strings.Repeat("r", 32), OrderLimit: limit}
	s, err := New(db, c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, s: s, h: s.Handler(), hash: string(hash)}
}

func body(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(w.Body.String(), err)
	}
	return out
}

func (f *fixture) request(method, path, token string, v any, status int, code string) *httptest.ResponseRecorder {
	f.t.Helper()
	b := ""
	if v != nil {
		b = body(v)
	}
	w := call(f.h, method, path, token, b)
	checkCode(f.t, w, status, code)
	req := httptest.NewRequest(method, path, nil)
	route, params, err := f.s.router.FindRoute(req)
	if err != nil {
		f.t.Fatal(err)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 w.Code, Header: w.Header(),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(w.Body.Bytes())
	if err = openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		f.t.Fatalf("response does not match OpenAPI: %v; body: %s", err, w.Body.String())
	}
	return w
}

func (f *fixture) user(role string) (int64, string) {
	f.t.Helper()
	var id int64
	err := f.s.db.QueryRow("INSERT INTO users(email,password_hash,role) VALUES($1,$2,$3) RETURNING id", newID()+"@example.test", f.hash, role).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	return id, accessFor(f.t, f.s, id, role)
}

func productBody(stock int64, price int, category string) map[string]any {
	return map[string]any{"name": "book", "price": price, "stock": stock, "category": category, "status": "ACTIVE"}
}

func (f *fixture) product(token string, stock int64) api.ProductResponse {
	w := f.request("POST", "/products", token, productBody(stock, 100, newID()), 201, "")
	return decode[api.ProductResponse](f.t, w)
}

func (f *fixture) promo(token string, uses int, minimum int) api.PromoCodeResponse {
	code := "P_" + strings.ToUpper(strings.ReplaceAll(newID(), "-", ""))[:12]
	v := map[string]any{"code": code, "discount_type": "PERCENTAGE", "discount_value": 10, "min_order_amount": minimum, "max_uses": uses,
		"valid_from": time.Now().Add(-time.Minute).UTC(), "valid_until": time.Now().Add(time.Hour).UTC()}
	return decode[api.PromoCodeResponse](f.t, f.request("POST", "/promo-codes", token, v, 201, ""))
}

func orderBody(id, qty int64, code string) map[string]any {
	v := map[string]any{"items": []map[string]any{{"product_id": id, "quantity": qty}}}
	if code != "" {
		v["promo_code"] = code
	}
	return v
}

func (f *fixture) number(q string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.s.db.QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) stock(id, want int64) {
	f.t.Helper()
	if n := f.number("SELECT stock FROM products WHERE id=$1", id); n != want {
		f.t.Fatalf("stock %d, want %d", n, want)
	}
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.s.db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func TestIntegrationProducts(t *testing.T) {
	f := integration(t, 0)
	sellerID, seller := f.user("SELLER")
	_, other := f.user("SELLER")
	_, user := f.user("USER")
	_, admin := f.user("ADMIN")
	f.request("POST", "/products", user, productBody(1, 100, "books"), 403, "ACCESS_DENIED")
	p := f.product(seller, 0)
	path := fmt.Sprintf("/products/%d", p.ID)
	for _, token := range []string{seller, other, user, admin} {
		f.request("GET", path, token, nil, 200, "")
	}
	f.request("PUT", path, other, productBody(5, 200, p.Category), 403, "ACCESS_DENIED")
	f.request("DELETE", path, other, nil, 403, "ACCESS_DENIED")
	p2 := decode[api.ProductResponse](t, f.request("PUT", path, seller, productBody(5, 200, p.Category), 200, ""))
	if p2.Price != 20000 || !p2.UpdatedAt.After(p.CreatedAt) || !p2.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("incorrect update: %+v", p2)
	}
	f.request("DELETE", path, admin, nil, 204, "")
	f.request("DELETE", path, seller, nil, 204, "")
	p3 := decode[api.ProductResponse](t, f.request("GET", path, user, nil, 200, ""))
	if p3.Status != "ARCHIVED" || p3.SellerID != sellerID {
		t.Fatal("not archived")
	}
	page := decode[api.ProductPage](t, f.request("GET", "/products?status=ARCHIVED&category="+p.Category, user, nil, 200, ""))
	if page.TotalElements != 1 || page.Page != 0 || page.Size != 20 || len(page.Content) != 1 {
		t.Fatalf("invalid page: %+v", page)
	}
	empty := decode[api.ProductPage](t, f.request("GET", "/products?page=9&size=1&category="+p.Category, user, nil, 200, ""))
	if empty.TotalElements != 1 || empty.Content == nil || len(empty.Content) != 0 {
		t.Fatalf("invalid empty page: %+v", empty)
	}
	v := productBody(1, 100, newID())
	v["seller_id"] = sellerID
	f.request("POST", "/products", other, v, 403, "ACCESS_DENIED")
	p4 := decode[api.ProductResponse](t, f.request("POST", "/products", admin, v, 201, ""))
	if p4.SellerID != sellerID {
		t.Fatal("admin could not choose seller")
	}
	f.request("GET", "/products/9223372036854775806", user, nil, 404, "PRODUCT_NOT_FOUND")
}

func TestIntegrationOrderRollbackAndPromo(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	uid, user := f.user("USER")
	otherID, other := f.user("USER")
	p := f.product(seller, 5)
	promo := f.promo(seller, 1, 200)
	o := decode[api.OrderResponse](t, f.request("POST", "/orders", user, orderBody(p.ID, 3, promo.Code), 201, ""))
	path := fmt.Sprintf("/orders/%d", o.ID)
	if o.TotalAmount != 27000 || o.DiscountAmount != 3000 || o.UserID != uid || o.Status != "CREATED" {
		t.Fatalf("wrong order: %+v", o)
	}
	f.stock(p.ID, 2)
	f.request("POST", "/orders", user, orderBody(p.ID, 1, ""), 409, "ORDER_HAS_ACTIVE")
	f.request("GET", path, other, nil, 403, "ORDER_OWNERSHIP_VIOLATION")
	f.request("PUT", path, other, orderBody(p.ID, 1, ""), 403, "ORDER_OWNERSHIP_VIOLATION")
	f.request("POST", path+"/cancel", other, nil, 403, "ORDER_OWNERSHIP_VIOLATION")
	f.request("PUT", path, user, orderBody(p.ID, 7, ""), 409, "INSUFFICIENT_STOCK")
	f.stock(p.ID, 2)
	f.request("PUT", path, user, orderBody(9223372036854775806, 1, ""), 404, "PRODUCT_NOT_FOUND")
	f.stock(p.ID, 2)
	if n := f.number("SELECT count(*) FROM user_operations WHERE user_id=$1 AND operation_type='UPDATE_ORDER'", uid); n != 0 {
		t.Fatal("failed update recorded")
	}
	f.request("PUT", fmt.Sprintf("/products/%d", p.ID), seller, productBody(2, 200, p.Category), 200, "")
	old := decode[api.OrderResponse](t, f.request("GET", path, user, nil, 200, ""))
	if old.Items[0].PriceAtOrder != 10000 || old.TotalAmount != 27000 {
		t.Fatal("snapshot changed")
	}
	o = decode[api.OrderResponse](t, f.request("PUT", path, user, orderBody(p.ID, 4, ""), 200, ""))
	if o.TotalAmount != 36000 || o.Items[0].PriceAtOrder != 10000 || f.number("SELECT current_uses FROM promo_codes WHERE id=$1", promo.ID) != 1 {
		t.Fatalf("update reuses promo incorrectly: %+v", o)
	}
	f.stock(p.ID, 1)
	o = decode[api.OrderResponse](t, f.request("PUT", path, user, orderBody(p.ID, 1, ""), 200, ""))
	if o.PromoCodeID != nil || o.DiscountAmount != 0 || o.TotalAmount != 10000 {
		t.Fatalf("promo was not removed: %+v", o)
	}
	if f.number("SELECT current_uses FROM promo_codes WHERE id=$1", promo.ID) != 0 {
		t.Fatal("promo use not returned")
	}
	f.stock(p.ID, 4)
	f.request("POST", path+"/cancel", user, nil, 200, "")
	f.stock(p.ID, 5)
	f.request("POST", path+"/cancel", user, nil, 409, "INVALID_STATE_TRANSITION")
	f.stock(p.ID, 5)
	f.request("POST", "/orders", other, orderBody(p.ID, 1, "NOT_FOUND"), 422, "PROMO_CODE_INVALID")
	f.stock(p.ID, 5)
	if f.number("SELECT count(*) FROM orders WHERE user_id=$1", otherID) != 0 || f.number("SELECT count(*) FROM user_operations WHERE user_id=$1", otherID) != 0 {
		t.Fatal("failed create left data")
	}
	f.request("GET", "/orders/9223372036854775806", other, nil, 404, "ORDER_NOT_FOUND")
}

func TestIntegrationRateAndStates(t *testing.T) {
	f := integration(t, 5*time.Minute)
	_, seller := f.user("SELLER")
	uid, user := f.user("USER")
	_, admin := f.user("ADMIN")
	p := f.product(seller, 30)
	o := decode[api.OrderResponse](t, f.request("POST", "/orders", user, orderBody(p.ID, 1, ""), 201, ""))
	path := fmt.Sprintf("/orders/%d", o.ID)
	w := f.request("POST", "/orders", user, orderBody(p.ID, 1, ""), 429, "ORDER_LIMIT_EXCEEDED")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry-after")
	}
	f.request("PUT", path, user, orderBody(p.ID, 2, ""), 200, "")
	f.request("PUT", path, user, orderBody(p.ID, 3, ""), 429, "ORDER_LIMIT_EXCEEDED")
	f.request("POST", path+"/status", user, map[string]any{"status": "PAYMENT_PENDING"}, 403, "ACCESS_DENIED")
	f.request("POST", path+"/status", admin, map[string]any{"status": "PAID"}, 409, "INVALID_STATE_TRANSITION")
	f.request("POST", path+"/status", admin, map[string]any{"status": "PAYMENT_PENDING"}, 200, "")
	f.request("PUT", path, user, orderBody(p.ID, 1, ""), 409, "INVALID_STATE_TRANSITION")
	f.request("POST", path+"/cancel", user, nil, 200, "")
	f.stock(p.ID, 30)
	f.request("POST", "/orders", user, orderBody(p.ID, 1, ""), 429, "ORDER_LIMIT_EXCEEDED")
	f.exec("UPDATE user_operations SET created_at=clock_timestamp()-interval '6 minutes' WHERE user_id=$1", uid)
	o = decode[api.OrderResponse](t, f.request("POST", "/orders", user, orderBody(p.ID, 1, ""), 201, ""))
	path = fmt.Sprintf("/orders/%d", o.ID)
	for _, status := range []string{"PAYMENT_PENDING", "PAID", "SHIPPED", "COMPLETED"} {
		f.request("POST", path+"/status", admin, map[string]any{"status": status}, 200, "")
	}
	f.request("POST", path+"/cancel", user, nil, 409, "INVALID_STATE_TRANSITION")
	f.request("POST", path+"/status", admin, map[string]any{"status": "PAYMENT_PENDING"}, 409, "INVALID_STATE_TRANSITION")
	f.stock(p.ID, 29)
}

func TestIntegrationAuth(t *testing.T) {
	f := integration(t, 0)
	email := newID() + "@example.test"
	credentials := map[string]any{"email": email, "password": "password123"}
	u := decode[api.UserResponse](t, f.request("POST", "/auth/register", "", credentials, 201, ""))
	if u.Role != "USER" {
		t.Fatal("wrong default role")
	}
	f.request("POST", "/auth/register", "", credentials, 409, "EMAIL_ALREADY_EXISTS")
	f.request("POST", "/auth/login", "", map[string]any{"email": email, "password": "wrong-password"}, 401, "INVALID_CREDENTIALS")
	pair := decode[api.AuthTokens](t, f.request("POST", "/auth/login", "", credentials, 200, ""))
	f.request("GET", "/products", pair.AccessToken, nil, 200, "")
	f.request("GET", "/products", pair.RefreshToken, nil, 401, "TOKEN_INVALID")
	next := decode[api.AuthTokens](t, f.request("POST", "/auth/refresh", "", map[string]any{"refresh_token": pair.RefreshToken}, 200, ""))
	if next.AccessToken == pair.AccessToken || next.RefreshToken == pair.RefreshToken || next.ExpiresIn != 900 {
		t.Fatal("tokens did not rotate")
	}
	f.request("POST", "/auth/refresh", "", map[string]any{"refresh_token": pair.RefreshToken}, 401, "REFRESH_TOKEN_INVALID")
	f.request("POST", "/auth/refresh", "", map[string]any{"refresh_token": pair.AccessToken}, 401, "REFRESH_TOKEN_INVALID")
	f.request("POST", "/auth/refresh", "", map[string]any{"refresh_token": "bad"}, 401, "REFRESH_TOKEN_INVALID")
	f.request("GET", "/products", next.AccessToken, nil, 200, "")
}

func TestIntegrationPromoErrors(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	uid, user := f.user("USER")
	p := f.product(seller, 10)
	promo := f.promo(seller, 1, 200)
	f.request("POST", "/orders", user, orderBody(p.ID, 1, promo.Code), 422, "PROMO_CODE_MIN_AMOUNT")
	f.stock(p.ID, 10)
	f.exec("UPDATE promo_codes SET active=false WHERE id=$1", promo.ID)
	f.request("POST", "/orders", user, orderBody(p.ID, 3, promo.Code), 422, "PROMO_CODE_INVALID")
	f.exec("UPDATE promo_codes SET active=true,valid_from=clock_timestamp()-interval '2 hours',valid_until=clock_timestamp()-interval '1 hour' WHERE id=$1", promo.ID)
	f.request("POST", "/orders", user, orderBody(p.ID, 3, promo.Code), 422, "PROMO_CODE_INVALID")
	f.exec("UPDATE promo_codes SET valid_until=clock_timestamp()+interval '1 hour' WHERE id=$1", promo.ID)
	o := decode[api.OrderResponse](t, f.request("POST", "/orders", user, orderBody(p.ID, 3, promo.Code), 201, ""))
	path := fmt.Sprintf("/orders/%d", o.ID)
	f.exec("UPDATE promo_codes SET active=false WHERE id=$1", promo.ID)
	f.request("PUT", path, user, orderBody(p.ID, 4, ""), 422, "PROMO_CODE_INVALID")
	f.stock(p.ID, 7)
	if f.number("SELECT count(*) FROM user_operations WHERE user_id=$1 AND operation_type='UPDATE_ORDER'", uid) != 0 {
		t.Fatal("failed promo update recorded")
	}
	f.request("POST", path+"/cancel", user, nil, 200, "")
	f.stock(p.ID, 10)
	if f.number("SELECT current_uses FROM promo_codes WHERE id=$1", promo.ID) != 0 {
		t.Fatal("cancellation did not return promo")
	}
	p2 := f.product(seller, 0)
	v := map[string]any{"items": []map[string]any{{"product_id": p.ID, "quantity": 11}, {"product_id": p2.ID, "quantity": 1}}}
	w := f.request("POST", "/orders", user, v, 409, "INSUFFICIENT_STOCK")
	e := decode[api.ErrorResponse](t, w)
	if e.Details == nil || len(e.Details.Products) != 2 {
		t.Fatal("expected both stock shortages")
	}
	f.request("DELETE", fmt.Sprintf("/products/%d", p2.ID), seller, nil, 204, "")
	f.request("POST", "/orders", user, v, 409, "PRODUCT_INACTIVE")
	f.stock(p.ID, 10)
}

func TestIntegrationConcurrentStock(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	p := f.product(seller, 1)
	tokens := make([]string, 8)
	for i := range tokens {
		_, tokens[i] = f.user("USER")
	}
	b := body(orderBody(p.ID, 1, ""))
	start := make(chan struct{})
	out := make(chan *httptest.ResponseRecorder, len(tokens))
	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			<-start
			out <- call(f.h, "POST", "/orders", token, b)
		}(token)
	}
	close(start)
	wg.Wait()
	close(out)
	ok := 0
	for w := range out {
		if w.Code == 201 {
			ok++
		} else {
			checkCode(t, w, 409, "INSUFFICIENT_STOCK")
		}
	}
	if ok != 1 {
		t.Fatalf("created %d orders", ok)
	}
	f.stock(p.ID, 0)
}

func TestIntegrationConcurrentActiveOrder(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	uid, user := f.user("USER")
	p := f.product(seller, 20)
	b := body(orderBody(p.ID, 1, ""))
	start := make(chan struct{})
	out := make(chan *httptest.ResponseRecorder, 6)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; out <- call(f.h, "POST", "/orders", user, b) }()
	}
	close(start)
	wg.Wait()
	close(out)
	ok := 0
	for w := range out {
		if w.Code == 201 {
			ok++
		} else {
			checkCode(t, w, 409, "ORDER_HAS_ACTIVE")
		}
	}
	if ok != 1 || f.number("SELECT count(*) FROM orders WHERE user_id=$1", uid) != 1 {
		t.Fatal("more than one active order")
	}
	f.stock(p.ID, 19)
}

func TestIntegrationConcurrentPromo(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	promo := f.promo(seller, 1, 0)
	_, a := f.user("USER")
	_, b := f.user("USER")
	p := f.product(seller, 2)
	q := f.product(seller, 2)
	start := make(chan struct{})
	out := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, v := range []struct {
		token string
		id    int64
	}{{a, p.ID}, {b, q.ID}} {
		wg.Add(1)
		go func(token string, id int64) {
			defer wg.Done()
			<-start
			out <- call(f.h, "POST", "/orders", token, body(orderBody(id, 1, promo.Code)))
		}(v.token, v.id)
	}
	close(start)
	wg.Wait()
	close(out)
	ok := 0
	for w := range out {
		if w.Code == 201 {
			ok++
		} else {
			checkCode(t, w, 422, "PROMO_CODE_INVALID")
		}
	}
	if ok != 1 || f.number("SELECT current_uses FROM promo_codes WHERE id=$1", promo.ID) != 1 || f.number("SELECT sum(stock) FROM products WHERE id IN ($1,$2)", p.ID, q.ID) != 3 {
		t.Fatal("promo or stock race")
	}
}
