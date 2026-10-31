package isureal

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// sendBatch は複数の SQL を1往復で送り、1つの暗黙のトランザクションとして順に実行する。
// 文ごとにクライアントとの往復を挟まないので、先頭の文で取った行ロックを握る時間は、サーバー内の実行時間だけになる
// （BEGIN → ロック → 判定 → COMMIT を別々に送ると、遅延の大きいリージョンでは往復の数だけロックを握り続ける）。
// READ COMMITTED では文ごとにスナップショットを取るので、ロックの後の文は、ロックを待っているあいだに
// 他のトランザクションがコミットした内容を見られる（別々に送った場合と同じ）。
// 途中の文が失敗したら、それ以降は実行されず、全体がロールバックされる。
// read は、キューに積んだ順に結果を読み出す。
func sendBatch(ctx context.Context, db *sqlx.DB, batch *pgx.Batch, read func(pgx.BatchResults) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		results := driverConn.(*stdlib.Conn).Conn().SendBatch(ctx, batch)
		if err := read(results); err != nil {
			results.Close()
			return err
		}
		return results.Close()
	})
}
