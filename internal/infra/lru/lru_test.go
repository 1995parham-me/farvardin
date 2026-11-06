package lru_test

import (
	"testing"

	"github.com/1995parham-me/farvardin/internal/infra/lru"
)

func TestEvictsLeastRecentlyUsedByCost(t *testing.T) {
	t.Parallel()

	c := lru.New[string, string](10, func(v string) int { return len(v) })
	c.Put("a", "aaaa")
	c.Put("b", "bbbb")
	c.Get("a")
	c.Put("c", "cccc") // 12 > 10: b, the least recently used, goes

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}

	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was used recently and must stay")
	}

	c.Put("huge", "xxxxxxxxxxxx")

	if _, ok := c.Get("huge"); ok || c.Len() != 2 {
		t.Fatal("a value over the whole budget must not be stored")
	}
}
