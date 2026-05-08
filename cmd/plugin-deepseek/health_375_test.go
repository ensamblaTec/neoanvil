package main

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
)

func TestCacheColdAdvisory_Cold(t *testing.T) {
	s := &state{}
	resp := &deepseek.CallResponse{CacheHitTokens: 500, CacheMissTokens: 3000}
	adv := s.cacheColdAdvisory(resp)
	if !strings.Contains(adv, "CACHE_COLD") {
		t.Errorf("expected CACHE_COLD advisory for 500 hit / 3000 miss, got %q", adv)
	}
}

func TestCacheColdAdvisory_Warm(t *testing.T) {
	s := &state{}
	resp := &deepseek.CallResponse{CacheHitTokens: 5000, CacheMissTokens: 100}
	adv := s.cacheColdAdvisory(resp)
	if adv != "" {
		t.Errorf("expected no advisory for warm cache (5000 hit), got %q", adv)
	}
}

func TestCacheColdAdvisory_Nil(t *testing.T) {
	s := &state{}
	if adv := s.cacheColdAdvisory(nil); adv != "" {
		t.Errorf("expected empty for nil response, got %q", adv)
	}
}

func TestHealthExtended_CacheHitRatio(t *testing.T) {
	s := &state{apiKey: "test"}
	atomic.StoreInt64(&s.startedAtUnix, 1000)
	atomic.StoreInt64(&s.cacheHitTokens, 8000)
	atomic.StoreInt64(&s.cacheMissTokens, 2000)
	atomic.StoreInt64(&s.threadCount, 3)

	resp := s.handleHealth(1)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatal("expected result map")
	}
	ratio, _ := result["cache_hit_ratio"].(float64)
	if ratio < 0.79 || ratio > 0.81 {
		t.Errorf("expected ~0.80 ratio (8000/10000), got %f", ratio)
	}
	tc, _ := result["thread_count"].(int64)
	if tc != 3 {
		t.Errorf("expected thread_count=3, got %d", tc)
	}
}
