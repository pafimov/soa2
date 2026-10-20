package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"marketplace/internal/api"
)

const productCols = "id,name,description,price,stock,category,status,seller_id,created_at,updated_at"

func scanProduct(row scanner) (api.ProductResponse, error) {
	var p api.ProductResponse
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.Category, &p.Status, &p.SellerID, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func productAccess(a actor, p api.ProductResponse) error {
	if a.Role != "ADMIN" && p.SellerID != a.ID {
		return problem(403, "ACCESS_DENIED", "Нельзя менять чужой товар")
	}
	return nil
}

func (s *Server) GetProduct(w http.ResponseWriter, r *http.Request, id int64) {
	p, err := scanProduct(s.db.QueryRowContext(r.Context(), "SELECT "+productCols+" FROM products WHERE id=$1", id))
	if errors.Is(err, sql.ErrNoRows) {
		err = problem(404, "PRODUCT_NOT_FOUND", "Товар не найден")
	}
	s.reply(w, r, 200, p, err)
}

func (s *Server) ListProducts(w http.ResponseWriter, r *http.Request, q api.ListProductsParams) {
	page, size := int64(0), int64(20)
	if q.Page != nil {
		page = *q.Page
	}
	if q.Size != nil {
		size = *q.Size
	}
	var status, category any
	if q.Status != nil {
		status = string(*q.Status)
	}
	if q.Category != nil {
		category = *q.Category
	}
	out := api.ProductPage{Content: []api.ProductResponse{}, Page: page, Size: size}
	tx, err := s.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	defer tx.Rollback()
	where := " FROM products WHERE ($1::text IS NULL OR status::text=$1) AND ($2::text IS NULL OR category=$2)"
	err = tx.QueryRowContext(r.Context(), "SELECT count(*)"+where, status, category).Scan(&out.TotalElements)
	if err == nil {
		var rows *sql.Rows
		rows, err = tx.QueryContext(r.Context(), "SELECT "+productCols+where+" ORDER BY created_at DESC,id DESC LIMIT $3 OFFSET $4", status, category, size, page*size)
		if err == nil {
			for rows.Next() {
				p, e := scanProduct(rows)
				if e != nil {
					err = e
					break
				}
				out.Content = append(out.Content, p)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	s.reply(w, r, 200, out, err)
}

func (s *Server) CreateProduct(w http.ResponseWriter, r *http.Request) {
	b, err := readBody[api.ProductCreate](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	a := current(r)
	owner := a.ID
	if b.SellerID != nil {
		owner = *b.SellerID
	}
	if a.Role != "ADMIN" && owner != a.ID {
		s.reply(w, r, 0, nil, problem(403, "ACCESS_DENIED", "Нельзя создавать чужой товар"))
		return
	}
	var p api.ProductResponse
	err = s.tx(r.Context(), func(tx *sql.Tx) error {
		var role string
		err := tx.QueryRowContext(r.Context(), "SELECT role FROM users WHERE id=$1 FOR KEY SHARE", owner).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && role != "SELLER" && role != "ADMIN") {
			return invalid("seller_id", "Нужен существующий SELLER или ADMIN")
		}
		if err != nil {
			return err
		}
		p, err = scanProduct(tx.QueryRowContext(r.Context(), `INSERT INTO products(name,description,price,stock,category,status,seller_id)
			VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+productCols, b.Name, b.Description, b.Price, b.Stock, b.Category, string(b.Status), owner))
		return err
	})
	if err == nil {
		w.Header().Set("Location", "/products/"+strconv.FormatInt(p.ID, 10))
	}
	s.reply(w, r, 201, p, err)
}

func lockedProduct(ctx context.Context, tx *sql.Tx, id int64) (api.ProductResponse, error) {
	p, err := scanProduct(tx.QueryRowContext(ctx, "SELECT "+productCols+" FROM products WHERE id=$1 FOR UPDATE", id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, problem(404, "PRODUCT_NOT_FOUND", "Товар не найден")
	}
	return p, err
}

func (s *Server) UpdateProduct(w http.ResponseWriter, r *http.Request, id int64) {
	b, err := readBody[api.ProductUpdate](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	var out api.ProductResponse
	err = s.tx(r.Context(), func(tx *sql.Tx) error {
		p, err := lockedProduct(r.Context(), tx, id)
		if err != nil {
			return err
		}
		if err = productAccess(current(r), p); err != nil {
			return err
		}
		out, err = scanProduct(tx.QueryRowContext(r.Context(), `UPDATE products SET
			name=$2,description=$3,price=$4,stock=$5,category=$6,status=$7 WHERE id=$1 RETURNING `+productCols,
			id, b.Name, b.Description, b.Price, b.Stock, b.Category, string(b.Status)))
		return err
	})
	s.reply(w, r, 200, out, err)
}

func (s *Server) DeleteProduct(w http.ResponseWriter, r *http.Request, id int64) {
	err := s.tx(r.Context(), func(tx *sql.Tx) error {
		p, err := lockedProduct(r.Context(), tx, id)
		if err != nil {
			return err
		}
		if err = productAccess(current(r), p); err != nil {
			return err
		}
		_, err = tx.ExecContext(r.Context(), "UPDATE products SET status='ARCHIVED' WHERE id=$1 AND status<>'ARCHIVED'", id)
		return err
	})
	s.reply(w, r, 204, nil, err)
}
