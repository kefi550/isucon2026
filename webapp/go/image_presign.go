package isureal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

// 初期化のときに、全ユーザーのプロフィール画像・投稿・リアクションの署名付き URL を先に計算しておく。
// ベンチで署名が外れる画像の9割以上は初期データ（どの投稿が見られるかは絞れない）なので、全部（約180万件）を対象にする。
// isu1 は署名1件 約0.45ms と遅く、isu2/isu3 は SHA 命令があり約0.14ms で、負荷もほとんどない。
// そこで isu1 は先頭の Config.PresignLocalItems 件（当たりやすいプロフィール画像から）だけ計算して初期化の応答を返し、
// 残りは ssh（プライベート IP。Router を経由する）で isu2/isu3 の同じバイナリ（presign-worker）に分けて計算させ、
// 結果を受け取りながらキャッシュに入れていく。exp は初期化の時刻で揃え、imageURLReuseSeconds のあいだ使える。

var imagePresigned = expvar.NewInt("image_presigned")

// 計算した署名。画像 ID（UUID）を16バイトにしてキーにし、ポインタを持たない形にする（180万件でも GC がたどらない）。
type presignStore struct {
	exp  int64
	keys [][sha256.Size]byte
	mu   sync.RWMutex
	sigs map[[16]byte]presignedSig
}

type presignedSig struct {
	owner uint32
	sig   [sha256.Size]byte
}

type presignItem struct {
	owner uint32
	id    [16]byte
}

var (
	currentPresign atomic.Pointer[presignStore]
	presignMu      sync.Mutex
	presignCancel  context.CancelFunc = func() {}
)

func (s *presignStore) put(items []presignItem, idx []uint32, sigs [][sha256.Size]byte) {
	s.mu.Lock()
	for i, n := range idx {
		s.sigs[items[n].id] = presignedSig{owner: items[n].owner, sig: sigs[i]}
	}
	s.mu.Unlock()
	imagePresigned.Add(int64(len(idx)))
}

// lookupPresignedImageURL は、先に計算した署名が使えればその URL を返す。持ち主の鍵が変わっていれば使わない。
func lookupPresignedImageURL(key []byte, imageID string, exp int64) (string, bool) {
	s := currentPresign.Load()
	if s == nil || exp < s.exp || exp-s.exp >= imageURLReuseSeconds {
		return "", false
	}
	if e, ok := s.get(key, imageID); ok {
		return formatSignedImageURL(imageID, s.exp, e.sig[:]), true
	}
	return "", false
}

// lookupPresignedSignature は、画像の取得で届いた URL（exp が先に計算したときのもの）の署名を返す。
func lookupPresignedSignature(key []byte, imageID string, exp int64) ([]byte, bool) {
	s := currentPresign.Load()
	if s == nil || exp != s.exp {
		return nil, false
	}
	if e, ok := s.get(key, imageID); ok {
		return e.sig[:], true
	}
	return nil, false
}

func (s *presignStore) get(key []byte, imageID string) (presignedSig, bool) {
	id, ok := parseImageUUID(imageID)
	if !ok {
		return presignedSig{}, false
	}
	s.mu.RLock()
	e, ok := s.sigs[id]
	s.mu.RUnlock()
	if !ok || !bytes.Equal(s.keys[e.owner][:], key) {
		return presignedSig{}, false
	}
	return e, true
}

// parseImageUUID は小文字の UUID 文字列だけを受け付ける（16バイトから同じ文字列に戻せるものだけ）。
func parseImageUUID(s string) ([16]byte, bool) {
	var id [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return id, false
	}
	var buf [32]byte
	copy(buf[0:8], s[0:8])
	copy(buf[8:12], s[9:13])
	copy(buf[12:16], s[14:18])
	copy(buf[16:20], s[19:23])
	copy(buf[20:32], s[24:36])
	for _, c := range buf {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return id, false
		}
	}
	if _, err := hex.Decode(id[:], buf[:]); err != nil {
		return id, false
	}
	return id, true
}

func formatImageUUID(id [16]byte) string {
	var buf [36]byte
	hex.Encode(buf[0:8], id[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], id[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], id[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], id[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], id[10:16])
	return string(buf[:])
}

func clearImagePresign() {
	presignMu.Lock()
	presignCancel()
	presignCancel = func() {}
	presignMu.Unlock()
	currentPresign.Store(nil)
}

// runImagePresign は自分の分（先頭 config.PresignLocalItems 件）を計算し終えたら戻る。ssh 先の分は裏で続く。
func runImagePresign(ctx context.Context, db *sqlx.DB, config *Config) error {
	clearImagePresign()
	imagePresigned.Set(0)
	imageSignHits.Set(0)
	imageSignMisses.Set(0)
	if config.PresignLocalItems == 0 && len(config.PresignWorkers) == 0 {
		return nil
	}
	start := time.Now()
	exp := imageSignatureExpiry(start)
	keys, items, err := loadPresignItems(ctx, db)
	if err != nil {
		return err
	}
	store := &presignStore{exp: exp, keys: keys, sigs: make(map[[16]byte]presignedSig, len(items))}
	currentPresign.Store(store)
	slog.Info("presign: loaded", "items", len(items), "elapsed", time.Since(start))

	local := min(config.PresignLocalItems, len(items))
	// ssh 先は、残りを1件ずつ交互に受け持つ（どちらも当たりやすい順に進む）。ベンチが終わるころには打ち切る。
	bg, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	presignMu.Lock()
	presignCancel = cancel
	presignMu.Unlock()
	// ssh 先にも同じ場所に同じバイナリがある（push.sh が3台に配る）。
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	for w, host := range config.PresignWorkers {
		var share []uint32
		for i := local + w; i < len(items); i += len(config.PresignWorkers) {
			share = append(share, uint32(i))
		}
		go func() {
			began := time.Now()
			n, err := runRemotePresign(bg, host, exe+" presign-worker", store, items, share)
			slog.Info("presign: remote done", "host", host, "items", n, "share", len(share), "elapsed", time.Since(began), "error", err)
		}()
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signLocal(store, items, func() int {
				if i := int(next.Add(1)) - 1; i < local && ctx.Err() == nil {
					return i
				}
				return -1
			})
		}()
	}
	wg.Wait()
	slog.Info("presign: local done", "items", local, "elapsed", time.Since(start))
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// 自分の分が終わったら、ssh 先と反対側（末尾）から計算する。ssh 先が済ませた分は飛ばす。
	// 初期化の中で config.PresignWait 待ち、応答のあとも負荷が上がるまで config.PresignBackground 続ける。
	waitUntil := time.Now().Add(config.PresignWait)
	stopAt := waitUntil.Add(config.PresignBackground)
	var back atomic.Int64
	var tail sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		tail.Add(1)
		go func() {
			defer tail.Done()
			signLocal(store, items, func() int {
				for bg.Err() == nil && time.Now().Before(stopAt) {
					i := len(items) - 1 - int(back.Add(1)-1)
					if i < local {
						return -1
					}
					if !store.has(items[i].id) {
						return i
					}
				}
				return -1
			})
		}()
	}
	go func() {
		tail.Wait()
		slog.Info("presign: local tail done", "items", back.Load(), "elapsed", time.Since(start))
	}()
	select {
	case <-time.After(time.Until(waitUntil)):
	case <-ctx.Done():
	}
	slog.Info("presign: initialize returns", "presigned", imagePresigned.Value(), "items", len(items), "elapsed", time.Since(start))
	return ctx.Err()
}

// signLocal は next が返す番号（-1 で終わり）の署名を計算してキャッシュに入れる。
func signLocal(store *presignStore, items []presignItem, next func() int) {
	const batch = 64
	idx := make([]uint32, 0, batch)
	sigs := make([][sha256.Size]byte, 0, batch)
	for {
		i := next()
		if i < 0 {
			break
		}
		var sig [sha256.Size]byte
		copy(sig[:], computeImageSignature(store.keys[items[i].owner][:], formatImageUUID(items[i].id), store.exp))
		idx = append(idx, uint32(i))
		sigs = append(sigs, sig)
		if len(idx) == batch {
			store.put(items, idx, sigs)
			idx, sigs = idx[:0], sigs[:0]
		}
	}
	store.put(items, idx, sigs)
}

func (s *presignStore) has(id [16]byte) bool {
	s.mu.RLock()
	_, ok := s.sigs[id]
	s.mu.RUnlock()
	return ok
}

// loadPresignItems は署名の対象を、当たりやすい順（プロフィール画像、投稿、リアクション）に並べて返す。
func loadPresignItems(ctx context.Context, db *sqlx.DB) ([][sha256.Size]byte, []presignItem, error) {
	owners := map[string]uint32{}
	var keys [][sha256.Size]byte
	var items []presignItem
	rows, err := db.QueryContext(ctx, `SELECT id, secret_key_hash, profile_image_id FROM users`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, hash string
		var profile sql.NullString
		if err := rows.Scan(&id, &hash, &profile); err != nil {
			rows.Close()
			return nil, nil, err
		}
		key, err := imageSigningKey(hash)
		if err != nil {
			continue
		}
		owner := uint32(len(keys))
		keys = append(keys, [sha256.Size]byte(key))
		owners[id] = owner
		if profile.Valid {
			if uuid, ok := parseImageUUID(profile.String); ok {
				items = append(items, presignItem{owner: owner, id: uuid})
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var lists [2][]presignItem
	for q, query := range []string{
		`SELECT author_id, image_id FROM posts WHERE deleted_at IS NULL`,
		`SELECT reactor_id, image_id FROM realmojis WHERE replaced_at IS NULL AND deleted_at IS NULL`,
	} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, nil, err
		}
		var ownerID, imageID sql.RawBytes
		for rows.Next() {
			if err := rows.Scan(&ownerID, &imageID); err != nil {
				rows.Close()
				return nil, nil, err
			}
			owner, ok := owners[string(ownerID)]
			if !ok {
				continue
			}
			if uuid, ok := parseImageUUID(string(imageID)); ok {
				lists[q] = append(lists[q], presignItem{owner: owner, id: uuid})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return keys, append(items, interleavePresignItems(lists[0], lists[1])...), nil
}

// interleavePresignItems は a と b を件数の比で混ぜる（どこで打ち切られても両方が同じ割合で進んでいる）。
func interleavePresignItems(a, b []presignItem) []presignItem {
	out := make([]presignItem, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if j >= len(b) || (i < len(a) && i*len(b) <= j*len(a)) {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return out
}

// presign-worker とのやりとり（リトルエンディアン）。
// 送る: exp int64, 鍵の数 uint32, 鍵 [32]byte..., 件数 uint32, {番号 uint32, 鍵の番号 uint32, 画像 ID [16]byte}...
// 返る: {番号 uint32, 署名 [32]byte}...（計算できた順）
const presignResultSize = 4 + sha256.Size

func runRemotePresign(ctx context.Context, host, command string, store *presignStore, items []presignItem, share []uint32) (int, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=5",
		host, command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() {
		writePresignRequest(stdin, store, items, share)
		stdin.Close()
	}()
	total, err := readPresignResults(stdout, store, items)
	if werr := cmd.Wait(); werr != nil && err == nil {
		err = fmt.Errorf("%w: %s", werr, bytes.TrimSpace(stderr.Bytes()))
	}
	return total, err
}

func writePresignRequest(out io.Writer, store *presignStore, items []presignItem, share []uint32) error {
	w := bufio.NewWriterSize(out, 1<<20)
	var head [12]byte
	binary.LittleEndian.PutUint64(head[0:8], uint64(store.exp))
	binary.LittleEndian.PutUint32(head[8:12], uint32(len(store.keys)))
	w.Write(head[:])
	for _, key := range store.keys {
		w.Write(key[:])
	}
	binary.LittleEndian.PutUint32(head[0:4], uint32(len(share)))
	w.Write(head[0:4])
	var rec [24]byte
	for _, i := range share {
		binary.LittleEndian.PutUint32(rec[0:4], i)
		binary.LittleEndian.PutUint32(rec[4:8], items[i].owner)
		copy(rec[8:24], items[i].id[:])
		w.Write(rec[:])
	}
	return w.Flush()
}

func readPresignResults(in io.Reader, store *presignStore, items []presignItem) (int, error) {
	r := bufio.NewReaderSize(in, 1<<20)
	const batch = 1024
	idx := make([]uint32, 0, batch)
	sigs := make([][sha256.Size]byte, 0, batch)
	total := 0
	var rec [presignResultSize]byte
	for {
		_, err := io.ReadFull(r, rec[:])
		if err == nil {
			i := binary.LittleEndian.Uint32(rec[0:4])
			if int(i) < len(items) {
				idx = append(idx, i)
				sigs = append(sigs, [sha256.Size]byte(rec[4:]))
			}
		}
		// 届いた分はすぐ使えるようにする（手元に続きがなければ、まとまるのを待たない）。
		if len(idx) == batch || (len(idx) > 0 && (err != nil || r.Buffered() < presignResultSize)) {
			store.put(items, idx, sigs)
			total += len(idx)
			idx, sigs = idx[:0], sigs[:0]
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return total, err
		}
	}
}

// RunPresignWorker は ssh 越しに呼ばれ、標準入力の対象を全コアで署名して標準出力に返す（cmd/isureal の presign-worker）。
func RunPresignWorker(in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 1<<20)
	var head [12]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}
	exp := int64(binary.LittleEndian.Uint64(head[0:8]))
	keys := make([][sha256.Size]byte, binary.LittleEndian.Uint32(head[8:12]))
	for i := range keys {
		if _, err := io.ReadFull(r, keys[i][:]); err != nil {
			return err
		}
	}
	if _, err := io.ReadFull(r, head[0:4]); err != nil {
		return err
	}
	type job struct {
		index, owner uint32
		id           [16]byte
	}
	jobs := make([]job, binary.LittleEndian.Uint32(head[0:4]))
	var rec [24]byte
	for i := range jobs {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			return err
		}
		jobs[i] = job{index: binary.LittleEndian.Uint32(rec[0:4]), owner: binary.LittleEndian.Uint32(rec[4:8]), id: [16]byte(rec[8:24])}
		if int(jobs[i].owner) >= len(keys) {
			return fmt.Errorf("invalid key index %d", jobs[i].owner)
		}
	}

	results := make(chan [presignResultSize]byte, 4096)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(jobs) {
					return
				}
				var res [presignResultSize]byte
				binary.LittleEndian.PutUint32(res[0:4], jobs[i].index)
				copy(res[4:], computeImageSignature(keys[jobs[i].owner][:], formatImageUUID(jobs[i].id), exp))
				results <- res
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	// 受け取る側が少しずつキャッシュに入れられるよう、200ms ごとに送り出す。
	w := bufio.NewWriterSize(out, 256<<10)
	lastFlush := time.Now()
	for res := range results {
		if _, err := w.Write(res[:]); err != nil {
			return err
		}
		if time.Since(lastFlush) > 200*time.Millisecond {
			if err := w.Flush(); err != nil {
				return err
			}
			lastFlush = time.Now()
		}
	}
	return w.Flush()
}
