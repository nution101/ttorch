package approval

import (
	"path/filepath"
	"testing"
	"time"
)

func TestGrantValidConsume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.approve")

	if Valid(path) {
		t.Fatal("no token yet, should be invalid")
	}
	if err := Grant(path, time.Minute, "sha-abc"); err != nil {
		t.Fatal(err)
	}
	if !Valid(path) {
		t.Fatal("freshly granted token should be valid")
	}
	data, ok := Consume(path)
	if !ok || data != "sha-abc" {
		t.Fatalf("consume returned data=%q ok=%v, want sha-abc/true", data, ok)
	}
	if Valid(path) {
		t.Fatal("token should be gone after consume")
	}
	if _, ok := Consume(path); ok {
		t.Fatal("second consume should fail")
	}
}

func TestExpiredToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.approve")
	if err := Grant(path, -time.Second, "sha-abc"); err != nil {
		t.Fatal(err)
	}
	if Valid(path) {
		t.Fatal("expired token should be invalid")
	}
	if _, ok := Consume(path); ok {
		t.Fatal("consuming an expired token should return false")
	}
}
