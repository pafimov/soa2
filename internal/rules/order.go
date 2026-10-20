package rules

func CanCancel(s string) bool {
	return s == "CREATED" || s == "PAYMENT_PENDING"
}

func CanMove(from, to string) bool {
	if to == "CANCELED" {
		return CanCancel(from)
	}
	next := map[string]string{
		"CREATED":         "PAYMENT_PENDING",
		"PAYMENT_PENDING": "PAID",
		"PAID":            "SHIPPED",
		"SHIPPED":         "COMPLETED",
	}
	s, ok := next[from]
	return ok && s == to
}

func Discount(total, value int64, typ string) int64 {
	if typ == "FIXED_AMOUNT" {
		return min(total, value)
	}
	if typ != "PERCENTAGE" || value > 7000 {
		return 0
	}
	d := (total*value + 5000) / 10000
	return min(d, total*70/100)
}
