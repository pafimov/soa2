package app

import (
	"errors"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr        string
	DatabaseURL string
	AccessKey   string
	RefreshKey  string
	OrderLimit  time.Duration
	AdminEmail  string
	AdminPass   string
}

func LoadConfig() (Config, error) {
	c := Config{
		Addr:        os.Getenv("HTTP_ADDR"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AccessKey:   os.Getenv("JWT_ACCESS_SECRET"),
		RefreshKey:  os.Getenv("JWT_REFRESH_SECRET"),
		AdminEmail:  os.Getenv("ADMIN_EMAIL"),
		AdminPass:   os.Getenv("ADMIN_PASSWORD"),
		OrderLimit:  5 * time.Minute,
	}
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.AccessKey) < 32 || len(c.RefreshKey) < 32 || c.AccessKey == c.RefreshKey {
		return c, errors.New("JWT secrets must be different and at least 32 bytes long")
	}
	if v := os.Getenv("ORDER_LIMIT_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1440 {
			return c, errors.New("ORDER_LIMIT_MINUTES must be between 0 and 1440")
		}
		c.OrderLimit = time.Duration(n) * time.Minute
	}
	if (c.AdminEmail == "") != (c.AdminPass == "") {
		return c, errors.New("set both ADMIN_EMAIL and ADMIN_PASSWORD")
	}
	return c, nil
}
