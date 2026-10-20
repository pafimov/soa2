package money

import (
	"encoding/json"
	"math"
	"testing"
)

func TestJSON(t *testing.T) {
	cases := []struct {
		in   string
		want Amount
		text string
	}{
		{"0", 0, "0.00"},
		{"0.01", 1, "0.01"},
		{"0.29", 29, "0.29"},
		{"1.2300", 123, "1.23"},
		{"-12.34", -1234, "-12.34"},
		{"1e2", 10000, "100.00"},
		{"1e-2", 1, "0.01"},
		{"1000000.01", 100000001, "1000000.01"},
		{"9999999999.99", Max, "9999999999.99"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var v Amount
			if err := json.Unmarshal([]byte(tc.in), &v); err != nil || v != tc.want {
				t.Fatalf("got %d, %v, want %d", v, err, tc.want)
			}
			b, err := json.Marshal(v)
			if err != nil || string(b) != tc.text {
				t.Fatalf("got %s, %v", b, err)
			}
		})
	}
}

func TestInvalidJSON(t *testing.T) {
	for _, v := range []string{`"1.25"`, "null", "true", "1.001", "1e-3", "NaN", "[]", "{}", "1/2", "1e999999999", "9223372036854775808", "1 2"} {
		var a Amount
		if err := json.Unmarshal([]byte(v), &a); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
}

func TestScanAndValue(t *testing.T) {
	for _, in := range []any{"12.34", []byte("12.34")} {
		var a Amount
		if err := a.Scan(in); err != nil || a != 1234 {
			t.Fatalf("scan: %d %v", a, err)
		}
		v, err := a.Value()
		if err != nil || v != "12.34" {
			t.Fatalf("value: %v %v", v, err)
		}
	}
	var a Amount
	if err := a.Scan(int64(12)); err != nil || a != 1200 {
		t.Fatalf("integer scan: %d %v", a, err)
	}
	if a.Scan(nil) == nil || a.Scan(12.34) == nil {
		t.Fatal("unexpected scan success")
	}
	if Amount(math.MinInt64).String() != "-92233720368547758.08" {
		t.Fatal("min int overflow")
	}
}
