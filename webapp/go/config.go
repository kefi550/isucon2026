package isureal

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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
	// SessionDBConns はセッション確認専用の接続プールの本数
	SessionDBConns int
	// 初期化のときに画像の署名を先に計算する（image_presign.go）。
	// PresignLocalItems 件を自分で計算してから初期化の応答を返し、残りは PresignWorkers（ssh 先）に分けて任せる。
	// 両方 0 / 空なら計算しない。
	PresignLocalItems int
	PresignWorkers    []string
	// 自分の分のあと、末尾から計算を続ける時間。PresignWait は初期化の応答を返すまで、PresignBackground はそのあと。
	PresignWait       time.Duration
	PresignBackground time.Duration
}

func ConfigFromEnv() *Config {
	region := envOrDefault("ISUREAL_REGION", "asia")
	// asia は DB と同じリージョンで1本の接続をすぐ返すので、少ない本数で足りる。
	// 2コアの DB に同時に流し込みすぎると、CPU とロック管理の取り合いで全員が遅くなる。
	// europe と america は1往復 約100ms のあいだ接続を握るので、本数を多めにする。
	// ただし DB（メモリ3.8GB）は接続1本ごとにプロセスとキャッシュを持ち、待機中の接続が190本を超えたところで
	// メモリが尽きてスラッシングした。30本でも毎秒300往復さばけ、europe/america の負荷には十分足りる。
	maxConns := 30
	if region == "asia" {
		maxConns = 24
	}
	if n, err := strconv.Atoi(os.Getenv("ISUREAL_DB_MAX_CONNS")); err == nil && n > 0 {
		maxConns = n
	}
	sessionConns := 6
	if n, err := strconv.Atoi(os.Getenv("ISUREAL_DB_SESSION_CONNS")); err == nil && n > 0 {
		sessionConns = n
	}
	// isu1 は署名1件 約0.45ms で、2万件なら約5秒。残りは SHA 命令のある isu2/isu3（約0.14ms）に任せる。
	presignLocal := 0
	if n, err := strconv.Atoi(os.Getenv("ISUREAL_PRESIGN_LOCAL")); err == nil && n >= 0 {
		presignLocal = n
	}
	var presignWorkers []string
	for _, host := range strings.Split(os.Getenv("ISUREAL_PRESIGN_WORKERS"), ",") {
		if host = strings.TrimSpace(host); host != "" {
			presignWorkers = append(presignWorkers, host)
		}
	}
	// 初期化の時間の制限は分からない（約15秒は通った）。応答のあと約35秒は負荷が軽く、isu1 の CPU が空いている。
	presignSeconds := func(key string) time.Duration {
		if n, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil && n > 0 {
			return time.Duration(n * float64(time.Second))
		}
		return 0
	}
	return &Config{
		Region:         region,
		DBHost:         envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:         envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:         envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword:     envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:         envOrDefault("ISUREAL_DB_NAME", "isureal"),
		LogLevel:       envOrDefault("ISUREAL_LOG_LEVEL", "debug"),
		DBMaxConns:     maxConns,
		SessionDBConns: sessionConns,

		PresignLocalItems: presignLocal,
		PresignWorkers:    presignWorkers,
		PresignWait:       presignSeconds("ISUREAL_PRESIGN_WAIT"),
		PresignBackground: presignSeconds("ISUREAL_PRESIGN_BACKGROUND"),
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
