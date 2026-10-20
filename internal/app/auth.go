package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"marketplace/internal/api"
)

var tokenIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const accessTTL = 15 * time.Minute
const refreshTTL = 14 * 24 * time.Hour

func (s *Server) sign(u api.UserResponse, kind, id string, until time.Time) (string, error) {
	key := s.cfg.AccessKey
	if kind == "refresh" {
		key = s.cfg.RefreshKey
	}
	c := jwt.MapClaims{
		"sub": strconv.FormatInt(u.ID, 10), "role": string(u.Role), "type": kind,
		"jti": id, "iss": "marketplace", "aud": kind,
		"iat": time.Now().Unix(), "exp": until.Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(key))
}

func (s *Server) parse(raw, kind string) (jwt.MapClaims, error) {
	key := s.cfg.AccessKey
	if kind == "refresh" {
		key = s.cfg.RefreshKey
	}
	token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		return []byte(key), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("marketplace"),
		jwt.WithAudience(kind), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		return nil, err
	}
	c, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid || c["type"] != kind {
		return nil, errors.New("invalid token type")
	}
	return c, nil
}

func (s *Server) authenticate(header string) (actor, error) {
	var a actor
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return a, problem(401, "TOKEN_INVALID", "Нужен access token")
	}
	c, err := s.parse(parts[1], "access")
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return a, problem(401, "TOKEN_EXPIRED", "Access token истёк")
		}
		return a, problem(401, "TOKEN_INVALID", "Неверный access token")
	}
	sub, err := c.GetSubject()
	if err != nil {
		return a, problem(401, "TOKEN_INVALID", "Неверный access token")
	}
	a.ID, err = strconv.ParseInt(sub, 10, 64)
	a.Role, _ = c["role"].(string)
	if err != nil || a.ID <= 0 || (a.Role != "USER" && a.Role != "SELLER" && a.Role != "ADMIN") {
		return actor{}, problem(401, "TOKEN_INVALID", "Неверный access token")
	}
	return a, nil
}

func (s *Server) tokens(ctx context.Context, tx *sql.Tx, u api.UserResponse) (api.AuthTokens, error) {
	var out api.AuthTokens
	id := newID()
	until := time.Now().Add(refreshTTL)
	access, err := s.sign(u, "access", newID(), time.Now().Add(accessTTL))
	if err != nil {
		return out, err
	}
	refresh, err := s.sign(u, "refresh", id, until)
	if err != nil {
		return out, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO refresh_tokens(id,user_id,expires_at) VALUES($1,$2,$3)", id, u.ID, until)
	if err != nil {
		return out, err
	}
	out = api.AuthTokens{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int64(accessTTL.Seconds())}
	return out, nil
}

func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	b, err := readBody[api.RegisterRequest](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	role := "USER"
	if b.Role != nil {
		role = string(*b.Role)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(b.Password), bcrypt.DefaultCost)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	var out api.UserResponse
	err = s.db.QueryRowContext(r.Context(), `INSERT INTO users(email,password_hash,role)
		VALUES($1,$2,$3) RETURNING id,email,role`, strings.ToLower(b.Email), string(hash), role).
		Scan(&out.ID, &out.Email, &out.Role)
	if isUnique(err) {
		err = problem(409, "EMAIL_ALREADY_EXISTS", "Этот email уже зарегистрирован")
	}
	s.reply(w, r, 201, out, err)
}

func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	b, err := readBody[api.LoginRequest](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	var u api.UserResponse
	var hash string
	err = s.db.QueryRowContext(r.Context(), "SELECT id,email,role,password_hash FROM users WHERE email=$1", strings.ToLower(b.Email)).
		Scan(&u.ID, &u.Email, &u.Role, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		err = problem(401, "INVALID_CREDENTIALS", "Неверный email или пароль")
	}
	if err == nil && bcrypt.CompareHashAndPassword([]byte(hash), []byte(b.Password)) != nil {
		err = problem(401, "INVALID_CREDENTIALS", "Неверный email или пароль")
	}
	var out api.AuthTokens
	if err == nil {
		err = s.tx(r.Context(), func(tx *sql.Tx) error {
			var e error
			out, e = s.tokens(r.Context(), tx, u)
			return e
		})
	}
	s.reply(w, r, 200, out, err)
}

func (s *Server) Refresh(w http.ResponseWriter, r *http.Request) {
	b, err := readBody[api.RefreshRequest](r)
	if err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	bad := problem(401, "REFRESH_TOKEN_INVALID", "Неверный или использованный refresh token")
	c, err := s.parse(b.RefreshToken, "refresh")
	if err != nil {
		s.reply(w, r, 0, nil, bad)
		return
	}
	id, ok := c["jti"].(string)
	sub, subErr := c.GetSubject()
	uid, idErr := strconv.ParseInt(sub, 10, 64)
	if !ok || !tokenIDPattern.MatchString(id) || subErr != nil || idErr != nil || uid <= 0 {
		s.reply(w, r, 0, nil, bad)
		return
	}
	var out api.AuthTokens
	err = s.tx(r.Context(), func(tx *sql.Tx) error {
		var userID int64
		err := tx.QueryRowContext(r.Context(), `DELETE FROM refresh_tokens
			WHERE id=$1 AND user_id=$2 AND expires_at>clock_timestamp() RETURNING user_id`, id, uid).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return bad
		}
		if err != nil {
			return err
		}
		var u api.UserResponse
		if err = tx.QueryRowContext(r.Context(), "SELECT id,email,role FROM users WHERE id=$1", userID).Scan(&u.ID, &u.Email, &u.Role); err != nil {
			return err
		}
		out, err = s.tokens(r.Context(), tx, u)
		return err
	})
	s.reply(w, r, 200, out, err)
}

func (s *Server) BootstrapAdmin(ctx context.Context) error {
	if s.cfg.AdminEmail == "" {
		return nil
	}
	props := s.spec.Components.Schemas["RegisterRequest"].Value.Properties
	if err := props["email"].Value.VisitJSON(s.cfg.AdminEmail); err != nil {
		return errors.New("ADMIN_EMAIL has invalid format")
	}
	if err := props["password"].Value.VisitJSON(s.cfg.AdminPass); err != nil {
		return errors.New("ADMIN_PASSWORD must contain 8-72 printable ASCII characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(s.cfg.AdminPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	email := strings.ToLower(s.cfg.AdminEmail)
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(email,password_hash,role)
		VALUES($1,$2,'ADMIN') ON CONFLICT(email) DO NOTHING`, email, string(hash))
	if err != nil {
		return err
	}
	var role string
	if err = s.db.QueryRowContext(ctx, "SELECT role FROM users WHERE email=$1", email).Scan(&role); err != nil {
		return err
	}
	if role != "ADMIN" {
		return fmt.Errorf("ADMIN_EMAIL is already registered without ADMIN role")
	}
	return nil
}
