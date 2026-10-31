package isureal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"strconv"
	"testing"
	"time"
)

// 実機で ssh 先の presign-worker の速さを測る。普段は動かない。
// PRESIGN_REMOTE_HOST=192.168.2.10 PRESIGN_REMOTE_COMMAND="/tmp/isureal presign-worker" PRESIGN_REMOTE_ITEMS=300000
func TestPresignRemoteThroughput(t *testing.T) {
	host := os.Getenv("PRESIGN_REMOTE_HOST")
	if host == "" {
		t.Skip("PRESIGN_REMOTE_HOST is not set")
	}
	n, _ := strconv.Atoi(os.Getenv("PRESIGN_REMOTE_ITEMS"))
	keys := make([][sha256.Size]byte, 5000)
	for i := range keys {
		rand.Read(keys[i][:])
	}
	items := make([]presignItem, n)
	share := make([]uint32, n)
	for i := range items {
		items[i].owner = uint32(i % len(keys))
		rand.Read(items[i].id[:])
		share[i] = uint32(i)
	}
	store := &presignStore{exp: imageSignatureExpiry(time.Now()), keys: keys, sigs: make(map[[16]byte]presignedSig, n)}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Second):
				t.Logf("%v: %d", time.Since(start).Round(time.Second), imagePresigned.Value())
			}
		}
	}()
	got, err := runRemotePresign(context.Background(), host, os.Getenv("PRESIGN_REMOTE_COMMAND"), store, items, share)
	close(done)
	elapsed := time.Since(start)
	t.Logf("%d / %d items in %v (%.0f/s), err=%v", got, n, elapsed, float64(got)/elapsed.Seconds(), err)
	for _, i := range []int{0, n / 2, n - 1} {
		key := keys[items[i].owner][:]
		id := formatImageUUID(items[i].id)
		if sig, ok := lookupPresignedSignatureIn(store, key, id); !ok || string(sig) != string(computeImageSignature(key, id, store.exp)) {
			t.Errorf("item %d: signature mismatch", i)
		}
	}
}

func lookupPresignedSignatureIn(s *presignStore, key []byte, imageID string) ([]byte, bool) {
	e, ok := s.get(key, imageID)
	return e.sig[:], ok
}
