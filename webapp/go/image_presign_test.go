package isureal

import (
	"crypto/sha256"
	"io"
	"testing"
	"time"
)

func TestImageUUIDRoundTrip(t *testing.T) {
	const s = "40c6362d-2341-45fa-b397-ea4dfdfc3f29"
	id, ok := parseImageUUID(s)
	if !ok || formatImageUUID(id) != s {
		t.Fatalf("round trip failed: %v %q", ok, formatImageUUID(id))
	}
	for _, bad := range []string{"40C6362D-2341-45FA-B397-EA4DFDFC3F29", "40c6362d234145fab397ea4dfdfc3f29", "image-1", "40c6362d-2341-45fa-b397-ea4dfdfc3f2g"} {
		if _, ok := parseImageUUID(bad); ok {
			t.Errorf("parseImageUUID(%q) accepted", bad)
		}
	}
}

func TestPresignWorkerRoundTrip(t *testing.T) {
	t.Cleanup(clearImagePresign)
	keys := [][sha256.Size]byte{{1}, {2}}
	ids := []string{
		"40c6362d-2341-45fa-b397-ea4dfdfc3f29",
		"0192f3a4-aaaa-bbbb-cccc-0123456789ab",
		"ffffffff-0000-1111-2222-333333333333",
	}
	var items []presignItem
	for i, s := range ids {
		id, _ := parseImageUUID(s)
		items = append(items, presignItem{owner: uint32(i % 2), id: id})
	}
	exp := imageSignatureExpiry(time.Now())
	store := &presignStore{exp: exp, keys: keys, sigs: map[[16]byte]presignedSig{}}
	currentPresign.Store(store)

	reqR, reqW := io.Pipe()
	resR, resW := io.Pipe()
	go func() {
		writePresignRequest(reqW, store, items, []uint32{0, 2})
		reqW.Close()
	}()
	go func() {
		resW.CloseWithError(RunPresignWorker(reqR, resW))
	}()
	n, err := readPresignResults(resR, store, items)
	if err != nil || n != 2 {
		t.Fatalf("readPresignResults = %d, %v", n, err)
	}

	for _, i := range []int{0, 2} {
		key := keys[items[i].owner][:]
		want := formatSignedImageURL(ids[i], exp, computeImageSignature(key, ids[i], exp))
		if got, ok := lookupPresignedImageURL(key, ids[i], exp+10); !ok || got != want {
			t.Errorf("item %d: got %q %v, want %q", i, got, ok, want)
		}
		if sig, ok := lookupPresignedSignature(key, ids[i], exp); !ok || string(sig) != string(computeImageSignature(key, ids[i], exp)) {
			t.Errorf("item %d: signature mismatch", i)
		}
		if _, ok := lookupPresignedImageURL(keys[1-items[i].owner][:], ids[i], exp); ok {
			t.Errorf("item %d: used with another owner's key", i)
		}
	}
	if _, ok := lookupPresignedImageURL(keys[1][:], ids[1], exp); ok {
		t.Error("item 1 was not sent but found")
	}
	if _, ok := lookupPresignedImageURL(keys[0][:], ids[0], exp+imageURLReuseSeconds); ok {
		t.Error("used after the reuse window")
	}
}
