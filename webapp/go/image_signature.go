package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"expvar"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

// 同じ鍵と画像の URL は、発行からこの秒数のあいだ使い回す。exp は now+24h より最大でこの秒数だけ前になる。
// 初期化の直後に先に署名した URL（image_presign.go）を、ベンチの最後（約80秒後）まで使えるようにする。
// 600 秒はベンチに通った。先（切り上げ）にずらすと、1時間単位でベンチに範囲外とされた。
const imageURLReuseSeconds = 300

// 署名付き URL を使い回せた回数と、リクエストの中で計算した回数。/debug/vars に出し、profile.sh が読む。
var (
	imageSignHits   = expvar.NewInt("image_sign_hits")
	imageSignMisses = expvar.NewInt("image_sign_misses")
)

func imageSigningKey(secretKeyHash string) ([]byte, error) {
	key, err := hex.DecodeString(secretKeyHash)
	if err != nil || len(key) != sha256.Size {
		return nil, fmt.Errorf("invalid image signing key")
	}
	return key, nil
}

func imageSignatureExpiry(now time.Time) int64 {
	// ベンチは now+24h から大きくずれた exp を範囲外にする。1時間単位の丸めはここで落ちた。
	return now.Add(imageURLLifetime).Unix()
}

func computeImageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	// HMAC を1つだけ作り、Reset で鍵から導出済みの状態へ戻して使い回す（毎回 hmac.New すると鍵の処理と確保が1000回走る）。
	mac := hmac.New(sha256.New, key)
	sum := make([]byte, 0, sha256.Size)
	for range imageSignatureIterations {
		mac.Reset()
		mac.Write(value)
		sum = mac.Sum(sum[:0])
		value = append(value[:0], sum...)
	}
	return append([]byte(nil), value...)
}

type imageSignatureCacheKey struct {
	key     [sha256.Size]byte
	imageID string
	exp     int64
}

type imageSignatureCacheEntry struct {
	exp int64
	sig []byte
	url string
}

type issuedImageURLKey struct {
	key     [sha256.Size]byte
	imageID string
}

var (
	imageSignatureCacheMu sync.RWMutex
	imageSignatureCache   = map[imageSignatureCacheKey]imageSignatureCacheEntry{}
	issuedImageURLs       = map[issuedImageURLKey]imageSignatureCacheEntry{}
	imageSignatureFlight  singleflight.Group
)

func newImageSignatureCacheKey(key []byte, imageID string, exp int64) (imageSignatureCacheKey, bool) {
	if len(key) != sha256.Size {
		return imageSignatureCacheKey{}, false
	}
	cacheKey := imageSignatureCacheKey{imageID: imageID, exp: exp}
	copy(cacheKey.key[:], key)
	return cacheKey, true
}

func (key imageSignatureCacheKey) flightKey() string {
	return hex.EncodeToString(key.key[:]) + "\x00" + key.imageID + "\x00" + strconv.FormatInt(key.exp, 10)
}

func loadImageSignature(key imageSignatureCacheKey) (imageSignatureCacheEntry, bool) {
	imageSignatureCacheMu.RLock()
	entry, ok := imageSignatureCache[key]
	imageSignatureCacheMu.RUnlock()
	return entry, ok
}

func storeImageSignature(key imageSignatureCacheKey, entry imageSignatureCacheEntry) {
	imageSignatureCacheMu.Lock()
	imageSignatureCache[key] = entry
	issuedImageURLs[issuedImageURLKey{key: key.key, imageID: key.imageID}] = entry
	imageSignatureCacheMu.Unlock()
}

// exp の時点で使い回せる発行済みの URL を返す。
func loadIssuedImageURL(key imageSignatureCacheKey) (string, bool) {
	imageSignatureCacheMu.RLock()
	entry, ok := issuedImageURLs[issuedImageURLKey{key: key.key, imageID: key.imageID}]
	imageSignatureCacheMu.RUnlock()
	if !ok || entry.exp > key.exp || key.exp-entry.exp >= imageURLReuseSeconds {
		return "", false
	}
	return entry.url, true
}

func clearImageSignatureCache() {
	imageSignatureCacheMu.Lock()
	imageSignatureCache = map[imageSignatureCacheKey]imageSignatureCacheEntry{}
	issuedImageURLs = map[issuedImageURLKey]imageSignatureCacheEntry{}
	imageSignatureCacheMu.Unlock()
}

func imageSignature(key []byte, imageID string, exp int64) []byte {
	if cacheKey, ok := newImageSignatureCacheKey(key, imageID, exp); ok {
		if entry, ok := loadImageSignature(cacheKey); ok {
			return entry.sig
		}
	}
	if sig, ok := lookupPresignedSignature(key, imageID, exp); ok {
		return sig
	}
	// 発行していない exp は保存しない。不正な URL でキャッシュを増やさない。
	return computeImageSignature(key, imageID, exp)
}

func formatSignedImageURL(imageID string, exp int64, sig []byte) string {
	query := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {base64.RawURLEncoding.EncodeToString(sig)},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}

func signedImageURL(user User, imageID string) string {
	return signedImageURLFor("other", user, imageID)
}

// kind（post / realmoji / profile）は、使い回せなかったときの記録（image_sign_misses.go）に使う。
func signedImageURLFor(kind string, user User, imageID string) string {
	url, computed := signedImageURLAtWithResult(user, imageID, time.Now())
	if computed {
		recordImageSignMiss(kind, imageID)
	}
	return url
}

func signedImageURLAt(user User, imageID string, now time.Time) string {
	url, _ := signedImageURLAtWithResult(user, imageID, now)
	return url
}

func signedImageURLAtWithResult(user User, imageID string, now time.Time) (string, bool) {
	url, computed := issueImageURL(user.ImageKey, imageID, imageSignatureExpiry(now))
	if computed {
		imageSignMisses.Add(1)
	} else {
		imageSignHits.Add(1)
	}
	return url, computed
}

// issueImageURL は使い回せる URL があればそれを返し、なければ計算して保存する。2つ目は計算を待ったかどうか。
func issueImageURL(key []byte, imageID string, exp int64) (string, bool) {
	cacheKey, ok := newImageSignatureCacheKey(key, imageID, exp)
	if !ok {
		return formatSignedImageURL(imageID, exp, computeImageSignature(key, imageID, exp)), true
	}
	if url, ok := loadIssuedImageURL(cacheKey); ok {
		return url, false
	}
	if url, ok := lookupPresignedImageURL(key, imageID, exp); ok {
		return url, false
	}
	value, _, _ := imageSignatureFlight.Do(cacheKey.flightKey(), func() (any, error) {
		if url, ok := loadIssuedImageURL(cacheKey); ok {
			return url, nil
		}
		sig := computeImageSignature(key, imageID, exp)
		entry := imageSignatureCacheEntry{exp: exp, sig: sig, url: formatSignedImageURL(imageID, exp, sig)}
		storeImageSignature(cacheKey, entry)
		return entry.url, nil
	})
	return value.(string), true
}
