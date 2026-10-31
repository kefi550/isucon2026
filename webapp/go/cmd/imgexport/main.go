// imgexport は、初期データ（initial_data_boundary より前）の画像を、DB の bytea からファイルに書き出す。
// 書き出したファイルは、画像置き場（asia2 の nginx）が配信する。画像取得のたびに PostgreSQL の TOAST を約16ブロック読む代わりに、
// ファイルの1〜2ブロックで済ませるためのもの。初期データは変わらないので、1回書き出せばよい。
//
//	imgexport -dsn 'host=/var/run/postgresql user=postgres dbname=isureal' -out /var/lib/isureal-images
//
// ファイルは <out>/<idの先頭2文字>/<id>。すでにあるファイル（サイズ > 0）は飛ばすので、途中で止めても再実行できる。
// -verify N を付けると、書き出さずに、書き出し済みのファイルからランダムに N 件を選び、DB と照合する（sha256・サイズ）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dsn := flag.String("dsn", "host=/var/run/postgresql user=postgres dbname=isureal", "接続先")
	out := flag.String("out", "/var/lib/isureal-images", "書き出し先")
	workers := flag.Int("workers", 3, "同時に処理するパーティション数")
	limit := flag.Int("limit", 0, "テスト用: 1パーティションあたりの最大件数（0 なら全件）")
	verify := flag.Int("verify", 0, "N>0 のとき、書き出さずに、書き出し済みのファイルからランダムに N 件を選んで DB と照合する")
	flag.Parse()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	var boundary time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM initial_data_boundary`).Scan(&boundary); err != nil {
		fatal(fmt.Errorf("initial_data_boundary: %w", err))
	}
	// 初期データのパーティション: 今日の新しい画像のパーティション（image_objects_20261028 以降）と default は対象外
	rows, err := pool.Query(ctx, `
		SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'image_objects'::regclass AND c.relname < 'image_objects_20261028' AND c.relname <> 'image_objects_default'
		ORDER BY c.relname`)
	if err != nil {
		fatal(err)
	}
	var parts []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			fatal(err)
		}
		parts = append(parts, name)
	}
	rows.Close()
	sort.Strings(parts)
	fmt.Printf("境界 %s / パーティション %d 個 / 書き出し先 %s\n", boundary.Format(time.RFC3339), len(parts), *out)

	if *verify > 0 {
		os.Exit(verifySample(ctx, pool, boundary, *out, *verify))
	}

	var written, skipped, bytesTotal atomic.Int64
	sem := make(chan struct{}, *workers)
	var wg sync.WaitGroup
	start := time.Now()
	for _, part := range parts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w, s, b, err := exportPartition(ctx, pool, part, boundary, *out, *limit)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: エラー: %v\n", part, err)
				os.Exit(1)
			}
			written.Add(w)
			skipped.Add(s)
			bytesTotal.Add(b)
			fmt.Printf("%s: 書き出し %d / 既存 %d / %.0f MB（累計 %d 件、%.0f 秒）\n", part, w, s, float64(b)/1e6, written.Load(), time.Since(start).Seconds())
		}()
	}
	wg.Wait()
	fmt.Printf("完了: 書き出し %d 件 / 既存 %d 件 / %.1f GB / %.0f 秒\n", written.Load(), skipped.Load(), float64(bytesTotal.Load())/1e9, time.Since(start).Seconds())
}

func pathFor(out, id string) string { return filepath.Join(out, id[:2], id) }

func exportPartition(ctx context.Context, pool *pgxpool.Pool, part string, boundary time.Time, out string, limit int) (written, skipped, bytesN int64, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer conn.Release()
	q := fmt.Sprintf(`SELECT id, data FROM %s WHERE created_at <= $1`, pgx.Identifier{part}.Sanitize())
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := conn.Query(ctx, q, boundary)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	made := map[string]bool{}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return written, skipped, bytesN, err
		}
		if len(id) < 3 || strings.ContainsAny(id, "/.\\") {
			return written, skipped, bytesN, fmt.Errorf("想定外の id: %q", id)
		}
		p := pathFor(out, id)
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			skipped++
			continue
		}
		dir := filepath.Dir(p)
		if !made[dir] {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return written, skipped, bytesN, err
			}
			made[dir] = true
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return written, skipped, bytesN, err
		}
		if err := os.Rename(tmp, p); err != nil {
			return written, skipped, bytesN, err
		}
		written++
		bytesN += int64(len(data))
	}
	return written, skipped, bytesN, rows.Err()
}

// verifySample は、書き出し済みのファイルからランダムに最大 n 件を選び、DB の画像と sha256・サイズで照合する。
// （書き出し途中でも、少数のテストでも、置いてあるファイルが正しいかを確かめられる）
func verifySample(ctx context.Context, pool *pgxpool.Pool, boundary time.Time, out string, n int) int {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	var picked []string
	seen := 0
	dirs, err := os.ReadDir(out)
	if err != nil {
		fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(out, d.Name()))
		if err != nil {
			fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".tmp") {
				continue
			}
			seen++
			if len(picked) < n {
				picked = append(picked, f.Name())
			} else if j := rng.Intn(seen); j < n {
				picked[j] = f.Name()
			}
		}
	}
	bad, ok, notInDB := 0, 0, 0
	for _, id := range picked {
		var want string
		var size int
		err := pool.QueryRow(ctx, `SELECT encode(sha256(data), 'hex'), length(data) FROM image_objects WHERE id = $1 AND created_at <= $2`, id, boundary).Scan(&want, &size)
		if err == pgx.ErrNoRows {
			notInDB++
			fmt.Printf("DB にない（または初期データではない）: %s\n", id)
			continue
		}
		if err != nil {
			fatal(err)
		}
		b, err := os.ReadFile(pathFor(out, id))
		if err != nil {
			fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want || len(b) != size {
			bad++
			fmt.Printf("不一致: %s\n", id)
			continue
		}
		ok++
	}
	fmt.Printf("照合: ファイル %d 個のうち %d 件を確認 / 一致 %d / 不一致 %d / DB にない %d\n", seen, len(picked), ok, bad, notInDB)
	if bad > 0 || notInDB > 0 || len(picked) == 0 {
		return 1
	}
	return 0
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "エラー:", err)
	os.Exit(1)
}
