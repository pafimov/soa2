package rules

import "testing"

func TestDiscount(t *testing.T) {
	cases := []struct {
		total, value int64
		typ          string
		want         int64
	}{
		{10000, 1000, "PERCENTAGE", 1000},
		{10000, 7000, "PERCENTAGE", 7000},
		{10000, 7001, "PERCENTAGE", 0},
		{10000, 9900, "PERCENTAGE", 0},
		{10000, 999999999999, "PERCENTAGE", 0},
		{105, 1000, "PERCENTAGE", 11},
		{1, 7000, "PERCENTAGE", 0},
		{10000, 1000, "FIXED_AMOUNT", 1000},
		{10000, 20000, "FIXED_AMOUNT", 10000},
		{0, 20000, "FIXED_AMOUNT", 0},
	}
	for _, tc := range cases {
		if v := Discount(tc.total, tc.value, tc.typ); v != tc.want {
			t.Errorf("%+v: got %d", tc, v)
		}
	}
}

func TestStates(t *testing.T) {
	states := []string{"CREATED", "PAYMENT_PENDING", "PAID", "SHIPPED", "COMPLETED", "CANCELED"}
	valid := map[[2]string]bool{
		{"CREATED", "PAYMENT_PENDING"}:  true,
		{"CREATED", "CANCELED"}:         true,
		{"PAYMENT_PENDING", "PAID"}:     true,
		{"PAYMENT_PENDING", "CANCELED"}: true,
		{"PAID", "SHIPPED"}:             true,
		{"SHIPPED", "COMPLETED"}:        true,
	}
	for _, a := range states {
		for _, b := range states {
			if CanMove(a, b) != valid[[2]string{a, b}] {
				t.Errorf("%s -> %s", a, b)
			}
		}
	}
	if CanMove("nope", "") || CanCancel("COMPLETED") {
		t.Fatal("unexpected transition")
	}
}
