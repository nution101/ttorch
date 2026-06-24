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

func TestData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.approve")
	if _, ok := Data(path); ok {
		t.Fatal("no token yet, Data should be false")
	}
	if err := Grant(path, time.Minute, "auto sha-abc"); err != nil {
		t.Fatal(err)
	}
	got, ok := Data(path)
	if !ok || got != "auto sha-abc" {
		t.Fatalf("Data = %q ok=%v, want the bound data", got, ok)
	}
	// Data does not consume.
	if !Valid(path) {
		t.Fatal("Data must not consume the token")
	}
	// An expired token yields no data.
	exp := filepath.Join(t.TempDir(), "t2.approve")
	if err := Grant(exp, -time.Second, "auto sha-xyz"); err != nil {
		t.Fatal(err)
	}
	if _, ok := Data(exp); ok {
		t.Fatal("an expired token must not return Data")
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
