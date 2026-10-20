package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"marketplace/internal/api"
	"marketplace/internal/money"
)

type Server struct {
	db           *sql.DB
	cfg          Config
	log          *slog.Logger
	spec         *openapi3.T
	router       routers.Router
	roles        map[string][]string
	decimalSteps map[*openapi3.Schema]money.Amount
}

var _ api.ServerInterface = (*Server)(nil)

func New(db *sql.DB, c Config, log *slog.Logger) (*Server, error) {
	doc, err := api.GetSwagger()
	if err != nil {
		return nil, err
	}
	doc.Servers = nil
	steps, err := decimalRules(doc)
	if err != nil {
		return nil, err
	}
	rt, err := legacy.NewRouter(doc)
	if err != nil {
		return nil, err
	}
	s := &Server{db: db, cfg: c, log: log, spec: doc, router: rt, roles: map[string][]string{}, decimalSteps: steps}
	for _, path := range doc.Paths.Map() {
		for _, op := range path.Operations() {
			if v, ok := op.Extensions["x-roles"]; ok {
				b, err := json.Marshal(v)
				if err != nil {
					return nil, err
				}
				var roles []string
				if err = json.Unmarshal(b, &roles); err != nil {
					return nil, err
				}
				s.roles[op.OperationID] = roles
			}
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	h := api.HandlerWithOptions(s, api.StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			s.reply(w, r, 0, nil, invalid("parameters", "Неверный формат параметра"))
		},
	})
	return s.logging(s.validate(h))
}

func (s *Server) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func readBody[T any](r *http.Request) (T, error) {
	var b T
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		return b, invalid("body", "Неверный формат JSON")
	}
	return b, nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type scanner interface {
	Scan(...any) error
}
