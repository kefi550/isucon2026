package isureal

import (
	"bufio"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// リクエストの中で署名を計算した（使い回せなかった）画像を記録し、127.0.0.1:6060/debug/image-sign-misses で返す。
// 事前に署名すべき画像を選ぶための調査用。分類は profile.sh がベンチのあとで DB と突き合わせて行う。

// 1回のベンチの外れは約4万件。メモリを使いすぎないよう上限を置く。
const imageSignMissLogLimit = 300000

type imageSignMiss struct {
	kind    string
	imageID string
	at      time.Time
}

var (
	imageSignMissMu  sync.Mutex
	imageSignMissLog []imageSignMiss
)

func recordImageSignMiss(kind, imageID string) {
	imageSignMissMu.Lock()
	if len(imageSignMissLog) < imageSignMissLogLimit {
		imageSignMissLog = append(imageSignMissLog, imageSignMiss{kind: kind, imageID: imageID, at: time.Now()})
	}
	imageSignMissMu.Unlock()
}

func resetImageSignMisses() {
	imageSignMissMu.Lock()
	imageSignMissLog = nil
	imageSignMissMu.Unlock()
}

func init() {
	// 1行に「種類<TAB>画像 ID<TAB>時刻（unix ミリ秒）」。
	http.HandleFunc("/debug/image-sign-misses", func(w http.ResponseWriter, _ *http.Request) {
		imageSignMissMu.Lock()
		misses := imageSignMissLog
		imageSignMissMu.Unlock()
		w.Header().Set("Content-Type", "text/tab-separated-values")
		out := bufio.NewWriter(w)
		for _, m := range misses {
			fmt.Fprintf(out, "%s\t%s\t%d\n", m.kind, m.imageID, m.at.UnixMilli())
		}
		out.Flush()
	})
}
