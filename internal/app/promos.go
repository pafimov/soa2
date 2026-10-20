package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"marketplace/internal/api"
	"marketplace/internal/money"
	"marketplace/internal/rules"
)

const promoCols = "id,code,discount_type,discount_value,min_order_amount,max_uses,current_uses,valid_from,valid_until,active"

func scanPromo(row scanner) (api.PromoCodeResponse, error) {
	var p api.PromoCodeResponse
	err := row.Scan(&p.ID, &p.Code, &p.DiscountType, &p.DiscountValue, &p.MinOrderAmount, &p.MaxUses, &p.CurrentUses, &p.ValidFrom, &p.ValidUntil, &p.Active)
	return p, err
}

func (s *Server) CreatePromoCode(w http.ResponseWriter, r *http.Request) {
	b, err := readBody[api.PromoCodeCreate](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	if !b.ValidUntil.After(b.ValidFrom) {
		s.reply(w, r, 0, nil, invalid("valid_until", "Должно быть позже valid_from"))
		return
	}
	active := true
	if b.Active != nil {
		active = *b.Active
	}
	p, err := scanPromo(s.db.QueryRowContext(r.Context(), `INSERT INTO promo_codes
		(code,discount_type,discount_value,min_order_amount,max_uses,valid_from,valid_until,active)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+promoCols,
		b.Code, string(b.DiscountType), b.DiscountValue, b.MinOrderAmount, b.MaxUses, b.ValidFrom, b.ValidUntil, active))
	if isUnique(err) {
		err = problem(409, "PROMO_CODE_EXISTS", "Промокод уже существует")
	}
	s.reply(w, r, 201, p, err)
}

func checkPromo(ctx context.Context, tx *sql.Tx, p api.PromoCodeResponse, own bool) error {
	var now time.Time
	if err := tx.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	uses := p.CurrentUses
	if own {
		uses--
	}
	if !p.Active || uses < 0 || uses >= p.MaxUses || now.Before(p.ValidFrom) || now.After(p.ValidUntil) {
		return problem(422, "PROMO_CODE_INVALID", "Промокод сейчас недоступен")
	}
	return nil
}

func applyPromo(ctx context.Context, tx *sql.Tx, code string, total money.Amount) (*int64, money.Amount, error) {
	p, err := scanPromo(tx.QueryRowContext(ctx, "SELECT "+promoCols+" FROM promo_codes WHERE code=$1 FOR UPDATE", code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, problem(422, "PROMO_CODE_INVALID", "Промокод не найден")
	}
	if err != nil {
		return nil, 0, err
	}
	if err = checkPromo(ctx, tx, p, false); err != nil {
		return nil, 0, err
	}
	if total < p.MinOrderAmount {
		return nil, 0, problem(422, "PROMO_CODE_MIN_AMOUNT", "Минимальная сумма для промокода: "+p.MinOrderAmount.String())
	}
	d := money.Amount(rules.Discount(int64(total), int64(p.DiscountValue), string(p.DiscountType)))
	_, err = tx.ExecContext(ctx, "UPDATE promo_codes SET current_uses=current_uses+1 WHERE id=$1", p.ID)
	return &p.ID, d, err
}

func recalcPromo(ctx context.Context, tx *sql.Tx, id *int64, total money.Amount) (*int64, money.Amount, error) {
	if id == nil {
		return nil, 0, nil
	}
	p, err := scanPromo(tx.QueryRowContext(ctx, "SELECT "+promoCols+" FROM promo_codes WHERE id=$1 FOR UPDATE", *id))
	if err != nil {
		return nil, 0, err
	}
	if err = checkPromo(ctx, tx, p, true); err != nil {
		return nil, 0, err
	}
	if total < p.MinOrderAmount {
		_, err = tx.ExecContext(ctx, "UPDATE promo_codes SET current_uses=current_uses-1 WHERE id=$1", *id)
		return nil, 0, err
	}
	d := money.Amount(rules.Discount(int64(total), int64(p.DiscountValue), string(p.DiscountType)))
	return id, d, nil
}
