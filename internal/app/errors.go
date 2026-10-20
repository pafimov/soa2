package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"

	"marketplace/internal/api"
)

type httpError struct {
	status int
	body   api.ErrorResponse
}

func (e *httpError) Error() string {
	return e.body.Message
}

func problem(status int, code, msg string) *httpError {
	return &httpError{status: status, body: api.ErrorResponse{ErrorCode: api.ErrorCode(code), Message: msg}}
}

func invalid(field, msg string) *httpError {
	return invalidFields([]api.FieldViolation{{Field: field, Message: msg}})
}

func invalidFields(fields []api.FieldViolation) *httpError {
	e := problem(400, "VALIDATION_ERROR", "Проверьте параметры запроса")
	e.body.Details = &api.ErrorDetails{Fields: fields}
	return e
}

func isUnique(err error) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && e.Code == "23505"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err == nil {
		writeJSON(w, status, v)
		return
	}
	var e *httpError
	if !errors.As(err, &e) {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.ConstraintName == "orders_one_active_idx" {
			e = problem(409, "ORDER_HAS_ACTIVE", "У пользователя уже есть активный заказ")
		} else if errors.As(err, &pg) && pg.ConstraintName == "order_state_transition" {
			e = problem(409, "INVALID_STATE_TRANSITION", "Недопустимый переход состояния")
		} else {
			s.log.ErrorContext(r.Context(), "request failed", "request_id", w.Header().Get("X-Request-Id"), "error", err.Error())
			e = problem(500, "INTERNAL_ERROR", "Внутренняя ошибка сервера")
		}
	}
	if e.status == 401 {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	if e.body.Details != nil && e.body.Details.RetryAfterSeconds != nil {
		w.Header().Set("Retry-After", strconv.FormatInt(*e.body.Details.RetryAfterSeconds, 10))
	}
	writeJSON(w, e.status, e.body)
}
