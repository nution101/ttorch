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
	if err := Grant(path, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !Valid(path) {
		t.Fatal("freshly granted token should be valid")
	}
	if !Consume(path) {
		t.Fatal("consume of a valid token should succeed")
	}
	if Valid(path) {
		t.Fatal("token should be gone after consume")
	}
	if Consume(path) {
		t.Fatal("second consume should fail")
	}
}

func TestExpiredToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.approve")
	if err := Grant(path, -time.Second); err != nil {
		t.Fatal(err)
	}
	if Valid(path) {
		t.Fatal("expired token should be invalid")
	}
	if Consume(path) {
		t.Fatal("consuming an expired token should return false")
	}
}
