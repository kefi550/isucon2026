package isureal

import (
	"crypto/hmac"
	"encoding/base64"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestImageSignatureExpiryIsExactlyOneDay(t *testing.T) {
	cases := []time.Time{
		time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 31, 10, 0, 1, 0, time.UTC),
		time.Date(2026, 10, 31, 10, 59, 59, 0, time.UTC),
	}
	for _, now := range cases {
		exp := imageSignatureExpiry(now)
		if exp != now.Add(imageURLLifetime).Unix() {
			t.Fatalf("now=%s exp=%d", now, exp)
		}
	}
}

func TestSignedImageURLReusesSignature(t *testing.T) {
	clearImageSignatureCache()
	key := bytesRepeat(0xab)
	user := User{ImageKey: key}
	imageID := "11111111-1111-1111-1111-111111111111"

	before := time.Now().Add(imageURLLifetime).Unix()
	first := signedImageURL(user, imageID)
	second := signedImageURL(user, imageID)
	after := time.Now().Add(imageURLLifetime).Unix()
	if before == after && first != second {
		t.Fatalf("expected a cache hit\n%s\n%s", first, second)
	}

	exp, sig := parseSignedImageURL(t, first, imageID)
	if exp < before || exp > after {
		t.Fatalf("exp %d is outside %d..%d", exp, before, after)
	}
	want := computeImageSignature(key, imageID, exp)
	if !hmac.Equal(sig, want) {
		t.Fatal("cached signature does not match HMAC")
	}
	if !hmac.Equal(imageSignature(key, imageID, exp), want) {
		t.Fatal("verify path does not match HMAC")
	}
	if hmac.Equal(imageSignature(key, imageID, exp+1), want) {
		t.Fatal("a different exp must not reuse the signature")
	}

	cacheKey, ok := newImageSignatureCacheKey(key, imageID, exp)
	if !ok {
		t.Fatal("cache key")
	}
	if _, ok := loadImageSignature(cacheKey); !ok {
		t.Fatal("signature was not cached")
	}
	clearImageSignatureCache()
	if _, ok := loadImageSignature(cacheKey); ok {
		t.Fatal("cache was not cleared")
	}
	if !hmac.Equal(imageSignature(key, imageID, exp), want) {
		t.Fatal("signature changed after the cache was cleared")
	}
}

func TestSignedImageURLConcurrent(t *testing.T) {
	clearImageSignatureCache()
	user := User{ImageKey: bytesRepeat(0x11)}
	const n = 32
	urls := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range urls {
		go func() {
			defer wg.Done()
			urls[i] = signedImageURL(user, "image-concurrent")
		}()
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if urls[i] != urls[0] {
			t.Fatalf("concurrent URLs differ: %s vs %s", urls[0], urls[i])
		}
	}
}

func parseSignedImageURL(t *testing.T, raw, imageID string) (int64, []byte) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/api/images/"+imageID {
		t.Fatalf("path = %s", parsed.Path)
	}
	exp, err := strconv.ParseInt(parsed.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parsed.Query().Get("sig"))
	if err != nil {
		t.Fatal(err)
	}
	return exp, sig
}

func bytesRepeat(b byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = b
	}
	return key
}
