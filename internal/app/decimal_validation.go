package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/getkin/kin-openapi/openapi3"

	"marketplace/internal/api"
	"marketplace/internal/money"
)

func decimalRules(doc *openapi3.T) (map[*openapi3.Schema]money.Amount, error) {
	steps := map[*openapi3.Schema]money.Amount{}
	seen := map[*openapi3.Schema]bool{}
	var walk func(*openapi3.Schema) error
	walk = func(sc *openapi3.Schema) error {
		if sc == nil || seen[sc] {
			return nil
		}
		seen[sc] = true
		if sc.Format == "decimal" && sc.MultipleOf != nil {
			var a money.Amount
			if a.UnmarshalJSON([]byte(strconv.FormatFloat(*sc.MultipleOf, 'f', -1, 64))) != nil || a <= 0 {
				return errors.New("decimal step must be a positive number of cents")
			}
			steps[sc] = a
			sc.MultipleOf = nil
		}
		for _, p := range sc.Properties {
			if err := walk(p.Value); err != nil {
				return err
			}
		}
		if sc.Items != nil {
			return walk(sc.Items.Value)
		}
		return nil
	}
	for _, sc := range doc.Components.Schemas {
		if err := walk(sc.Value); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

func (s *Server) decimalFields(v any, sc *openapi3.Schema, path string) []api.FieldViolation {
	var out []api.FieldViolation
	if sc == nil {
		return out
	}
	switch value := v.(type) {
	case map[string]any:
		for k, child := range value {
			if prop := sc.Properties[k]; prop != nil {
				out = append(out, s.decimalFields(child, prop.Value, path+"."+k)...)
			}
		}
	case []any:
		if sc.Items != nil {
			for i, child := range value {
				out = append(out, s.decimalFields(child, sc.Items.Value, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	case json.Number:
		if sc.Format != "decimal" {
			break
		}
		var a money.Amount
		if err := a.UnmarshalJSON([]byte(value)); err != nil {
			return []api.FieldViolation{{Field: path, Message: "Ожидается денежное число с точностью до копейки"}}
		}
		if step := s.decimalSteps[sc]; step > 0 && a%step != 0 {
			out = append(out, api.FieldViolation{Field: path, Message: "Число должно быть кратно " + step.String()})
		}
		for _, bound := range []struct {
			value   *float64
			minimum bool
		}{{sc.Min, true}, {sc.Max, false}} {
			if bound.value == nil {
				continue
			}
			var n money.Amount
			if n.UnmarshalJSON([]byte(strconv.FormatFloat(*bound.value, 'f', -1, 64))) != nil {
				continue
			}
			if bound.minimum && a < n || !bound.minimum && a > n {
				out = append(out, api.FieldViolation{Field: path, Message: "Число вне диапазона OpenAPI"})
			}
		}
	}
	return out
}
