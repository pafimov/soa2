package app

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"marketplace/internal/api"
	"marketplace/internal/money"
	"marketplace/internal/rules"
)

const orderCols = "id,user_id,status,promo_code_id,total_amount,discount_amount,created_at,updated_at"

func scanOrder(row scanner) (api.OrderResponse, error) {
	o := api.OrderResponse{Items: []api.OrderItemResponse{}}
	err := row.Scan(&o.ID, &o.UserID, &o.Status, &o.PromoCodeID, &o.TotalAmount, &o.DiscountAmount, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return o, problem(404, "ORDER_NOT_FOUND", "Заказ не найден")
	}
	return o, err
}

func loadOrder(ctx context.Context, tx *sql.Tx, id int64, lock bool) (api.OrderResponse, error) {
	q := "SELECT " + orderCols + " FROM orders WHERE id=$1"
	if lock {
		q += " FOR UPDATE"
	}
	o, err := scanOrder(tx.QueryRowContext(ctx, q, id))
	if err != nil {
		return o, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,product_id,quantity,price_at_order FROM order_items WHERE order_id=$1 ORDER BY id", id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	for rows.Next() {
		var v api.OrderItemResponse
		if err = rows.Scan(&v.ID, &v.ProductID, &v.Quantity, &v.PriceAtOrder); err != nil {
			return o, err
		}
		o.Items = append(o.Items, v)
	}
	return o, rows.Err()
}

func orderAccess(a actor, o api.OrderResponse) error {
	if a.Role != "ADMIN" && a.ID != o.UserID {
		return problem(403, "ORDER_OWNERSHIP_VIOLATION", "Заказ принадлежит другому пользователю")
	}
	return nil
}

func lockUser(ctx context.Context, tx *sql.Tx, id int64) error {
	var v int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE", id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return problem(401, "TOKEN_INVALID", "Пользователь не найден")
	}
	return err
}

func (s *Server) checkRate(ctx context.Context, tx *sql.Tx, uid int64, op string) error {
	var last, now time.Time
	err := tx.QueryRowContext(ctx, `SELECT created_at,clock_timestamp() FROM user_operations
		WHERE user_id=$1 AND operation_type=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, uid, op).Scan(&last, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	wait := s.cfg.OrderLimit - now.Sub(last)
	if wait > 0 {
		sec := int64(math.Ceil(wait.Seconds()))
		e := problem(429, "ORDER_LIMIT_EXCEEDED", "Слишком частые операции с заказом")
		e.body.Details = &api.ErrorDetails{RetryAfterSeconds: &sec}
		return e
	}
	return nil
}

func productIDs(items []api.OrderItemInput, old []api.OrderItemResponse) []int64 {
	m := map[int64]bool{}
	for _, v := range items {
		m[v.ProductID] = true
	}
	for _, v := range old {
		m[v.ProductID] = true
	}
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func lockProducts(ctx context.Context, tx *sql.Tx, ids []int64) (map[int64]api.ProductResponse, error) {
	m := make(map[int64]api.ProductResponse, len(ids))
	for _, id := range ids {
		p, err := scanProduct(tx.QueryRowContext(ctx, "SELECT "+productCols+" FROM products WHERE id=$1 FOR UPDATE", id))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m[id] = p
	}
	return m, nil
}

func prepareItems(items []api.OrderItemInput, products map[int64]api.ProductResponse, old []api.OrderItemResponse) ([]api.OrderItemResponse, money.Amount, error) {
	for _, v := range items {
		p, ok := products[v.ProductID]
		if !ok {
			return nil, 0, problem(404, "PRODUCT_NOT_FOUND", "Товар не найден: "+strconv.FormatInt(v.ProductID, 10))
		}
		if p.Status != "ACTIVE" {
			return nil, 0, problem(409, "PRODUCT_INACTIVE", "Товар неактивен: "+strconv.FormatInt(v.ProductID, 10))
		}
	}
	short := []api.StockShortage{}
	for _, v := range items {
		p := products[v.ProductID]
		if p.Stock < v.Quantity {
			short = append(short, api.StockShortage{ProductID: p.ID, Requested: v.Quantity, Available: p.Stock})
		}
	}
	if len(short) != 0 {
		e := problem(409, "INSUFFICIENT_STOCK", "Недостаточно товаров на складе")
		e.body.Details = &api.ErrorDetails{Products: short}
		return nil, 0, e
	}
	prices := map[int64]money.Amount{}
	for _, v := range old {
		prices[v.ProductID] = v.PriceAtOrder
	}
	out := make([]api.OrderItemResponse, 0, len(items))
	var total money.Amount
	for _, v := range items {
		price, ok := prices[v.ProductID]
		if !ok {
			price = products[v.ProductID].Price
		}
		total += price * money.Amount(v.Quantity)
		out = append(out, api.OrderItemResponse{ProductID: v.ProductID, Quantity: v.Quantity, PriceAtOrder: price})
	}
	if total > money.Max {
		return nil, 0, problem(422, "ORDER_AMOUNT_EXCEEDED", "Сумма заказа превышает 9999999999.99")
	}
	return out, total, nil
}

func reserve(ctx context.Context, tx *sql.Tx, items []api.OrderItemResponse) error {
	for _, v := range items {
		if _, err := tx.ExecContext(ctx, "UPDATE products SET stock=stock-$2 WHERE id=$1", v.ProductID, v.Quantity); err != nil {
			return err
		}
	}
	return nil
}

func restore(ctx context.Context, tx *sql.Tx, items []api.OrderItemResponse, products map[int64]api.ProductResponse) error {
	for _, v := range items {
		if _, err := tx.ExecContext(ctx, "UPDATE products SET stock=stock+$2 WHERE id=$1", v.ProductID, v.Quantity); err != nil {
			return err
		}
		p := products[v.ProductID]
		p.Stock += v.Quantity
		products[v.ProductID] = p
	}
	return nil
}

func insertItems(ctx context.Context, tx *sql.Tx, id int64, items []api.OrderItemResponse) error {
	for i := range items {
		v := &items[i]
		if err := tx.QueryRowContext(ctx, `INSERT INTO order_items(order_id,product_id,quantity,price_at_order)
			VALUES($1,$2,$3,$4) RETURNING id`, id, v.ProductID, v.Quantity, v.PriceAtOrder).Scan(&v.ID); err != nil {
			return err
		}
	}
	return nil
}

func recordOperation(ctx context.Context, tx *sql.Tx, uid int64, op string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO user_operations(user_id,operation_type) VALUES($1,$2)", uid, op)
	return err
}

func (s *Server) createOrder(ctx context.Context, a actor, b api.OrderCreate) (api.OrderResponse, error) {
	var out api.OrderResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := lockUser(ctx, tx, a.ID); err != nil {
			return err
		}
		if err := s.checkRate(ctx, tx, a.ID, "CREATE_ORDER"); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM orders WHERE user_id=$1 AND status IN ('CREATED','PAYMENT_PENDING'))", a.ID).Scan(&active); err != nil {
			return err
		}
		if active {
			return problem(409, "ORDER_HAS_ACTIVE", "У пользователя уже есть активный заказ")
		}
		products, err := lockProducts(ctx, tx, productIDs(b.Items, nil))
		if err != nil {
			return err
		}
		items, total, err := prepareItems(b.Items, products, nil)
		if err != nil {
			return err
		}
		if err = reserve(ctx, tx, items); err != nil {
			return err
		}
		var promoID *int64
		var discount money.Amount
		if b.PromoCode != nil {
			promoID, discount, err = applyPromo(ctx, tx, *b.PromoCode, total)
			if err != nil {
				return err
			}
		}
		out, err = scanOrder(tx.QueryRowContext(ctx, `INSERT INTO orders(user_id,status,promo_code_id,total_amount,discount_amount)
			VALUES($1,'CREATED',$2,$3,$4) RETURNING `+orderCols, a.ID, promoID, total-discount, discount))
		if err != nil {
			return err
		}
		if err = insertItems(ctx, tx, out.ID, items); err != nil {
			return err
		}
		out.Items = items
		return recordOperation(ctx, tx, a.ID, "CREATE_ORDER")
	})
	return out, err
}

func (s *Server) updateOrder(ctx context.Context, a actor, id int64, b api.OrderUpdate) (api.OrderResponse, error) {
	var out api.OrderResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := lockUser(ctx, tx, a.ID); err != nil {
			return err
		}
		o, err := loadOrder(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err = orderAccess(a, o); err != nil {
			return err
		}
		if o.Status != "CREATED" {
			return problem(409, "INVALID_STATE_TRANSITION", "Менять позиции можно только в CREATED")
		}
		if err = s.checkRate(ctx, tx, a.ID, "UPDATE_ORDER"); err != nil {
			return err
		}
		products, err := lockProducts(ctx, tx, productIDs(b.Items, o.Items))
		if err != nil {
			return err
		}
		if err = restore(ctx, tx, o.Items, products); err != nil {
			return err
		}
		items, total, err := prepareItems(b.Items, products, o.Items)
		if err != nil {
			return err
		}
		if err = reserve(ctx, tx, items); err != nil {
			return err
		}
		promoID, discount, err := recalcPromo(ctx, tx, o.PromoCodeID, total)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM order_items WHERE order_id=$1", id); err != nil {
			return err
		}
		if err = insertItems(ctx, tx, id, items); err != nil {
			return err
		}
		out, err = scanOrder(tx.QueryRowContext(ctx, `UPDATE orders SET promo_code_id=$2,total_amount=$3,discount_amount=$4
			WHERE id=$1 RETURNING `+orderCols, id, promoID, total-discount, discount))
		if err != nil {
			return err
		}
		out.Items = items
		return recordOperation(ctx, tx, a.ID, "UPDATE_ORDER")
	})
	return out, err
}

func (s *Server) cancelOrder(ctx context.Context, a actor, id int64) (api.OrderResponse, error) {
	var out api.OrderResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		o, err := loadOrder(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err = orderAccess(a, o); err != nil {
			return err
		}
		if !rules.CanCancel(string(o.Status)) {
			return problem(409, "INVALID_STATE_TRANSITION", "Этот заказ уже нельзя отменить")
		}
		products, err := lockProducts(ctx, tx, productIDs(nil, o.Items))
		if err != nil {
			return err
		}
		if err = restore(ctx, tx, o.Items, products); err != nil {
			return err
		}
		if o.PromoCodeID != nil {
			if _, err = tx.ExecContext(ctx, "UPDATE promo_codes SET current_uses=current_uses-1 WHERE id=$1", *o.PromoCodeID); err != nil {
				return err
			}
		}
		out, err = scanOrder(tx.QueryRowContext(ctx, "UPDATE orders SET status='CANCELED' WHERE id=$1 RETURNING "+orderCols, id))
		out.Items = o.Items
		return err
	})
	return out, err
}

func (s *Server) CreateOrder(w http.ResponseWriter, r *http.Request) {
	b, err := readBody[api.OrderCreate](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	o, err := s.createOrder(r.Context(), current(r), b)
	if err == nil {
		w.Header().Set("Location", "/orders/"+strconv.FormatInt(o.ID, 10))
	}
	s.reply(w, r, 201, o, err)
}

func (s *Server) UpdateOrder(w http.ResponseWriter, r *http.Request, id int64) {
	b, err := readBody[api.OrderUpdate](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	o, err := s.updateOrder(r.Context(), current(r), id, b)
	s.reply(w, r, 200, o, err)
}

func (s *Server) CancelOrder(w http.ResponseWriter, r *http.Request, id int64) {
	o, err := s.cancelOrder(r.Context(), current(r), id)
	s.reply(w, r, 200, o, err)
}

func (s *Server) GetOrder(w http.ResponseWriter, r *http.Request, id int64) {
	tx, err := s.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	defer tx.Rollback()
	o, err := loadOrder(r.Context(), tx, id, false)
	if err == nil {
		err = orderAccess(current(r), o)
	}
	if err == nil {
		err = tx.Commit()
	}
	s.reply(w, r, 200, o, err)
}

func (s *Server) ChangeOrderStatus(w http.ResponseWriter, r *http.Request, id int64) {
	b, err := readBody[api.OrderStatusUpdate](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	var out api.OrderResponse
	err = s.tx(r.Context(), func(tx *sql.Tx) error {
		o, err := loadOrder(r.Context(), tx, id, true)
		if err != nil {
			return err
		}
		if !rules.CanMove(string(o.Status), string(b.Status)) {
			return problem(409, "INVALID_STATE_TRANSITION", "Недопустимый переход состояния")
		}
		out, err = scanOrder(tx.QueryRowContext(r.Context(), "UPDATE orders SET status=$2 WHERE id=$1 RETURNING "+orderCols, id, string(b.Status)))
		out.Items = o.Items
		return err
	})
	s.reply(w, r, 200, out, err)
}
