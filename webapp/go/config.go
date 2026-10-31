package isureal

import (
	"net"
	"net/url"
	"os"
	"strconv"
)

type Config struct {
	Region     string
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	LogLevel   string
	// DB への接続数の上限（open と idle の両方）
	DBMaxConns int
}

func ConfigFromEnv() *Config {
	region := envOrDefault("ISUREAL_REGION", "asia")
	// asia は DB と同じリージョンで1本の接続をすぐ返すので、少ない本数で足りる。
	// 2コアの DB に同時に流し込みすぎると、CPU とロック管理の取り合いで全員が遅くなる。
	// europe と america は1往復 約100ms のあいだ接続を握るので、本数を多めにする。
	maxConns := 75
	if region == "asia" {
		maxConns = 24
	}
	if n, err := strconv.Atoi(os.Getenv("ISUREAL_DB_MAX_CONNS")); err == nil && n > 0 {
		maxConns = n
	}
	return &Config{
		Region:     region,
		DBHost:     envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:     envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:     envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword: envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:     envOrDefault("ISUREAL_DB_NAME", "isureal"),
		LogLevel:   envOrDefault("ISUREAL_LOG_LEVEL", "debug"),
		DBMaxConns: maxConns,
	}
}

func (c *Config) DatabaseDSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.DBUser, c.DBPassword),
		Host:   net.JoinHostPort(c.DBHost, c.DBPort),
		Path:   "/" + c.DBName,
	}
	return u.String()
}

// ImageDatabaseDSN は画像取得専用の接続プール用の DSN。
// 画像のテーブルは36個のパーティションに分かれていて、毎回計画を作り直すと1回40ms以上の CPU を使うため、
// この接続だけ plan_cache_mode=force_generic_plan にして計画を使い回させる（pgx が接続時の設定として送る）。
func (c *Config) ImageDatabaseDSN() string {
	u, err := url.Parse(c.DatabaseDSN())
	if err != nil {
		return c.DatabaseDSN()
	}
	query := u.Query()
	query.Set("plan_cache_mode", "force_generic_plan")
	u.RawQuery = query.Encode()
	return u.String()
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
