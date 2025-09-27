package userdb

import "testing"

func TestComputeHA1NormalisesPlaintext(t *testing.T) {
	hash := ComputeHA1("alice", "example.com", "secret")
	expected := HashPassword("alice", "example.com", "secret")
	if hash != expected {
		t.Fatalf("expected %q, got %q", expected, hash)
	}
}

func TestComputeHA1KeepsDigest(t *testing.T) {
	stored := "0123456789abcdef0123456789ABCDEF"
	hash := ComputeHA1("alice", "example.com", stored)
	if hash != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("unexpected normalised hash: %s", hash)
	}
}

func TestVerifyPassword(t *testing.T) {
	stored := HashPassword("alice", "example.com", "secret")
	if !VerifyPassword(stored, "alice", "example.com", "secret") {
		t.Fatalf("expected password to match")
	}
	if VerifyPassword(stored, "alice", "example.com", "other") {
		t.Fatalf("password should not match")
	}
	if !VerifyPassword("", "alice", "example.com", "") {
		t.Fatalf("empty password should match empty stored value")
	}
}
