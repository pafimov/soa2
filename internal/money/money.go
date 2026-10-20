package money

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

type Amount int64

const Max Amount = 999999999999

func (a Amount) String() string {
	x := int64(a)
	sign := ""
	if x < 0 {
		sign = "-"
	}
	n := uint64(x)
	if x < 0 {
		n = uint64(-(x + 1)) + 1
	}
	return fmt.Sprintf("%s%d.%02d", sign, n/100, n%100)
}

func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(a.String()), nil
}

func (a *Amount) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || len(b) > 128 || b[0] == '"' || !json.Valid(b) {
		return errors.New("expected decimal number")
	}
	s := string(b)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e < -1000 || e > 1000 {
			return errors.New("invalid decimal exponent")
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return errors.New("expected decimal number")
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return errors.New("expected at most two decimal places")
	}
	*a = Amount(r.Num().Int64())
	return nil
}

func (a *Amount) Scan(v any) error {
	switch x := v.(type) {
	case string:
		return a.UnmarshalJSON([]byte(x))
	case []byte:
		return a.UnmarshalJSON(x)
	case int64:
		return a.UnmarshalJSON([]byte(strconv.FormatInt(x, 10)))
	default:
		return fmt.Errorf("cannot scan money from %T", v)
	}
}

func (a Amount) Value() (driver.Value, error) {
	return a.String(), nil
}
