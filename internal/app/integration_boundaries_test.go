package app

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"marketplace/internal/api"
)

func TestIntegrationAdminOrderAccess(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	userID, user := f.user("USER")
	_, admin := f.user("ADMIN")
	p := f.product(seller, 10)
	o := decode[api.OrderResponse](t, f.request("POST", "/orders", user, orderBody(p.ID, 1, ""), 201, ""))
	path := fmt.Sprintf("/orders/%d", o.ID)
	f.request("GET", path, admin, nil, 200, "")
	o = decode[api.OrderResponse](t, f.request("PUT", path, admin, orderBody(p.ID, 3, ""), 200, ""))
	if o.UserID != userID || o.TotalAmount != 30000 || o.Items[0].Quantity != 3 {
		t.Fatalf("incorrect admin update: %+v", o)
	}
	f.stock(p.ID, 7)
	f.request("POST", path+"/cancel", admin, nil, 200, "")
	f.stock(p.ID, 10)
}

func TestIntegrationDiscountBoundaries(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	for _, tc := range []struct {
		name, kind, value, price string
		quantity                 int64
		total, discount          int64
	}{
		{"fixed exceeds total", "FIXED_AMOUNT", "1000", "100", 1, 0, 10000},
		{"fixed decimal", "FIXED_AMOUNT", "2.50", "10", 2, 1750, 250},
		{"seventy percent", "PERCENTAGE", "70", "100", 1, 3000, 7000},
		{"above seventy percent", "PERCENTAGE", "70.01", "100", 1, 10000, 0},
		{"percentage rounding", "PERCENTAGE", "10", "199.90", 2, 35982, 3998},
		{"rounding respects cap", "PERCENTAGE", "70", "0.05", 1, 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fixture{t: t, s: f.s, h: f.h, hash: f.hash}
			_, user := f.user("USER")
			v := productBody(10, 1, newID())
			v["price"] = json.Number(tc.price)
			p := decode[api.ProductResponse](t, f.request("POST", "/products", seller, v, 201, ""))
			now := time.Now().UTC()
			promo := map[string]any{
				"code":          "P_" + strings.ToUpper(strings.ReplaceAll(newID(), "-", ""))[:12],
				"discount_type": tc.kind, "discount_value": json.Number(tc.value),
				"min_order_amount": 0, "max_uses": 1,
				"valid_from": now.Add(-time.Minute), "valid_until": now.Add(time.Hour),
			}
			code := decode[api.PromoCodeResponse](t, f.request("POST", "/promo-codes", seller, promo, 201, ""))
			o := decode[api.OrderResponse](t, f.request("POST", "/orders", user, orderBody(p.ID, tc.quantity, code.Code), 201, ""))
			if int64(o.TotalAmount) != tc.total || int64(o.DiscountAmount) != tc.discount {
				t.Fatalf("total=%s discount=%s, want cents %d and %d", o.TotalAmount, o.DiscountAmount, tc.total, tc.discount)
			}
			if f.number("SELECT current_uses FROM promo_codes WHERE id=$1", code.ID) != 1 {
				t.Fatal("promo use not recorded")
			}
			f.request("POST", fmt.Sprintf("/orders/%d/cancel", o.ID), user, nil, 200, "")
			f.stock(p.ID, 10)
			if f.number("SELECT current_uses FROM promo_codes WHERE id=$1", code.ID) != 0 {
				t.Fatal("promo use not returned")
			}
		})
	}
}

func TestIntegrationConcurrentUpdateCancel(t *testing.T) {
	f := integration(t, 0)
	_, seller := f.user("SELLER")
	_, user := f.user("USER")
	p := f.product(seller, 10)
	promo := f.promo(seller, 1, 0)
	o := decode[api.OrderResponse](t, f.request("POST", "/orders", user, orderBody(p.ID, 1, promo.Code), 201, ""))
	path := fmt.Sprintf("/orders/%d", o.ID)
	start := make(chan struct{})
	updated := make(chan *httptest.ResponseRecorder, 1)
	canceled := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		<-start
		updated <- call(f.h, "PUT", path, user, body(orderBody(p.ID, 3, "")))
	}()
	go func() {
		<-start
		canceled <- call(f.h, "POST", path+"/cancel", user, "")
	}()
	close(start)
	u, c := <-updated, <-canceled
	if u.Code == 200 {
		checkCode(t, u, 200, "")
	} else {
		checkCode(t, u, 409, "INVALID_STATE_TRANSITION")
	}
	checkCode(t, c, 200, "")
	f.stock(p.ID, 10)
	if f.number("SELECT current_uses FROM promo_codes WHERE id=$1", promo.ID) != 0 {
		t.Fatal("concurrent cancellation did not return promo exactly once")
	}
	o = decode[api.OrderResponse](t, f.request("GET", path, user, nil, 200, ""))
	if o.Status != "CANCELED" {
		t.Fatalf("final status %s, want CANCELED", o.Status)
	}
}
