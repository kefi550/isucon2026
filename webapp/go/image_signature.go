package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

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
	sig []byte
	url string
}

var (
	imageSignatureCacheMu sync.RWMutex
	imageSignatureCache   = map[imageSignatureCacheKey]imageSignatureCacheEntry{}
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
	imageSignatureCacheMu.Unlock()
}

func clearImageSignatureCache() {
	imageSignatureCacheMu.Lock()
	imageSignatureCache = map[imageSignatureCacheKey]imageSignatureCacheEntry{}
	imageSignatureCacheMu.Unlock()
}

func imageSignature(key []byte, imageID string, exp int64) []byte {
	if cacheKey, ok := newImageSignatureCacheKey(key, imageID, exp); ok {
		if entry, ok := loadImageSignature(cacheKey); ok {
			return entry.sig
		}
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
	exp := imageSignatureExpiry(time.Now())
	cacheKey, ok := newImageSignatureCacheKey(user.ImageKey, imageID, exp)
	if !ok {
		return formatSignedImageURL(imageID, exp, computeImageSignature(user.ImageKey, imageID, exp))
	}
	if entry, ok := loadImageSignature(cacheKey); ok {
		return entry.url
	}
	value, _, _ := imageSignatureFlight.Do(cacheKey.flightKey(), func() (any, error) {
		if entry, ok := loadImageSignature(cacheKey); ok {
			return entry.url, nil
		}
		sig := computeImageSignature(user.ImageKey, imageID, exp)
		entry := imageSignatureCacheEntry{sig: sig, url: formatSignedImageURL(imageID, exp, sig)}
		storeImageSignature(cacheKey, entry)
		return entry.url, nil
	})
	return value.(string)
}
