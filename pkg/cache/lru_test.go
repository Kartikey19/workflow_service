package cache

import "testing"

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewLRU[string, int](2)
	c.Add("a", 1)
	c.Add("b", 2)
	if _, ok := c.Get("a"); !ok { // a becomes most recent
		t.Fatal("a missing")
	}
	c.Add("c", 3) // evicts b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("a = %v, %v", v, ok)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d", c.Len())
	}
}

func TestLRUAddReplacesValue(t *testing.T) {
	c := NewLRU[[2]byte, *string](1)
	first, second := "x", "y"
	key := [2]byte{1, 2}
	c.Add(key, &first)
	c.Add(key, &second)
	if v, _ := c.Get(key); v != &second || c.Len() != 1 {
		t.Fatalf("value not replaced, len %d", c.Len())
	}
}
