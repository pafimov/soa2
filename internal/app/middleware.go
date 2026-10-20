package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"

	"marketplace/internal/api"
)

type ctxKey int

const (
	actorKey ctxKey = iota
	logKey
)

type actor struct {
	ID   int64
	Role string
}

type requestLog struct {
	userID *int64
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}

func current(r *http.Request) actor {
	return r.Context().Value(actorKey).(actor)
}

func mask(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			key := strings.ToLower(k)
			if strings.Contains(key, "password") || strings.Contains(key, "token") || key == "authorization" || strings.Contains(key, "secret") {
				x[k] = "***"
			} else {
				x[k] = mask(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = mask(x[i])
		}
	}
	return v
}

func bodyForLog(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if !json.Valid(b) || d.Decode(&v) != nil {
		return "[invalid JSON omitted]"
	}
	return mask(v)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now().UTC()
		id := newID()
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("Cache-Control", "no-store")
		ww := &responseWriter{ResponseWriter: w}
		data := &requestLog{}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(context.WithValue(ctx, logKey, data))
		var body any
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "request_id", id, "type", fmt.Sprintf("%T", v))
				if ww.status == 0 {
					s.reply(ww, r, 0, nil, problem(500, "INTERNAL_ERROR", "Внутренняя ошибка сервера"))
				}
			}
			status := ww.status
			if status == 0 {
				status = 200
			}
			args := []any{
				"request_id", id, "method", r.Method, "endpoint", r.URL.Path,
				"status_code", status, "duration_ms", float64(time.Since(start).Microseconds()) / 1000,
				"user_id", data.userID, "timestamp", start.Format(time.RFC3339Nano),
			}
			if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
				args = append(args, "request_body", body)
			}
			s.log.Info("api request", args...)
		}()
		b, err := io.ReadAll(http.MaxBytesReader(ww, r.Body, 1<<20))
		if err != nil {
			body = "[unreadable or oversized body omitted]"
			s.reply(ww, r, 0, nil, invalid("body", "Не удалось прочитать тело, максимум 1 MiB"))
			return
		}
		body = bodyForLog(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		next.ServeHTTP(ww, r)
	})
}

func (s *Server) validate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, params, err := s.router.FindRoute(r)
		if err != nil {
			var allowed []string
			for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "HEAD"} {
				q := r.Clone(r.Context())
				q.Method = method
				if _, _, e := s.router.FindRoute(q); e == nil {
					allowed = append(allowed, method)
				}
			}
			if len(allowed) > 0 {
				w.Header().Set("Allow", strings.Join(allowed, ", "))
				s.reply(w, r, 0, nil, problem(405, "METHOD_NOT_ALLOWED", "Метод не поддерживается"))
			} else {
				s.reply(w, r, 0, nil, problem(404, "ROUTE_NOT_FOUND", "Путь не найден"))
			}
			return
		}
		security := s.spec.Security
		if route.Operation.Security != nil {
			security = *route.Operation.Security
		}
		if len(security) != 0 {
			a, err := s.authenticate(r.Header.Get("Authorization"))
			if err != nil {
				s.reply(w, r, 0, nil, err)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), actorKey, a))
			if data, ok := r.Context().Value(logKey).(*requestLog); ok {
				data.userID = &a.ID
			}
			if !slices.Contains(s.roles[route.Operation.OperationID], a.Role) {
				s.reply(w, r, 0, nil, problem(403, "ACCESS_DENIED", "Недостаточно прав"))
				return
			}
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			s.reply(w, r, 0, nil, invalid("body", "Не удалось прочитать тело"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		if len(b) != 0 && !json.Valid(b) {
			s.reply(w, r, 0, nil, invalid("body", "Ожидается один корректный JSON-документ"))
			return
		}
		input := &openapi3filter.RequestValidationInput{
			Request: r, PathParams: params, Route: route,
			Options: &openapi3filter.Options{
				MultiError: true, SkipSettingDefaults: true,
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
		}
		var fields []api.FieldViolation
		if route.Operation.RequestBody != nil {
			var value any
			d := json.NewDecoder(bytes.NewReader(b))
			d.UseNumber()
			if d.Decode(&value) == nil {
				media := route.Operation.RequestBody.Value.Content["application/json"]
				fields = s.decimalFields(value, media.Schema.Value, "body")
			}
		}
		if err = openapi3filter.ValidateRequest(r.Context(), input); err != nil {
			fields = append(fields, validationFields(err, "body")...)
		}
		if len(fields) != 0 {
			s.reply(w, r, 0, nil, invalidFields(fields))
			return
		}
		if route.Operation.RequestBody == nil && len(b) != 0 {
			s.reply(w, r, 0, nil, invalid("body", "У этого метода нет тела запроса"))
			return
		}
		if v, ok := route.Operation.Extensions["x-unique-product-ids"]; ok && fmt.Sprint(v) == "true" {
			var q api.OrderUpdate
			if json.Unmarshal(b, &q) != nil {
				s.reply(w, r, 0, nil, invalid("items", "Неверный формат позиций"))
				return
			}
			seen := map[int64]bool{}
			for i, item := range q.Items {
				if seen[item.ProductID] {
					s.reply(w, r, 0, nil, invalid(fmt.Sprintf("items[%d].product_id", i), "Товар уже указан в заказе"))
					return
				}
				seen[item.ProductID] = true
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		next.ServeHTTP(w, r)
	})
}

func validationFields(err error, prefix string) []api.FieldViolation {
	var out []api.FieldViolation
	switch e := err.(type) {
	case openapi3.MultiError:
		for _, child := range e {
			out = append(out, validationFields(child, prefix)...)
		}
	case *openapi3filter.RequestError:
		if e.Parameter != nil {
			prefix = e.Parameter.In + "." + e.Parameter.Name
		}
		if e.Err != nil {
			out = append(out, validationFields(e.Err, prefix)...)
		} else {
			out = append(out, api.FieldViolation{Field: prefix, Message: "Обязательное значение отсутствует или имеет неверный формат"})
		}
	case *openapi3.SchemaError:
		field := prefix
		for _, part := range e.JSONPointer() {
			if _, err := strconv.Atoi(part); err == nil {
				field += "[" + part + "]"
			} else {
				field += "." + part
			}
		}
		if e.SchemaField == "required" && e.Schema != nil {
			if v, ok := e.Value.(map[string]any); ok {
				for _, name := range e.Schema.Required {
					if _, exists := v[name]; !exists {
						out = append(out, api.FieldViolation{Field: field + "." + name, Message: "Обязательное поле"})
					}
				}
			}
		}
		if len(out) == 0 {
			out = append(out, api.FieldViolation{Field: field, Message: e.Reason})
		}
	default:
		out = append(out, api.FieldViolation{Field: prefix, Message: "Значение не соответствует типу или формату из OpenAPI"})
	}
	return out
}
