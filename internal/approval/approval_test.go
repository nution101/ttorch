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

func TestRepin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.approve")

	// Re-pinning an absent token does nothing and reports not-moved (the carry then fails closed).
	if moved, err := Repin(path, "human sha-new"); err != nil || moved {
		t.Fatalf("Repin of an absent token: moved=%v err=%v, want false/nil", moved, err)
	}
	if Valid(path) {
		t.Fatal("Repin must not create a token where none existed")
	}

	// Grant a token, then re-pin it to new data: the data changes, the token stays valid, and
	// the ORIGINAL absolute expiry is preserved (a carry-forward never extends the grant).
	if err := Grant(path, time.Hour, "human sha-old"); err != nil {
		t.Fatal(err)
	}
	expBefore, _, _ := read(path)
	moved, err := Repin(path, "human sha-new")
	if err != nil || !moved {
		t.Fatalf("Repin of a valid token: moved=%v err=%v, want true/nil", moved, err)
	}
	data, ok := Data(path)
	if !ok || data != "human sha-new" {
		t.Fatalf("after Repin Data=%q ok=%v, want human sha-new/true", data, ok)
	}
	if expAfter, _, _ := read(path); expAfter != expBefore {
		t.Fatalf("Repin must preserve the original expiry: before=%d after=%d", expBefore, expAfter)
	}

	// Re-pinning an EXPIRED token does nothing and reports not-moved.
	exp := filepath.Join(t.TempDir(), "t2.approve")
	if err := Grant(exp, -time.Second, "auto sha-x"); err != nil {
		t.Fatal(err)
	}
	if moved, err := Repin(exp, "auto sha-y"); err != nil || moved {
		t.Fatalf("Repin of an expired token: moved=%v err=%v, want false/nil", moved, err)
	}
	if d, _ := Data(exp); d != "" {
		t.Fatal("an expired token must not be rebound")
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
