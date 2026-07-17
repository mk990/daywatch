package web

import "testing"

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	ip := "203.0.113.9"

	for i := range loginMaxFailures {
		if l.blocked(ip) {
			t.Fatalf("blocked after %d failures", i)
		}
		l.recordFailure(ip)
	}
	if !l.blocked(ip) {
		t.Fatal("not blocked after max failures")
	}
	if l.blocked("198.51.100.1") {
		t.Fatal("unrelated IP blocked")
	}

	// A successful login clears the counter.
	l.reset(ip)
	if l.blocked(ip) {
		t.Fatal("still blocked after reset")
	}
}
